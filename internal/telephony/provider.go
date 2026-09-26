// Package telephony defines vendor-neutral number and messaging contracts.
package telephony

import (
	"context"
	"errors"
)

var (
	ErrInvalidRequest          = errors.New("telephony: invalid request")
	ErrInvalidWebhookSignature = errors.New("telephony: invalid webhook signature")
	ErrProviderNotFound        = errors.New("telephony: provider resource not found")
	ErrProviderRateLimited     = errors.New("telephony: provider rate limited")
	ErrProviderUnavailable     = errors.New("telephony: provider unavailable")
	ErrProviderRejected        = errors.New("telephony: provider rejected request")
)

const MaxInboundWebhookBytes = 256 << 10

type NumberType string

const (
	NumberTypeLocal    NumberType = "Local"
	NumberTypeTollFree NumberType = "TollFree"
	NumberTypeMobile   NumberType = "Mobile"
)

type Capabilities struct {
	SMS   bool
	MMS   bool
	Voice bool
}

type SearchNumbersRequest struct {
	CountryCode string
	Type        NumberType
	AreaCode    string
	Require     Capabilities
	PageSize    int
	Cursor      string
}

type AvailableNumber struct {
	PhoneNumber string
	Name        string
	Capabilities
}

type NumberSearchPage struct {
	Numbers    []AvailableNumber
	NextCursor string
}

type ProvisionNumberRequest struct {
	PhoneNumber       string
	SMSWebhookURL     string
	StatusCallbackURL string
}

type Number struct {
	// ProviderReference is opaque and must not be used as application identity.
	ProviderReference string
	PhoneNumber       string
}

type SendMessageRequest struct {
	From              string
	To                string
	Body              string
	StatusCallbackURL string
}

type MessageReceipt struct {
	// ProviderReference is opaque and is only for provider-side correlation.
	ProviderReference string
	Status            string
}

type InboundWebhook struct {
	// PublicURL must be the exact externally visible URL configured with Twilio.
	PublicURL string
	Signature string
	Body      []byte
}

type InboundMessage struct {
	// ProviderReference is opaque and is only for provider-side correlation.
	ProviderReference string
	From              string
	To                string
	Body              string
}

type Provider interface {
	SearchNumbers(ctx context.Context, request SearchNumbersRequest) (NumberSearchPage, error)
	ProvisionNumber(ctx context.Context, request ProvisionNumberRequest) (Number, error)
	ReleaseNumber(ctx context.Context, providerReference string) error
	SendMessage(ctx context.Context, request SendMessageRequest) (MessageReceipt, error)
}

type InboundMessageParser interface {
	ParseInboundMessage(ctx context.Context, webhook InboundWebhook) (InboundMessage, error)
}
