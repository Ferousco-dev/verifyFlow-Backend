package wallet

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalidRequest      = errors.New("wallet: invalid request")
	ErrInsufficientFunds   = errors.New("wallet: insufficient funds")
	ErrIdempotencyConflict = errors.New("wallet: idempotency key conflict")
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

type Wallet struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"user_id"`
	Currency            string    `json:"currency"`
	AvailableMinorUnits int64     `json:"available_minor_units"`
	PendingMinorUnits   int64     `json:"pending_minor_units"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type Transaction struct {
	ID               string    `json:"id"`
	WalletID         string    `json:"wallet_id"`
	UserID           string    `json:"user_id"`
	Type             string    `json:"type"`
	Currency         string    `json:"currency"`
	Reference        string    `json:"reference"`
	Status           string    `json:"status"`
	Reason           string    `json:"reason,omitempty"`
	AmountMinorUnits int64     `json:"amount_minor_units"`
	ActorUserID      *string   `json:"actor_user_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type Adjustment struct {
	ActorUserID      string `json:"-"`
	UserID           string `json:"user_id"`
	Currency         string `json:"currency"`
	Type             string `json:"type"`
	Reference        string `json:"reference"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
	Reason           string `json:"reason"`
	AmountMinorUnits int64  `json:"amount_minor_units"`
}

// PurchaseDebit debits a customer's wallet for a number order at reservation
// time. Reference is the order ID; IdempotencyKey is shared with the order's
// own creation so retries can never double-charge.
type PurchaseDebit struct {
	UserID           string
	Currency         string
	Reference        string
	IdempotencyKey   string
	AmountMinorUnits int64
}

// RefundInput reverses a prior PurchaseDebit with a new, append-only ledger
// entry (never by editing the original row). OriginalIdempotencyKey ties the
// refund back to the debit it compensates and makes the refund itself
// idempotent: retrying it is a no-op.
type RefundInput struct {
	UserID                 string
	Currency               string
	Reference              string
	OriginalIdempotencyKey string
	AmountMinorUnits       int64
	Reason                 string
}

type Store interface {
	GetOrCreate(context.Context, string, string) (Wallet, error)
	ListTransactions(context.Context, string, string, int) ([]Transaction, error)
	Adjust(context.Context, Adjustment) (Transaction, error)
	Purchase(context.Context, PurchaseDebit) (Transaction, error)
	RefundPurchase(context.Context, RefundInput) (Transaction, error)
	Renew(context.Context, PurchaseDebit) (Transaction, error)
}

type Service struct{ store Store }

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalidRequest)
	}
	return &Service{store: store}, nil
}

func (s *Service) Balance(ctx context.Context, userID, currency string) (Wallet, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if userID == "" || !currencyPattern.MatchString(currency) {
		return Wallet{}, ErrInvalidRequest
	}
	return s.store.GetOrCreate(ctx, userID, currency)
}

func (s *Service) Transactions(ctx context.Context, userID, currency string, limit int) ([]Transaction, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if userID == "" || !currencyPattern.MatchString(currency) || limit < 0 || limit > 100 {
		return nil, ErrInvalidRequest
	}
	if limit == 0 {
		limit = 50
	}
	return s.store.ListTransactions(ctx, userID, currency, limit)
}

func (s *Service) Adjust(ctx context.Context, in Adjustment) (Transaction, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Reason = strings.TrimSpace(in.Reason)
	in.Reference = strings.TrimSpace(in.Reference)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ActorUserID == "" || in.UserID == "" || !currencyPattern.MatchString(in.Currency) || in.AmountMinorUnits <= 0 || (in.Type != "adjustment_credit" && in.Type != "adjustment_debit") || in.Reference == "" || in.IdempotencyKey == "" || in.Reason == "" || len(in.Reason) > 500 {
		return Transaction{}, ErrInvalidRequest
	}
	return s.store.Adjust(ctx, in)
}

func (s *Service) Purchase(ctx context.Context, in PurchaseDebit) (Transaction, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Reference = strings.TrimSpace(in.Reference)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.UserID == "" || !currencyPattern.MatchString(in.Currency) || in.AmountMinorUnits <= 0 || in.Reference == "" || in.IdempotencyKey == "" {
		return Transaction{}, ErrInvalidRequest
	}
	return s.store.Purchase(ctx, in)
}

// Renew debits a customer's wallet for the next billing period of an
// already-active rental. It shares PurchaseDebit's shape and validation
// because it's the same operation — a priced debit against a reference,
// made idempotent by IdempotencyKey — just posted to the ledger as
// "renewal" instead of "purchase" so the two are distinguishable in a
// customer's transaction history and in reporting.
func (s *Service) Renew(ctx context.Context, in PurchaseDebit) (Transaction, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Reference = strings.TrimSpace(in.Reference)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.UserID == "" || !currencyPattern.MatchString(in.Currency) || in.AmountMinorUnits <= 0 || in.Reference == "" || in.IdempotencyKey == "" {
		return Transaction{}, ErrInvalidRequest
	}
	return s.store.Renew(ctx, in)
}

func (s *Service) RefundPurchase(ctx context.Context, in RefundInput) (Transaction, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Reference = strings.TrimSpace(in.Reference)
	in.OriginalIdempotencyKey = strings.TrimSpace(in.OriginalIdempotencyKey)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.UserID == "" || !currencyPattern.MatchString(in.Currency) || in.AmountMinorUnits <= 0 || in.Reference == "" || in.OriginalIdempotencyKey == "" {
		return Transaction{}, ErrInvalidRequest
	}
	if in.Reason == "" {
		in.Reason = "compensating refund"
	}
	return s.store.RefundPurchase(ctx, in)
}
