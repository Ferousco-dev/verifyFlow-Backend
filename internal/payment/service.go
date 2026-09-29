// Package payment drives Paystack checkout: initializing a transaction,
// server-side verification, and authenticated/idempotent webhook processing.
// A webhook is only ever treated as a signal to re-verify with Paystack
// directly — never as proof of payment on its own.
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"migo/internal/fulfillment"
	"migo/internal/paystack"
)

var (
	ErrInvalidRequest          = errors.New("payment: invalid request")
	ErrOrderNotFound           = errors.New("payment: order not found")
	ErrOrderNotPending         = errors.New("payment: order is not pending")
	ErrAlreadyPaid             = errors.New("payment: order already has a successful payment")
	ErrAmountMismatch          = errors.New("payment: verified amount or currency does not match the order")
	ErrProviderConfigMissing   = errors.New("payment: no enabled Paystack provider config")
	ErrNotFound                = errors.New("payment: payment attempt not found")
	ErrInvalidWebhookSignature = errors.New("payment: invalid webhook signature")
)

type Attempt struct {
	ID                string
	OrderID           string
	ProviderConfigID  string
	ProviderReference string
	ProviderStatus    string
	AmountMinorUnits  int64
	Currency          string
	CreatedAt         time.Time
}

// OrderSnapshot is the minimal order view this package needs; it never
// touches order pricing or ownership logic itself.
type OrderSnapshot struct {
	ID              string
	UserID          string
	Status          string
	PriceMinorUnits int64
	Currency        string
}

// Orders is satisfied by an adapter over rental.Service.
type Orders interface {
	// GetOrder enforces that orderID belongs to userID.
	GetOrder(ctx context.Context, userID, orderID string) (OrderSnapshot, error)
	// GetOrderByID is for the webhook path, which has no authenticated user
	// context; it is never exposed directly over HTTP.
	GetOrderByID(ctx context.Context, orderID string) (OrderSnapshot, error)
}

// UserEmails is satisfied by an adapter over auth.Service.
type UserEmails interface {
	Email(ctx context.Context, userID string) (string, error)
}

// Fulfiller is satisfied by *fulfillment.Service. It is invoked once an
// attempt reconciles to "success", so provisioning happens right when
// payment is confirmed rather than needing a separate poller.
type Fulfiller interface {
	Fulfill(ctx context.Context, orderID string) error
}

// Resolver returns the currently enabled Paystack client and the
// provider_configs row backing it. Resolved per call (not cached) so an
// admin rotating or disabling the config takes effect immediately.
type Resolver interface {
	Resolve(ctx context.Context) (*paystack.Client, string, error)
}

type WalletFunder interface {
	HandlePaystackReference(context.Context, *paystack.Client, string, string) (bool, error)
}

type Store interface {
	CreateAttempt(ctx context.Context, orderID, providerConfigID string, amountMinorUnits int64, currency string) (Attempt, error)
	SetReference(ctx context.Context, id, reference, status string) error
	UpdateStatus(ctx context.Context, id, status string) error
	GetLatestForOrder(ctx context.Context, orderID string) (Attempt, error)
	GetByReference(ctx context.Context, providerConfigID, reference string) (Attempt, error)
	HasSuccessfulPayment(ctx context.Context, orderID string) (bool, error)
	RecordWebhookEvent(ctx context.Context, providerConfigID, idempotencyKey, providerReference, eventType string, payload []byte) (isNew bool, err error)
	MarkWebhookProcessed(ctx context.Context, providerConfigID, idempotencyKey string) error
}

type Service struct {
	store        Store
	orders       Orders
	emails       UserEmails
	resolver     Resolver
	fulfiller    Fulfiller
	callbackURL  string
	walletFunder WalletFunder
}

func (s *Service) EnableWalletFunding(funder WalletFunder) { s.walletFunder = funder }

func NewService(store Store, orders Orders, emails UserEmails, resolver Resolver, fulfiller Fulfiller, callbackURL string) (*Service, error) {
	if store == nil || orders == nil || emails == nil || resolver == nil || fulfiller == nil {
		return nil, fmt.Errorf("%w: store, orders, emails, resolver and fulfiller are required", ErrInvalidRequest)
	}
	return &Service{store: store, orders: orders, emails: emails, resolver: resolver, fulfiller: fulfiller, callbackURL: callbackURL}, nil
}

type InitializeResult struct {
	AuthorizationURL string
	Reference        string
}

// Initialize starts a new Paystack checkout for orderID, using the order's
// immutable price/currency snapshot as the expected payment amount.
func (s *Service) Initialize(ctx context.Context, userID, orderID string) (InitializeResult, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(orderID) == "" {
		return InitializeResult{}, ErrInvalidRequest
	}
	order, err := s.orders.GetOrder(ctx, userID, orderID)
	if err != nil {
		return InitializeResult{}, err
	}
	if order.Status != "PENDING" {
		return InitializeResult{}, ErrOrderNotPending
	}
	hasSuccess, err := s.store.HasSuccessfulPayment(ctx, orderID)
	if err != nil {
		return InitializeResult{}, err
	}
	if hasSuccess {
		return InitializeResult{}, ErrAlreadyPaid
	}

	client, providerConfigID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return InitializeResult{}, err
	}
	email, err := s.emails.Email(ctx, userID)
	if err != nil {
		return InitializeResult{}, err
	}
	attempt, err := s.store.CreateAttempt(ctx, orderID, providerConfigID, order.PriceMinorUnits, order.Currency)
	if err != nil {
		return InitializeResult{}, err
	}

	result, err := client.Initialize(ctx, paystack.InitializeRequest{
		Email: email, AmountMinorUnits: order.PriceMinorUnits, Currency: order.Currency,
		Reference: attempt.ID, CallbackURL: s.callbackURL,
	})
	if err != nil {
		_ = s.store.UpdateStatus(ctx, attempt.ID, "failed")
		return InitializeResult{}, err
	}
	if err := s.store.SetReference(ctx, attempt.ID, result.Reference, "pending"); err != nil {
		return InitializeResult{}, err
	}
	return InitializeResult{AuthorizationURL: result.AuthorizationURL, Reference: result.Reference}, nil
}

