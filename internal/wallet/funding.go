package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"migo/internal/mailer"
	"migo/internal/paystack"
)

var (
	ErrFundingNotFound     = errors.New("wallet: funding attempt not found")
	ErrAmountMismatch      = errors.New("wallet: verified funding amount mismatch")
	ErrProviderUnavailable = errors.New("wallet: payment provider unavailable")
)

type FundingAttempt struct {
	ID, UserID, WalletID, ProviderConfigID, ProviderReference, Status, Currency string
	AmountMinorUnits                                                            int64
	CreatedAt                                                                   time.Time
}
type FundingResult struct {
	Attempt          FundingAttempt
	AuthorizationURL string
}

type FundingStore interface {
	CreateFundingAttempt(context.Context, string, string, string, int64, string, string) (FundingAttempt, error)
	SetFundingReference(context.Context, string, string, string) error
	GetFundingAttempt(context.Context, string, string) (FundingAttempt, error)
	GetFundingByReference(context.Context, string, string) (FundingAttempt, error)
	SettleFunding(context.Context, FundingAttempt, string) (FundingAttempt, error)
	UpdateFundingStatus(context.Context, string, string) error
}
type PaystackResolver interface {
	Resolve(context.Context) (*paystack.Client, string, error)
}
type EmailLookup interface {
	Email(context.Context, string) (string, error)
}

// UserProfile is EmailLookup plus a display name, for anything (like a
// receipt) that needs to greet the customer by name. Satisfied by the same
// adapter that satisfies EmailLookup.
type UserProfile interface {
	EmailLookup
	FullName(context.Context, string) (string, error)
}

type FundingService struct {
	wallets     *Service
	store       FundingStore
	resolver    PaystackResolver
	emails      EmailLookup
	callbackURL string
	mailer      mailer.Sender // optional: nil disables top-up receipt emails
	profiles    UserProfile   // optional: required alongside mailer for receipts
}

func NewFundingService(wallets *Service, store FundingStore, resolver PaystackResolver, emails EmailLookup, callbackURL string) (*FundingService, error) {
	if wallets == nil || store == nil || resolver == nil || emails == nil {
		return nil, fmt.Errorf("%w: funding dependencies are required", ErrInvalidRequest)
	}
	return &FundingService{wallets: wallets, store: store, resolver: resolver, emails: emails, callbackURL: callbackURL}, nil
}

// EnableReceipts turns on the "wallet top-up receipt" email sent right
// after a deposit is verified and credited.
func (s *FundingService) EnableReceipts(sender mailer.Sender, profiles UserProfile) {
	s.mailer = sender
	s.profiles = profiles
}

func (s *FundingService) Initialize(ctx context.Context, userID, currency, idempotencyKey string, amount int64) (FundingResult, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if userID == "" || !currencyPattern.MatchString(currency) || amount <= 0 || amount > 100000000000 || idempotencyKey == "" || len(idempotencyKey) > 200 {
		return FundingResult{}, ErrInvalidRequest
	}
	w, err := s.wallets.Balance(ctx, userID, currency)
	if err != nil {
		return FundingResult{}, err
	}
	client, configID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return FundingResult{}, ErrProviderUnavailable
	}
	email, err := s.emails.Email(ctx, userID)
	if err != nil {
		return FundingResult{}, err
	}
	attempt, err := s.store.CreateFundingAttempt(ctx, w.ID, userID, configID, amount, currency, idempotencyKey)
	if err != nil {
		return FundingResult{}, err
	}
	result, err := client.Initialize(ctx, paystack.InitializeRequest{Email: email, AmountMinorUnits: amount, Currency: currency, Reference: attempt.ID, CallbackURL: s.callbackURL})
	if err != nil {
		_ = s.store.UpdateFundingStatus(ctx, attempt.ID, "failed")
		return FundingResult{}, err
	}
	if err := s.store.SetFundingReference(ctx, attempt.ID, result.Reference, "pending"); err != nil {
		return FundingResult{}, err
	}
	attempt.ProviderReference = result.Reference
	attempt.Status = "pending"
	return FundingResult{Attempt: attempt, AuthorizationURL: result.AuthorizationURL}, nil
}

func (s *FundingService) Verify(ctx context.Context, userID, attemptID string) (FundingAttempt, error) {
	attempt, err := s.store.GetFundingAttempt(ctx, userID, attemptID)
	if err != nil {
		return FundingAttempt{}, err
	}
	if attempt.Status == "success" {
		return attempt, nil
	}
	client, configID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return FundingAttempt{}, ErrProviderUnavailable
	}
	if configID != attempt.ProviderConfigID {
		return FundingAttempt{}, ErrProviderUnavailable
	}
	return s.reconcile(ctx, client, attempt)
}

func (s *FundingService) HandlePaystackReference(ctx context.Context, client *paystack.Client, providerConfigID, reference string) (bool, error) {
	attempt, err := s.store.GetFundingByReference(ctx, providerConfigID, reference)
	if errors.Is(err, ErrFundingNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if attempt.Status == "success" {
		return true, nil
	}
	_, err = s.reconcile(ctx, client, attempt)
	if errors.Is(err, ErrAmountMismatch) {
		return true, nil
	}
	return true, err
}

func (s *FundingService) reconcile(ctx context.Context, client *paystack.Client, attempt FundingAttempt) (FundingAttempt, error) {
	if attempt.ProviderReference == "" {
		return attempt, nil
	}
	result, err := client.Verify(ctx, attempt.ProviderReference)
	if err != nil {
		return FundingAttempt{}, err
	}
	status := result.Status
	if status == "success" && (result.AmountMinorUnits != attempt.AmountMinorUnits || !strings.EqualFold(result.Currency, attempt.Currency)) {
		status = "amount_mismatch"
	}
	if status == "success" {
		settled, err := s.store.SettleFunding(ctx, attempt, "funding:"+attempt.ID)
		if err == nil {
			s.sendReceipt(ctx, settled)
		}
		return settled, err
	}
	if err := s.store.UpdateFundingStatus(ctx, attempt.ID, status); err != nil {
		return FundingAttempt{}, err
	}
	attempt.Status = status
	if status == "amount_mismatch" {
		return attempt, ErrAmountMismatch
	}
	return attempt, nil
}

// sendReceipt is best-effort: a failed notification must never fail (or be
// retried as) the funding it's reporting on.
func (s *FundingService) sendReceipt(ctx context.Context, settled FundingAttempt) {
	if s.mailer == nil || s.profiles == nil {
		return
	}
	email, err := s.profiles.Email(ctx, settled.UserID)
	if err != nil {
		return
	}
	fullName, err := s.profiles.FullName(ctx, settled.UserID)
	if err != nil {
		return
	}
	msg := mailer.ReceiptEmail(mailer.DefaultBrand, email, fullName, "Your wallet top-up was successful", settled.ID,
		[]mailer.ReceiptLine{{Label: "Wallet top-up", AmountMinorUnits: settled.AmountMinorUnits}},
		settled.AmountMinorUnits, settled.Currency, time.Now(), nil)
	_ = s.mailer.Send(ctx, msg)
}
