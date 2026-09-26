package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"migo/internal/paystack"
)

// ---- fakes ----

type fakeStore struct {
	attempts    []Attempt
	recordedNew map[string]bool
	processed   map[string]bool
	createErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{recordedNew: map[string]bool{}, processed: map[string]bool{}}
}

func (f *fakeStore) CreateAttempt(_ context.Context, orderID, providerConfigID string, amount int64, currency string) (Attempt, error) {
	if f.createErr != nil {
		return Attempt{}, f.createErr
	}
	a := Attempt{
		ID: fmt.Sprintf("attempt-%d", len(f.attempts)+1), OrderID: orderID, ProviderConfigID: providerConfigID,
		ProviderStatus: "initializing", AmountMinorUnits: amount, Currency: currency, CreatedAt: time.Now(),
	}
	f.attempts = append(f.attempts, a)
	return a, nil
}

func (f *fakeStore) SetReference(_ context.Context, id, reference, status string) error {
	for i := range f.attempts {
		if f.attempts[i].ID == id {
			f.attempts[i].ProviderReference = reference
			f.attempts[i].ProviderStatus = status
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) UpdateStatus(_ context.Context, id, status string) error {
	for i := range f.attempts {
		if f.attempts[i].ID == id {
			f.attempts[i].ProviderStatus = status
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) GetLatestForOrder(_ context.Context, orderID string) (Attempt, error) {
	for i := len(f.attempts) - 1; i >= 0; i-- {
		if f.attempts[i].OrderID == orderID {
			return f.attempts[i], nil
		}
	}
	return Attempt{}, ErrNotFound
}

func (f *fakeStore) GetByReference(_ context.Context, providerConfigID, reference string) (Attempt, error) {
	for _, a := range f.attempts {
		if a.ProviderConfigID == providerConfigID && a.ProviderReference == reference {
			return a, nil
		}
	}
	return Attempt{}, ErrNotFound
}

func (f *fakeStore) HasSuccessfulPayment(_ context.Context, orderID string) (bool, error) {
	for _, a := range f.attempts {
		if a.OrderID == orderID && a.ProviderStatus == "success" {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) RecordWebhookEvent(_ context.Context, providerConfigID, idempotencyKey, _, _ string, _ []byte) (bool, error) {
	key := providerConfigID + "|" + idempotencyKey
	if f.recordedNew[key] {
		return false, nil
	}
	f.recordedNew[key] = true
	return true, nil
}

func (f *fakeStore) MarkWebhookProcessed(_ context.Context, providerConfigID, idempotencyKey string) error {
	f.processed[providerConfigID+"|"+idempotencyKey] = true
	return nil
}

type fakeOrders struct {
	byID map[string]OrderSnapshot
}

func (f *fakeOrders) GetOrder(_ context.Context, userID, orderID string) (OrderSnapshot, error) {
	o, ok := f.byID[orderID]
	if !ok || o.UserID != userID {
		return OrderSnapshot{}, ErrOrderNotFound
	}
	return o, nil
}

func (f *fakeOrders) GetOrderByID(_ context.Context, orderID string) (OrderSnapshot, error) {
	o, ok := f.byID[orderID]
	if !ok {
		return OrderSnapshot{}, ErrOrderNotFound
	}
	return o, nil
}

type fakeEmails struct{ email string }

func (f fakeEmails) Email(context.Context, string) (string, error) { return f.email, nil }

type fakeResolver struct {
	client           *paystack.Client
	providerConfigID string
	err              error
}

type fakeFulfiller struct{ err error }

func (f fakeFulfiller) Fulfill(context.Context, string) error { return f.err }

func (f fakeResolver) Resolve(context.Context) (*paystack.Client, string, error) {
	return f.client, f.providerConfigID, f.err
}

func newTestClient(t *testing.T, handler http.Handler) *paystack.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := paystack.NewClientWithBaseURL("sk_test_secret", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// ---- Initialize ----

func TestInitializeCreatesAttemptAndCallsPaystack(t *testing.T) {
	var capturedReference string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		capturedReference, _ = body["reference"].(string)
		_, _ = io.WriteString(w, `{"status":true,"data":{"authorization_url":"https://checkout.paystack.com/x","reference":"`+capturedReference+`"}}`)
	}))
	store := newFakeStore()
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{email: "payer@example.com"}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.Initialize(context.Background(), "user-1", "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthorizationURL != "https://checkout.paystack.com/x" {
		t.Fatalf("result = %+v", result)
	}
	if len(store.attempts) != 1 || store.attempts[0].ProviderReference != capturedReference || store.attempts[0].ProviderStatus != "pending" {
		t.Fatalf("attempt = %+v", store.attempts)
	}
	if capturedReference != store.attempts[0].ID {
		t.Fatalf("Paystack reference %q must be the attempt ID %q", capturedReference, store.attempts[0].ID)
	}
}

func TestInitializeRejectsNonPendingOrder(t *testing.T) {
	store := newFakeStore()
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "EXPIRED", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{email: "a@b.com"}, fakeResolver{}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Initialize(context.Background(), "user-1", "order-1"); !errors.Is(err, ErrOrderNotPending) {
		t.Fatalf("error = %v", err)
	}
}

func TestInitializeRejectsAlreadyPaidOrder(t *testing.T) {
	store := newFakeStore()
	store.attempts = append(store.attempts, Attempt{ID: "prior", OrderID: "order-1", ProviderStatus: "success"})
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{email: "a@b.com"}, fakeResolver{}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Initialize(context.Background(), "user-1", "order-1"); !errors.Is(err, ErrAlreadyPaid) {
		t.Fatalf("error = %v", err)
	}
}

func TestInitializeRejectsOrderOwnedByAnotherUser(t *testing.T) {
	store := newFakeStore()
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "someone-else", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{email: "a@b.com"}, fakeResolver{}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Initialize(context.Background(), "user-1", "order-1"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("error = %v", err)
	}
}

// ---- Verify ----

func TestVerifyMarksSuccessWhenAmountAndCurrencyMatch(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":true,"data":{"status":"success","amount":1299,"currency":"USD","reference":"ref-1"}}`)
	}))
	store := newFakeStore()
	store.attempts = append(store.attempts, Attempt{ID: "a1", OrderID: "order-1", ProviderReference: "ref-1", ProviderStatus: "pending", AmountMinorUnits: 1299, Currency: "USD"})
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}

	attempt, err := svc.Verify(context.Background(), "user-1", "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ProviderStatus != "success" {
		t.Fatalf("status = %q", attempt.ProviderStatus)
	}
}

