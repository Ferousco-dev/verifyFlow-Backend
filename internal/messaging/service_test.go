package messaging

import (
	"context"
	"errors"
	"testing"
	"time"

	"migo/internal/telephony"
)

type fakeStore struct {
	target   OutboundTarget
	outbound Message
	inbound  Message
	created  bool
	err      error
}

func (f *fakeStore) RecordInbound(context.Context, InboundInput) (Message, bool, error) {
	return f.inbound, f.created, f.err
}
func (f *fakeStore) OutboundTarget(context.Context, string, string) (OutboundTarget, error) {
	return f.target, f.err
}
func (f *fakeStore) RecordOutbound(context.Context, string, OutboundTarget, string, string, telephony.MessageReceipt, time.Time) (Message, error) {
	return f.outbound, f.err
}
func (f *fakeStore) ListMessages(context.Context, string, string, string, int) (Page, error) {
	return Page{}, f.err
}
func (f *fakeStore) ListNumbers(context.Context, string) ([]Number, error) { return nil, f.err }

type fakeResolver struct {
	provider telephony.Provider
	err      error
}

func (f fakeResolver) Resolve(context.Context, string) (telephony.Provider, error) {
	return f.provider, f.err
}

type fakeProvider struct{ sent telephony.SendMessageRequest }

func (f *fakeProvider) SearchNumbers(context.Context, telephony.SearchNumbersRequest) (telephony.NumberSearchPage, error) {
	return telephony.NumberSearchPage{}, nil
}
func (f *fakeProvider) ProvisionNumber(context.Context, telephony.ProvisionNumberRequest) (telephony.Number, error) {
	return telephony.Number{}, nil
}
func (f *fakeProvider) ReleaseNumber(context.Context, string) error { return nil }
func (f *fakeProvider) SendMessage(_ context.Context, in telephony.SendMessageRequest) (telephony.MessageReceipt, error) {
	f.sent = in
	return telephony.MessageReceipt{ProviderReference: "msg-1", Status: "queued"}, nil
}

func TestSendUsesOwnedActiveNumberProvider(t *testing.T) {
	store := &fakeStore{target: OutboundTarget{ProviderConfigID: "cfg", PhoneNumber: "+14155550100"}, outbound: Message{ID: "message"}}
	provider := &fakeProvider{}
	svc, _ := NewService(store, fakeResolver{provider: provider})
	got, err := svc.Send(context.Background(), "user", SendRequest{ProviderNumberID: "number", To: "+14155550101", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "message" || provider.sent.From != "+14155550100" || provider.sent.To != "+14155550101" {
		t.Fatalf("got=%+v sent=%+v", got, provider.sent)
	}
}
func TestSendRejectsInvalidRecipient(t *testing.T) {
	svc, _ := NewService(&fakeStore{}, fakeResolver{})
	_, err := svc.Send(context.Background(), "user", SendRequest{ProviderNumberID: "number", To: "0800", Body: "hello"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error=%v", err)
	}
}
