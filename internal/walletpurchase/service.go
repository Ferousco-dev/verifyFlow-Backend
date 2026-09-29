// Package walletpurchase orchestrates a wallet-backed number purchase:
// reserve inventory, debit the customer's wallet for the order's price, then
// provision the number. A fulfillment failure that isn't the expected
// "held for operator" outcome is reversed with a compensating refund, so the
// ledger only ever appends new entries — it never edits history.
package walletpurchase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"migo/internal/fulfillment"
	"migo/internal/mailer"
	"migo/internal/rental"
	"migo/internal/wallet"
)

var ErrInvalidRequest = errors.New("walletpurchase: invalid request")

// Rentals is satisfied by *rental.Service.
type Rentals interface {
	Reserve(ctx context.Context, authenticatedUserID string, request rental.ReserveRequest) (rental.Reservation, error)
}

// Wallets is satisfied by *wallet.Service.
type Wallets interface {
	Purchase(ctx context.Context, in wallet.PurchaseDebit) (wallet.Transaction, error)
	RefundPurchase(ctx context.Context, in wallet.RefundInput) (wallet.Transaction, error)
}

// Fulfiller is satisfied by *fulfillment.Service.
type Fulfiller interface {
	Fulfill(ctx context.Context, orderID string) error
}

// Profiles resolves a user's email and display name, for the purchase
// receipt email. Satisfied by payment.AuthUserEmails.
type Profiles interface {
	Email(ctx context.Context, userID string) (string, error)
	FullName(ctx context.Context, userID string) (string, error)
}

type Service struct {
	rentals   Rentals
	wallets   Wallets
	fulfiller Fulfiller
	mailer    mailer.Sender // optional: nil disables purchase receipt emails
	profiles  Profiles      // optional: required alongside mailer for receipts
}

func NewService(rentals Rentals, wallets Wallets, fulfiller Fulfiller) (*Service, error) {
	if rentals == nil || wallets == nil || fulfiller == nil {
		return nil, fmt.Errorf("%w: rentals, wallets and fulfiller are required", ErrInvalidRequest)
	}
	return &Service{rentals: rentals, wallets: wallets, fulfiller: fulfiller}, nil
}

// EnableReceipts turns on the "number purchase receipt" email sent right
// after a purchase successfully fulfills.
func (s *Service) EnableReceipts(sender mailer.Sender, profiles Profiles) {
	s.mailer = sender
	s.profiles = profiles
}

type PurchaseRequest struct {
	ProviderNumberID string
	RentalPlanID     string
	IdempotencyKey   string
}

// Purchase reserves a number, debits the customer's wallet for its price,
// and fulfills the order. Reserve and Purchase share one idempotency key,
// so retrying the same request after a partial failure never double-charges
// or double-reserves.
//
// If the debit finds insufficient funds, the reservation is left in place to
// expire on its own TTL; nothing else needs to be undone. If fulfillment
// fails for any reason other than fulfillment.ErrPendingFulfillment (which
// means the order is intentionally held for operator attention, not
// abandoned), the debit is reversed with a compensating refund before the
// error is returned.
func (s *Service) Purchase(ctx context.Context, userID string, req PurchaseRequest) (rental.Order, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(req.IdempotencyKey) == "" {
		return rental.Order{}, ErrInvalidRequest
	}

	reservation, err := s.rentals.Reserve(ctx, userID, rental.ReserveRequest{
		ProviderNumberID: req.ProviderNumberID,
		RentalPlanID:     req.RentalPlanID,
		IdempotencyKey:   req.IdempotencyKey,
	})
	if err != nil {
		return rental.Order{}, err
	}
	order := reservation.Order

	if _, err := s.wallets.Purchase(ctx, wallet.PurchaseDebit{
		UserID:           userID,
		Currency:         order.Currency,
		Reference:        order.ID,
		IdempotencyKey:   req.IdempotencyKey,
		AmountMinorUnits: order.PriceMinorUnits,
	}); err != nil && !errors.Is(err, wallet.ErrIdempotencyConflict) {
		return rental.Order{}, err
	}

	if err := s.fulfiller.Fulfill(ctx, order.ID); err != nil {
		if errors.Is(err, fulfillment.ErrPendingFulfillment) {
			return order, err
		}
		if _, refundErr := s.wallets.RefundPurchase(ctx, wallet.RefundInput{
			UserID:                 userID,
			Currency:               order.Currency,
			Reference:              order.ID,
			OriginalIdempotencyKey: req.IdempotencyKey,
			AmountMinorUnits:       order.PriceMinorUnits,
			Reason:                 "compensating refund: number fulfillment failed",
		}); refundErr != nil && !errors.Is(refundErr, wallet.ErrIdempotencyConflict) {
			return rental.Order{}, fmt.Errorf("fulfillment failed (%w) and compensating refund also failed: %v", err, refundErr)
		}
		return rental.Order{}, err
	}
	s.sendReceipt(ctx, userID, order)
	return order, nil
}

// sendReceipt is best-effort: a failed notification must never turn a
// completed, fulfilled purchase into an error response.
func (s *Service) sendReceipt(ctx context.Context, userID string, order rental.Order) {
	if s.mailer == nil || s.profiles == nil {
		return
	}
	email, err := s.profiles.Email(ctx, userID)
	if err != nil {
		return
	}
	fullName, err := s.profiles.FullName(ctx, userID)
	if err != nil {
		return
	}
	msg := mailer.ReceiptEmail(mailer.DefaultBrand, email, fullName, "Your number purchase was successful", order.ID,
		[]mailer.ReceiptLine{{Label: order.PlanName, AmountMinorUnits: order.PriceMinorUnits}},
		order.PriceMinorUnits, order.Currency, time.Now(), nil)
	_ = s.mailer.Send(ctx, msg)
}
