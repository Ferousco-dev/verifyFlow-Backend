package rental

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidRequest        = errors.New("rental: invalid request")
	ErrNumberNotFound        = errors.New("rental: provider number not found")
	ErrNumberUnavailable     = errors.New("rental: provider number unavailable")
	ErrPlanUnavailable       = errors.New("rental: rental plan unavailable")
	ErrIdempotencyConflict   = errors.New("rental: idempotency key reused with different request")
	ErrNotFound              = errors.New("rental: order or rental not found")
	ErrReservationNotExpired = errors.New("rental: reservation is not expired")
	ErrNotReservation        = errors.New("rental: rental is not an unpaid reservation")
	ErrAlreadyActivated      = errors.New("rental: rental is already activated")
)

type Order struct {
	ID                   string
	UserID               string
	RentalID             string
	ProviderNumberID     string
	RentalPlanID         string
	Status               string
	PlanCode             string
	PlanName             string
	DurationSeconds      int32
	PriceMinorUnits      int64
	Currency             string
	ReservationExpiresAt time.Time
	CreatedAt            time.Time
}

type ReserveRequest struct {
	ProviderNumberID string
	RentalPlanID     string
	IdempotencyKey   string
}

type Reservation struct {
	Order Order
}

type ReserveInput struct {
	UserID               string
	ProviderNumberID     string
	RentalPlanID         string
	IdempotencyKey       string
	ReservedAt           time.Time
	ReservationExpiresAt time.Time
}

type NumberFilter struct {
	NumberType   string
	RequireSMS   bool
	RequireMMS   bool
	RequireVoice bool
	Cursor       string
	Limit        int
}

type NumberSummary struct {
	ID           string
	PhoneNumber  string
	NumberType   string
	SMSEnabled   bool
	MMSEnabled   bool
	VoiceEnabled bool
}

type NumbersPage struct {
	Numbers    []NumberSummary
	NextCursor string
}

type Plan struct {
	ID              string
	Code            string
	Name            string
	DurationSeconds int32
	PriceMinorUnits int64
	Currency        string
}

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 100
)

// FulfillmentSnapshot is everything provisioning needs to know about an
// order's currently-assigned number, without exposing the rental package's
// storage details.
type FulfillmentSnapshot struct {
	OrderID          string
	RentalID         string
	ProviderNumberID string
	ProviderConfigID string
	PhoneNumber      string
	NumberType       string
	SMSEnabled       bool
	MMSEnabled       bool
	VoiceEnabled     bool
	DurationSeconds  int32
	ActivatedAt      *time.Time
}

type Store interface {
	SearchNumbers(context.Context, NumberFilter) (NumbersPage, error)
	ListPlans(context.Context) ([]Plan, error)
	Reserve(context.Context, ReserveInput) (Reservation, error)
	GetOrder(context.Context, string, string) (Order, error)
	GetOrderByID(context.Context, string) (Order, error)
	ExpireReservation(context.Context, string, time.Time) (bool, error)
	GetFulfillmentSnapshot(context.Context, string) (FulfillmentSnapshot, error)
	SwapToReplacementNumber(ctx context.Context, orderID, failedNumberID, numberType string, sms, mms, voice bool, now time.Time) (newNumberID string, found bool, err error)
	ActivateRental(ctx context.Context, orderID, providerNumberID, providerReference string, expiresAt, now time.Time) error
}

type Service struct {
	store          Store
	reservationTTL time.Duration
	now            func() time.Time
}

func NewService(store Store, reservationTTL time.Duration) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalidRequest)
	}
	if reservationTTL <= 0 {
		return nil, fmt.Errorf("%w: reservation TTL must be positive", ErrInvalidRequest)
	}
	return &Service{store: store, reservationTTL: reservationTTL, now: time.Now}, nil
}

func (s *Service) SearchNumbers(ctx context.Context, filter NumberFilter) (NumbersPage, error) {
	if filter.Limit <= 0 || filter.Limit > maxSearchLimit {
		filter.Limit = defaultSearchLimit
	}
	if filter.NumberType != "" &&
		filter.NumberType != "Local" && filter.NumberType != "TollFree" && filter.NumberType != "Mobile" {
		return NumbersPage{}, ErrInvalidRequest
	}
	return s.store.SearchNumbers(ctx, filter)
}

