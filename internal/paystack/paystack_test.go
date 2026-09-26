package paystack

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient("sk_test_secret")
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.client = server.Client()
	return client
}

func TestNewClientRequiresSecretKey(t *testing.T) {
	if _, err := NewClient(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty key error = %v", err)
	}
	if _, err := NewClient("   "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("blank key error = %v", err)
	}
}

func TestInitializeSendsExpectedRequestAndParsesResult(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/transaction/initialize" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk_test_secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["email"] != "payer@example.com" || body["amount"] != float64(1299) || body["currency"] != "USD" || body["reference"] != "order-1" {
			t.Errorf("body = %+v", body)
		}
		if body["callback_url"] != "https://app.example.com/paid" {
			t.Errorf("callback_url = %v", body["callback_url"])
		}
		_, _ = io.WriteString(w, `{"status":true,"message":"ok","data":{"authorization_url":"https://checkout.paystack.com/abc","access_code":"abc","reference":"order-1"}}`)
	}))

	result, err := client.Initialize(context.Background(), InitializeRequest{
		Email: "payer@example.com", AmountMinorUnits: 1299, Currency: "usd", Reference: "order-1",
		CallbackURL: "https://app.example.com/paid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthorizationURL != "https://checkout.paystack.com/abc" || result.Reference != "order-1" {
		t.Fatalf("result = %+v", result)
	}
}

func TestInitializeRejectsInvalidInput(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("provider should not be called for invalid input")
	}))
	cases := []InitializeRequest{
		{Email: "", AmountMinorUnits: 100, Currency: "USD", Reference: "r"},
		{Email: "a@b.com", AmountMinorUnits: 0, Currency: "USD", Reference: "r"},
		{Email: "a@b.com", AmountMinorUnits: 100, Currency: "", Reference: "r"},
		{Email: "a@b.com", AmountMinorUnits: 100, Currency: "USD", Reference: ""},
	}
	for _, req := range cases {
		if _, err := client.Initialize(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Initialize(%+v) error = %v", req, err)
		}
	}
}

func TestInitializeMapsProviderRejection(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":false,"message":"Invalid currency"}`)
	}))
	_, err := client.Initialize(context.Background(), InitializeRequest{
		Email: "a@b.com", AmountMinorUnits: 100, Currency: "XXX", Reference: "r",
	})
	if !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyParsesSuccessAndFailure(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/transaction/verify/order-1" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"status":true,"message":"ok","data":{"status":"success","amount":1299,"currency":"USD","reference":"order-1"}}`)
	}))
	result, err := client.Verify(context.Background(), "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || result.AmountMinorUnits != 1299 || result.Currency != "USD" {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyRejectsEmptyReference(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("provider should not be called")
	}))
	if _, err := client.Verify(context.Background(), "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestMapsServerErrorsAndRateLimits(t *testing.T) {
	unavailable := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if _, err := unavailable.Verify(context.Background(), "r"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("5xx error = %v", err)
	}

	rateLimited := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	if _, err := rateLimited.Verify(context.Background(), "r"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("429 error = %v", err)
	}
}

func TestVerifySignature(t *testing.T) {
	client, err := NewClient("sk_test_secret")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"charge.success","data":{"reference":"order-1"}}`)
	mac := hmac.New(sha512.New, []byte("sk_test_secret"))
	mac.Write(body)
	validSig := hex.EncodeToString(mac.Sum(nil))

	if !client.VerifySignature(body, validSig) {
		t.Fatal("expected valid signature to verify")
	}
	if client.VerifySignature(body, "") {
		t.Fatal("empty signature must not verify")
	}
	if client.VerifySignature(body, "not-hex!!") {
		t.Fatal("non-hex signature must not verify")
	}
	if client.VerifySignature([]byte("tampered body"), validSig) {
		t.Fatal("signature for a different body must not verify")
	}
}
