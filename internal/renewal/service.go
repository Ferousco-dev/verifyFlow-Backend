// Package renewal re-bills active rentals for each new monthly (or
// whatever the plan's duration is) billing period, so Migo never keeps
// paying a telephony provider to hold a number that nobody is paying Migo
// for. A rental is charged again from the same customer wallet that paid
// for it originally; if that debit fails (most commonly insufficient
// funds), the rental is released rather than silently renewed at Migo's
// expense.
package renewal

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"migo/internal/mailer"
	"migo/internal/rental"
	"migo/internal/wallet"
)

var ErrInvalidRequest = errors.New("renewal: invalid request")

// Rentals is satisfied by *rental.Service.
type Rentals interface {
	ListDueRenewals(ctx context.Context, limit int) ([]rental.DueRenewal, error)
	RenewRental(ctx context.Context, rentalID string, expectedRenewalCount int32, newExpiresAt time.Time) (bool, error)
	ExpireActiveRental(ctx context.Context, rentalID string, expectedRenewalCount int32) (bool, error)
}

// Wallets is satisfied by *wallet.Service.
type Wallets interface {
	Renew(ctx context.Context, in wallet.PurchaseDebit) (wallet.Transaction, error)
}

type Service struct {
	rentals   Rentals
	wallets   Wallets
	mailer    mailer.Sender // optional: nil disables renewal-failed emails
	topUpURL  string
	batchSize int
}

func NewService(rentals Rentals, wallets Wallets) (*Service, error) {
	if rentals == nil || wallets == nil {
		return nil, fmt.Errorf("%w: rentals and wallets are required", ErrInvalidRequest)
	}
	return &Service{rentals: rentals, wallets: wallets, batchSize: 200}, nil
}

// EnableFailureEmails turns on the "your number could not be renewed"
// notification sent the moment a renewal debit fails. topUpURL is the
// frontend page the email's button points to.
func (s *Service) EnableFailureEmails(sender mailer.Sender, topUpURL string) {
	s.mailer = sender
	s.topUpURL = topUpURL
}

// Result tallies one RunDue pass, for logging and for the admin trigger
// endpoint's response.
type Result struct {
	Renewed int
	Expired int
	Failed  int
}

// RunDue attempts to renew every rental whose current billing period has
// ended: debit the customer's wallet for another period at the price they
// originally agreed to, and extend the rental on success. A debit that
// fails (insufficient funds, most commonly) expires the rental instead —
// Migo stops paying the provider for it rather than eating the cost.
//
// Each rental's renewal or expiry uses an idempotency key derived from its
// RenewalCount, so re-running RunDue (a retried cron tick, an overlapping
// run) never double-charges or double-releases the same billing period.
func (s *Service) RunDue(ctx context.Context) (Result, error) {
	due, err := s.rentals.ListDueRenewals(ctx, s.batchSize)
	if err != nil {
		return Result{}, err
	}

	var result Result
	for _, r := range due {
		idempotencyKey := r.RentalID + ":renewal:" + strconv.Itoa(int(r.RenewalCount))
		_, debitErr := s.wallets.Renew(ctx, wallet.PurchaseDebit{
			UserID:           r.UserID,
			Currency:         r.Currency,
			Reference:        r.OrderID,
			IdempotencyKey:   idempotencyKey,
			AmountMinorUnits: r.PriceMinorUnits,
		})
		if debitErr != nil && !errors.Is(debitErr, wallet.ErrIdempotencyConflict) {
			ok, expireErr := s.rentals.ExpireActiveRental(ctx, r.RentalID, r.RenewalCount)
			if expireErr != nil {
				result.Failed++
				continue
			}
			if ok {
				result.Expired++
				if s.mailer != nil {
					// Best-effort: a failed notification must never turn a
					// successful release back into a "Failed" tally entry.
					_ = s.mailer.Send(ctx, mailer.RenewalFailedEmail(mailer.DefaultBrand, r.UserEmail, r.UserFullName, r.PhoneNumber, s.topUpURL))
				}
			}
			continue
		}

		newExpiresAt := r.ExpiresAt.Add(time.Duration(r.DurationSeconds) * time.Second)
		ok, renewErr := s.rentals.RenewRental(ctx, r.RentalID, r.RenewalCount, newExpiresAt)
		if renewErr != nil {
			result.Failed++
			continue
		}
		if ok {
			result.Renewed++
		}
	}
	return result, nil
}
