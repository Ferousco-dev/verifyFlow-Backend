package telephony

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestTelnyxParsesSignedInboundMessage(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewTelnyx("key", base64.StdEncoding.EncodeToString(pub), "profile")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"data":{"event_type":"message.received","id":"event-1","payload":{"id":"msg-1","text":"hello","from":{"phone_number":"+14155550100"},"to":{"phone_number":"+14155550101"}}}}`)
	ts := "1700000000"
	sig := ed25519.Sign(priv, append([]byte(ts+"|"), body...))
	got, err := client.ParseInboundMessage(context.Background(), InboundWebhook{Body: body, Signature: ts + "." + base64.StdEncoding.EncodeToString(sig)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderReference != "msg-1" || got.Body != "hello" {
		t.Fatalf("message = %+v", got)
	}
}

func TestTelnyxRejectsInvalidSignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	client, _ := NewTelnyx("key", base64.StdEncoding.EncodeToString(pub), "profile")
	_, err := client.ParseInboundMessage(context.Background(), InboundWebhook{Body: []byte(`{}`), Signature: "1." + base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))})
	if err != ErrInvalidWebhookSignature {
		t.Fatalf("error = %v", err)
	}
}