func TestVerifyFlagsAmountMismatchInsteadOfSuccess(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":true,"data":{"status":"success","amount":1,"currency":"USD","reference":"ref-1"}}`)
	}))
	store := newFakeStore()
	store.attempts = append(store.attempts, Attempt{ID: "a1", OrderID: "order-1", ProviderReference: "ref-1", ProviderStatus: "pending", AmountMinorUnits: 1299, Currency: "USD"})
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Verify(context.Background(), "user-1", "order-1")
	if !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("error = %v", err)
	}
	if store.attempts[0].ProviderStatus != "amount_mismatch" {
		t.Fatalf("persisted status = %q", store.attempts[0].ProviderStatus)
	}
}

func TestVerifyMarksSupersededWhenAnotherAttemptAlreadySucceeded(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":true,"data":{"status":"success","amount":1299,"currency":"USD","reference":"ref-2"}}`)
	}))
	store := newFakeStore()
	store.attempts = append(store.attempts,
		Attempt{ID: "a1", OrderID: "order-1", ProviderReference: "ref-1", ProviderStatus: "success", AmountMinorUnits: 1299, Currency: "USD"},
		Attempt{ID: "a2", OrderID: "order-1", ProviderReference: "ref-2", ProviderStatus: "pending", AmountMinorUnits: 1299, Currency: "USD"},
	)
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}

	attempt, err := svc.reconcile(context.Background(), client, 1299, "USD", store.attempts[1])
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ProviderStatus != "superseded" {
		t.Fatalf("status = %q, want superseded", attempt.ProviderStatus)
	}
	if store.attempts[1].ProviderStatus != "superseded" {
		t.Fatalf("persisted status = %q", store.attempts[1].ProviderStatus)
	}
	// The original successful attempt is untouched.
	if store.attempts[0].ProviderStatus != "success" {
		t.Fatalf("original attempt status = %q", store.attempts[0].ProviderStatus)
	}
}

