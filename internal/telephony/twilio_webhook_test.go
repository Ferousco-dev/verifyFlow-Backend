package telephony

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func signTwilioForm(authToken, targetURL string, fields url.Values) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var signingInput strings.Builder
	signingInput.WriteString(targetURL)
	for _, key := range keys {
		for _, value := range fields[key] {
			signingInput.WriteString(key)
			signingInput.WriteString(value)
		}
	}
	hash := hmac.New(sha1.New, []byte(authToken))
	_, _ = hash.Write([]byte(signingInput.String()))
	return base64.StdEncoding.EncodeToString(hash.Sum(nil))
}

func TestTwilioParsesOnlySignedInboundMessages(t *testing.T) {
	const (
		token      = "webhook-token"
		webhookURL = "https://api.example.com/webhooks/twilio?source=sms"
	)
	fields := url.Values{
		"MessageSid": {"SM-provider-reference"},
		"From":       {"+14155550100"},
		"To":         {"+14155550101"},
		"Body":       {"hello from Twilio"},
	}
	provider, err := NewTwilio("AC-test", token)
	if err != nil {
		t.Fatal(err)
	}
	webhook := InboundWebhook{PublicURL: webhookURL, Signature: signTwilioForm(token, webhookURL, fields), Body: []byte(fields.Encode())}
	message, err := provider.ParseInboundMessage(context.Background(), webhook)
	if err != nil {
		t.Fatal(err)
	}
	if message.ProviderReference != "SM-provider-reference" || message.From != "+14155550100" ||
		message.To != "+14155550101" || message.Body != "hello from Twilio" {
		t.Fatalf("normalized message = %+v", message)
	}

	webhook.Body = []byte(url.Values{"MessageSid": {"SM-forged"}, "From": {"+14155550100"}, "To": {"+14155550101"}}.Encode())
	if _, err := provider.ParseInboundMessage(context.Background(), webhook); !errors.Is(err, ErrInvalidWebhookSignature) {
		t.Fatalf("tampered event error = %v", err)
	}
	webhook.Signature = "invalid"
	if _, err := provider.ParseInboundMessage(context.Background(), webhook); !errors.Is(err, ErrInvalidWebhookSignature) {
		t.Fatalf("invalid signature error = %v", err)
	}
}

func TestTwilioInboundRejectsMalformedOrStatusEvents(t *testing.T) {
	const (
		token      = "webhook-token"
		webhookURL = "https://api.example.com/webhooks/twilio"
	)
	provider, err := NewTwilio("AC-test", token)
	if err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string]url.Values{
		"missing message SID": {"From": {"+14155550100"}, "To": {"+14155550101"}},
		"status callback":     {"MessageSid": {"SM-status"}, "From": {"+14155550100"}, "To": {"+14155550101"}, "MessageStatus": {"delivered"}},
		"media message":       {"MessageSid": {"MM-media"}, "From": {"+14155550100"}, "To": {"+14155550101"}, "NumMedia": {"1"}},
		"bad media count":     {"MessageSid": {"SM-text"}, "From": {"+14155550100"}, "To": {"+14155550101"}, "NumMedia": {"many"}},
	} {
		webhook := InboundWebhook{PublicURL: webhookURL, Body: []byte(fields.Encode()), Signature: signTwilioForm(token, webhookURL, fields)}
		if _, err := provider.ParseInboundMessage(context.Background(), webhook); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s error = %v", name, err)
		}
	}
	if _, err := provider.ParseInboundMessage(context.Background(), InboundWebhook{PublicURL: "//example.com/hook"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("relative URL error = %v", err)
	}
}

func TestTwilioInboundHonorsCanceledContext(t *testing.T) {
	provider, err := NewTwilio("AC-test", "webhook-token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.ParseInboundMessage(ctx, InboundWebhook{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
}

func TestTwilioInboundRejectsOversizedWebhook(t *testing.T) {
	provider, err := NewTwilio("AC-test", "webhook-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.ParseInboundMessage(context.Background(), InboundWebhook{Body: make([]byte, MaxInboundWebhookBytes+1)})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized body error = %v", err)
	}
}
