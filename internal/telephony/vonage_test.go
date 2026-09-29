package telephony

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func TestVonageParsesSignedInboundMessage(t *testing.T) {
	v, err := NewVonage("key", "secret", "signature-secret", "NG")
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"messageId": {"msg-1"}, "msisdn": {"2348012345678"}, "to": {"2348098765432"}, "text": {"hello"}, "timestamp": {"123"}, "nonce": {"abc"}}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var raw strings.Builder
	for _, k := range keys {
		raw.WriteByte('&')
		raw.WriteString(k)
		raw.WriteByte('=')
		raw.WriteString(values.Get(k))
	}
	mac := hmac.New(sha256.New, []byte("signature-secret"))
	_, _ = mac.Write([]byte(raw.String()))
	values.Set("sig", hex.EncodeToString(mac.Sum(nil)))
	got, err := v.ParseInboundMessage(context.Background(), InboundWebhook{Body: []byte(values.Encode())})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderReference != "msg-1" || got.From != "+2348012345678" || got.To != "+2348098765432" || got.Body != "hello" {
		t.Fatalf("message = %+v", got)
	}
}

func TestVonageRejectsInvalidSignature(t *testing.T) {
	v, _ := NewVonage("key", "secret", "signature-secret", "NG")
	_, err := v.ParseInboundMessage(context.Background(), InboundWebhook{Body: []byte("messageId=x&msisdn=1&to=2&sig=00")})
	if err != ErrInvalidWebhookSignature {
		t.Fatalf("error = %v", err)
	}
}