func TestVerifyShortCircuitsWhenAlreadySuccessful(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Paystack must not be called for an already-successful attempt")
	}))
	store := newFakeStore()
	store.attempts = append(store.attempts, Attempt{ID: "a1", OrderID: "order-1", ProviderReference: "ref-1", ProviderStatus: "success", AmountMinorUnits: 1299, Currency: "USD"})
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.Verify(context.Background(), "user-1", "order-1")
	if err != nil || attempt.ProviderStatus != "success" {
		t.Fatalf("attempt = %+v, err = %v", attempt, err)
	}
}

// ---- Webhook ----

func signedBody(t *testing.T, body []byte) string {
	t.Helper()
	mac := hmac.New(sha512.New, []byte("sk_test_secret"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHandleWebhookEventRejectsInvalidSignature(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Paystack must not be called when the signature is invalid")
	}))
	svc, err := NewService(newFakeStore(), &fakeOrders{byID: map[string]OrderSnapshot{}}, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"charge.success","data":{"id":1,"reference":"ref-1"}}`)
	if err := svc.HandleWebhookEvent(context.Background(), body, "not-a-valid-signature"); !errors.Is(err, ErrInvalidWebhookSignature) {
		t.Fatalf("error = %v", err)
	}
}

func TestHandleWebhookEventReconcilesChargeSuccess(t *testing.T) {
	verifyCalls := 0
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyCalls++
		_, _ = io.WriteString(w, `{"status":true,"data":{"status":"success","amount":1299,"currency":"USD","reference":"ref-1"}}`)
	}))
	store := newFakeStore()
	store.attempts = append(store.attempts, Attempt{ID: "a1", OrderID: "order-1", ProviderConfigID: "cfg-1", ProviderReference: "ref-1", ProviderStatus: "pending", AmountMinorUnits: 1299, Currency: "USD"})
	orders := &fakeOrders{byID: map[string]OrderSnapshot{
		"order-1": {ID: "order-1", UserID: "user-1", Status: "PENDING", PriceMinorUnits: 1299, Currency: "USD"},
	}}
	svc, err := NewService(store, orders, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"event":"charge.success","data":{"id":42,"reference":"ref-1"}}`)
	sig := signedBody(t, body)
	if err := svc.HandleWebhookEvent(context.Background(), body, sig); err != nil {
		t.Fatal(err)
	}
	if store.attempts[0].ProviderStatus != "success" {
		t.Fatalf("attempt status = %q", store.attempts[0].ProviderStatus)
	}
	if verifyCalls != 1 {
		t.Fatalf("verify calls = %d", verifyCalls)
	}

	// Redelivery of the same event is a no-op: no second Paystack call.
	if err := svc.HandleWebhookEvent(context.Background(), body, sig); err != nil {
		t.Fatal(err)
	}
	if verifyCalls != 1 {
		t.Fatalf("verify calls after redelivery = %d, want still 1", verifyCalls)
	}
}

func TestHandleWebhookEventIgnoresUnknownReference(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Paystack must not be called for a reference we never created")
	}))
	svc, err := NewService(newFakeStore(), &fakeOrders{byID: map[string]OrderSnapshot{}}, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"charge.success","data":{"id":99,"reference":"unknown-ref"}}`)
	if err := svc.HandleWebhookEvent(context.Background(), body, signedBody(t, body)); err != nil {
		t.Fatal(err)
	}
}

func TestHandleWebhookEventIgnoresNonChargeSuccessEvents(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Paystack must not be called for events we don't reconcile")
	}))
	svc, err := NewService(newFakeStore(), &fakeOrders{byID: map[string]OrderSnapshot{}}, fakeEmails{}, fakeResolver{client: client, providerConfigID: "cfg-1"}, fakeFulfiller{}, "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"charge.failed","data":{"id":7,"reference":"ref-1"}}`)
	if err := svc.HandleWebhookEvent(context.Background(), body, signedBody(t, body)); err != nil {
		t.Fatal(err)
	}
}
