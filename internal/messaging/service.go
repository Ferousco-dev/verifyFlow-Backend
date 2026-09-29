package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"migo/internal/telephony"
)

var (
	ErrInvalidRequest        = errors.New("messaging: invalid request")
	ErrNotFound              = errors.New("messaging: number or message not found")
	ErrNumberInactive        = errors.New("messaging: number is not active or SMS capable")
	ErrProviderConfigMissing = errors.New("messaging: provider configuration is unavailable")
)

type Message struct {
	ID, RentalID, ProviderNumberID, Direction, Sender, Recipient, Body, ProviderMessageID, Status string
	ReceivedAt, SentAt                                                                            *time.Time
	CreatedAt                                                                                     time.Time
}

type Number struct {
	ID, RentalID, PhoneNumber, NumberType, Status string
	SMSEnabled, MMSEnabled, VoiceEnabled          bool
	ActivatedAt, ExpiresAt                        *time.Time
}

type Page struct {
	Messages   []Message
	NextCursor string
}

type SendRequest struct{ ProviderNumberID, To, Body string }

type InboundInput struct {
	ProviderConfigID string
	Message          telephony.InboundMessage
	Payload          []byte
	ReceivedAt       time.Time
}

type OutboundTarget struct{ RentalID, ProviderNumberID, ProviderConfigID, PhoneNumber string }

type Store interface {
	RecordInbound(context.Context, InboundInput) (Message, bool, error)
	OutboundTarget(context.Context, string, string) (OutboundTarget, error)
	RecordOutbound(context.Context, string, OutboundTarget, string, string, telephony.MessageReceipt, time.Time) (Message, error)
	ListMessages(context.Context, string, string, string, int) (Page, error)
	ListNumbers(context.Context, string) ([]Number, error)
}

type Resolver interface {
	Resolve(context.Context, string) (telephony.Provider, error)
}

type Service struct {
	store    Store
	resolver Resolver
	now      func() time.Time
}

func NewService(store Store, resolver Resolver) (*Service, error) {
	if store == nil || resolver == nil {
		return nil, fmt.Errorf("%w: store and resolver are required", ErrInvalidRequest)
	}
	return &Service{store: store, resolver: resolver, now: time.Now}, nil
}

func (s *Service) ProcessInbound(ctx context.Context, providerConfigID string, parser telephony.InboundMessageParser, webhook telephony.InboundWebhook) (bool, error) {
	if strings.TrimSpace(providerConfigID) == "" || parser == nil {
		return false, ErrInvalidRequest
	}
	message, err := parser.ParseInboundMessage(ctx, webhook)
	if err != nil {
		return false, err
	}
	_, created, err := s.store.RecordInbound(ctx, InboundInput{ProviderConfigID: providerConfigID, Message: message, Payload: webhook.Body, ReceivedAt: s.now().UTC()})
	return created, err
}

func (s *Service) Send(ctx context.Context, userID string, in SendRequest) (Message, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(in.ProviderNumberID) == "" || !validE164(in.To) || strings.TrimSpace(in.Body) == "" || len([]rune(in.Body)) > 1600 {
		return Message{}, ErrInvalidRequest
	}
	target, err := s.store.OutboundTarget(ctx, userID, in.ProviderNumberID)
	if err != nil {
		return Message{}, err
	}
	provider, err := s.resolver.Resolve(ctx, target.ProviderConfigID)
	if err != nil {
		return Message{}, err
	}
	receipt, err := provider.SendMessage(ctx, telephony.SendMessageRequest{From: target.PhoneNumber, To: in.To, Body: in.Body})
	if err != nil {
		return Message{}, err
	}
	return s.store.RecordOutbound(ctx, userID, target, in.To, in.Body, receipt, s.now().UTC())
}

func (s *Service) ListMessages(ctx context.Context, userID, numberID, cursor string, limit int) (Page, error) {
	if userID == "" || limit < 0 || limit > 100 {
		return Page{}, ErrInvalidRequest
	}
	if limit == 0 {
		limit = 50
	}
	return s.store.ListMessages(ctx, userID, numberID, cursor, limit)
}

func (s *Service) ListNumbers(ctx context.Context, userID string) ([]Number, error) {
	if userID == "" {
		return nil, ErrInvalidRequest
	}
	return s.store.ListNumbers(ctx, userID)
}

func validE164(v string) bool {
	if len(v) < 3 || len(v) > 16 || v[0] != '+' || v[1] == '0' {
		return false
	}
	for _, r := range v[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