// Verify re-checks the latest payment attempt for orderID directly against
// Paystack. It is safe to call repeatedly (e.g. while polling after
// checkout redirect).
func (s *Service) Verify(ctx context.Context, userID, orderID string) (Attempt, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(orderID) == "" {
		return Attempt{}, ErrInvalidRequest
	}
	order, err := s.orders.GetOrder(ctx, userID, orderID)
	if err != nil {
		return Attempt{}, err
	}
	attempt, err := s.store.GetLatestForOrder(ctx, orderID)
	if err != nil {
		return Attempt{}, err
	}
	if attempt.ProviderStatus == "success" {
		return attempt, nil
	}
	client, _, err := s.resolver.Resolve(ctx)
	if err != nil {
		return Attempt{}, err
	}
	return s.reconcile(ctx, client, order.PriceMinorUnits, order.Currency, attempt)
}

// reconcile calls Paystack Verify for attempt and persists the outcome. An
// attempt that reports Paystack "success" but with an amount/currency that
// does not match the order's immutable snapshot is recorded as
// "amount_mismatch", never as "success".
func (s *Service) reconcile(ctx context.Context, client *paystack.Client, expectedAmount int64, expectedCurrency string, attempt Attempt) (Attempt, error) {
	if attempt.ProviderReference == "" {
		return attempt, nil
	}
	result, err := client.Verify(ctx, attempt.ProviderReference)
	if err != nil {
		return Attempt{}, err
	}
	status := result.Status
	if status == "success" && (result.AmountMinorUnits != expectedAmount || !strings.EqualFold(result.Currency, expectedCurrency)) {
		status = "amount_mismatch"
	}
	if status == "success" {
		// A different attempt for the same order (e.g. two checkout tabs)
		// may have already been recorded successful; only one attempt per
		// order ever is (payments_one_success_per_order_key). Leave this one
		// as "superseded" rather than racing that constraint.
		hasSuccess, err := s.store.HasSuccessfulPayment(ctx, attempt.OrderID)
		if err != nil {
			return Attempt{}, err
		}
		if hasSuccess {
			status = "superseded"
		}
	}
	if err := s.store.UpdateStatus(ctx, attempt.ID, status); err != nil {
		return Attempt{}, err
	}
	attempt.ProviderStatus = status
	if status == "amount_mismatch" {
		return attempt, ErrAmountMismatch
	}
	if status == "success" {
		if err := s.fulfiller.Fulfill(ctx, attempt.OrderID); err != nil && !errors.Is(err, fulfillment.ErrPendingFulfillment) {
			return attempt, err
		}
	}
	return attempt, nil
}

// HandleWebhookEvent processes a Paystack webhook delivery. The signature is
// verified before anything else; the event is then recorded idempotently by
// (provider_config_id, idempotency_key) so redeliveries are no-ops. A
// charge.success event triggers a direct Paystack re-verification of the
// matching payment attempt — the webhook body's own amount/status are never
// trusted as proof of payment.
func (s *Service) HandleWebhookEvent(ctx context.Context, body []byte, signatureHeader string) error {
	client, providerConfigID, err := s.resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	if !client.VerifySignature(body, signatureHeader) {
		return ErrInvalidWebhookSignature
	}

	var event struct {
		Event string `json:"event"`
		Data  struct {
			ID        int64  `json:"id"`
			Reference string `json:"reference"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil || strings.TrimSpace(event.Data.Reference) == "" {
		return fmt.Errorf("%w: malformed webhook payload", ErrInvalidRequest)
	}
	idempotencyKey := event.Event + ":" + strconv.FormatInt(event.Data.ID, 10)

	isNew, err := s.store.RecordWebhookEvent(ctx, providerConfigID, idempotencyKey, event.Data.Reference, event.Event, body)
	if err != nil {
		return err
	}
	if !isNew || event.Event != "charge.success" {
		return s.store.MarkWebhookProcessed(ctx, providerConfigID, idempotencyKey)
	}

	attempt, err := s.store.GetByReference(ctx, providerConfigID, event.Data.Reference)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			if s.walletFunder != nil {
				if _, walletErr := s.walletFunder.HandlePaystackReference(ctx, client, providerConfigID, event.Data.Reference); walletErr != nil {
					return walletErr
				}
			}
			return s.store.MarkWebhookProcessed(ctx, providerConfigID, idempotencyKey)
		}
		return err
	}
	if attempt.ProviderStatus != "success" {
		order, err := s.orders.GetOrderByID(ctx, attempt.OrderID)
		if err != nil {
			return err
		}
		if _, err := s.reconcile(ctx, client, order.PriceMinorUnits, order.Currency, attempt); err != nil && !errors.Is(err, ErrAmountMismatch) {
			return err
		}
	}
	return s.store.MarkWebhookProcessed(ctx, providerConfigID, idempotencyKey)
}