func (s *Service) ListPlans(ctx context.Context) ([]Plan, error) {
	return s.store.ListPlans(ctx)
}

func (s *Service) Reserve(ctx context.Context, authenticatedUserID string, request ReserveRequest) (Reservation, error) {
	if strings.TrimSpace(authenticatedUserID) == "" || strings.TrimSpace(request.ProviderNumberID) == "" ||
		strings.TrimSpace(request.RentalPlanID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		len(request.IdempotencyKey) > 200 {
		return Reservation{}, ErrInvalidRequest
	}
	now := s.now().UTC()
	return s.store.Reserve(ctx, ReserveInput{
		UserID:               authenticatedUserID,
		ProviderNumberID:     request.ProviderNumberID,
		RentalPlanID:         request.RentalPlanID,
		IdempotencyKey:       request.IdempotencyKey,
		ReservedAt:           now,
		ReservationExpiresAt: now.Add(s.reservationTTL),
	})
}

func (s *Service) GetOrder(ctx context.Context, authenticatedUserID, orderID string) (Order, error) {
	if strings.TrimSpace(authenticatedUserID) == "" || strings.TrimSpace(orderID) == "" {
		return Order{}, ErrInvalidRequest
	}
	return s.store.GetOrder(ctx, authenticatedUserID, orderID)
}

// GetOrderByID looks up an order without checking ownership. It exists for
// trusted internal callers only (e.g. reconciling a payment webhook, which
// has no authenticated user context) and must never be exposed directly
// over HTTP.
func (s *Service) GetOrderByID(ctx context.Context, orderID string) (Order, error) {
	if strings.TrimSpace(orderID) == "" {
		return Order{}, ErrInvalidRequest
	}
	return s.store.GetOrderByID(ctx, orderID)
}

func (s *Service) ExpireReservation(ctx context.Context, rentalID string) (bool, error) {
	if strings.TrimSpace(rentalID) == "" {
		return false, ErrInvalidRequest
	}
	return s.store.ExpireReservation(ctx, rentalID, s.now().UTC())
}

// GetFulfillmentSnapshot is for trusted internal callers only (provisioning
// after payment); it is never exposed directly over HTTP.
func (s *Service) GetFulfillmentSnapshot(ctx context.Context, orderID string) (FulfillmentSnapshot, error) {
	if strings.TrimSpace(orderID) == "" {
		return FulfillmentSnapshot{}, ErrInvalidRequest
	}
	return s.store.GetFulfillmentSnapshot(ctx, orderID)
}

// SwapToReplacementNumber is called when failedNumberID could not be
// provisioned. It looks for another AVAILABLE number with the same type and
// capabilities, atomically releasing failedNumberID back to AVAILABLE and
// reserving the replacement in its place. If none is available, the order
// is moved to PENDING_FULFILLMENT and found is false; failedNumberID is left
// untouched (RESERVED) for operator attention.
func (s *Service) SwapToReplacementNumber(ctx context.Context, orderID, failedNumberID, numberType string, sms, mms, voice bool) (string, bool, error) {
	if strings.TrimSpace(orderID) == "" || strings.TrimSpace(failedNumberID) == "" {
		return "", false, ErrInvalidRequest
	}
	return s.store.SwapToReplacementNumber(ctx, orderID, failedNumberID, numberType, sms, mms, voice, s.now().UTC())
}

// ActivateRental marks providerNumberID ACTIVE and the order's rental
// activated, recording providerReference (the telephony provider's resource
// id for that number) and computing the rental's own expiry from now plus
// the order's plan duration.
func (s *Service) ActivateRental(ctx context.Context, orderID, providerNumberID, providerReference string, durationSeconds int32) error {
	if strings.TrimSpace(orderID) == "" || strings.TrimSpace(providerNumberID) == "" || strings.TrimSpace(providerReference) == "" {
		return ErrInvalidRequest
	}
	now := s.now().UTC()
	expiresAt := now.Add(time.Duration(durationSeconds) * time.Second)
	return s.store.ActivateRental(ctx, orderID, providerNumberID, providerReference, expiresAt, now)
}
