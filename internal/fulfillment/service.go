// Package fulfillment provisions a telephony number after a rental order's
// payment has been confirmed: it calls the telephony provider to actually
// provision the reserved number, and falls back to an equivalent
// replacement (same plan capacity/capabilities) if that fails.
package fulfillment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"migo/internal/rental"
	"migo/internal/telephony"
)

var (
	ErrInvalidRequest = errors.New("fulfillment: invalid request")
	// ErrPendingFulfillment means no equivalent replacement number was
	// available; the order has been moved to PENDING_FULFILLMENT and is
	// waiting on operator attention. This is an expected outcome, not a
	// system failure.
	ErrPendingFulfillment    = errors.New("fulfillment: no replacement number available; order held for operator")
	ErrProviderConfigMissing = errors.New("fulfillment: no enabled telephony provider config")
)

// Rentals is satisfied directly by *rental.Service.
type Rentals interface {
	GetFulfillmentSnapshot(ctx context.Context, orderID string) (rental.FulfillmentSnapshot, error)
	SwapToReplacementNumber(ctx context.Context, orderID, failedNumberID, numberType string, sms, mms, voice bool) (newNumberID string, found bool, err error)
	ActivateRental(ctx context.Context, orderID, providerNumberID, providerReference string, durationSeconds int32) error
}

// TelephonyResolver resolves the currently enabled telephony provider into
// a live client, decrypting its credentials on every call.
type TelephonyResolver interface {
	Resolve(ctx context.Context, providerConfigID string) (telephony.Provider, error)
}

type Service struct {
	rentals   Rentals
	telephony TelephonyResolver
}

func NewService(rentals Rentals, telephonyResolver TelephonyResolver) (*Service, error) {
	if rentals == nil || telephonyResolver == nil {
		return nil, fmt.Errorf("%w: rentals and a telephony resolver are required", ErrInvalidRequest)
	}
	return &Service{rentals: rentals, telephony: telephonyResolver}, nil
}

// Fulfill provisions orderID's assigned number. It is idempotent: calling
// it again after a successful activation is a no-op.
func (s *Service) Fulfill(ctx context.Context, orderID string) error {
	if strings.TrimSpace(orderID) == "" {
		return ErrInvalidRequest
	}
	snapshot, err := s.rentals.GetFulfillmentSnapshot(ctx, orderID)
	if err != nil {
		return err
	}
	if snapshot.ActivatedAt != nil {
		return nil
	}

	provider, err := s.telephony.Resolve(ctx, snapshot.ProviderConfigID)
	if err != nil {
		return err
	}

	result, provisionErr := provider.ProvisionNumber(ctx, telephony.ProvisionNumberRequest{PhoneNumber: snapshot.PhoneNumber})
	if provisionErr != nil {
		_, found, err := s.rentals.SwapToReplacementNumber(ctx, orderID, snapshot.ProviderNumberID, snapshot.NumberType, snapshot.SMSEnabled, snapshot.MMSEnabled, snapshot.VoiceEnabled)
		if err != nil {
			return err
		}
		if !found {
			return ErrPendingFulfillment
		}
		snapshot, err = s.rentals.GetFulfillmentSnapshot(ctx, orderID)
		if err != nil {
			return err
		}
		provider, err = s.telephony.Resolve(ctx, snapshot.ProviderConfigID)
		if err != nil {
			return err
		}
		result, err = provider.ProvisionNumber(ctx, telephony.ProvisionNumberRequest{PhoneNumber: snapshot.PhoneNumber})
		if err != nil {
			return err
		}
	}
	return s.rentals.ActivateRental(ctx, orderID, snapshot.ProviderNumberID, result.ProviderReference, snapshot.DurationSeconds)
}
