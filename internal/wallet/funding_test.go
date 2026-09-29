package wallet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"migo/internal/paystack"
)

type fundingStore struct {
	attempt FundingAttempt
	settled bool
}

func (f *fundingStore) CreateFundingAttempt(_ context.Context, walletID, userID, configID string, amount int64, currency, key string) (FundingAttempt, error) {
	f.attempt = FundingAttempt{ID: "attempt-1", WalletID: walletID, UserID: userID, ProviderConfigID: configID, AmountMinorUnits: amount, Currency: currency, Status: "initializing"}
	return f.attempt, nil
}
func (f *fundingStore) SetFundingReference(_ context.Context, id, ref, status string) error {
	f.attempt.ProviderReference = ref
	f.attempt.Status = status
	return nil
}
func (f *fundingStore) GetFundingAttempt(context.Context, string, string) (FundingAttempt, error) {
	return f.attempt, nil
}
func (f *fundingStore) GetFundingByReference(context.Context, string, string) (FundingAttempt, error) {
	return f.attempt, nil
}
func (f *fundingStore) SettleFunding(_ context.Context, a FundingAttempt, _ string) (FundingAttempt, error) {
	f.settled = true
	a.Status = "success"
	f.attempt = a
	return a, nil
}
func (f *fundingStore) UpdateFundingStatus(_ context.Context, _ string, status string) error {
	f.attempt.Status = status
	return nil
}

type fundingResolver struct{ client *paystack.Client }

func (f fundingResolver) Resolve(context.Context) (*paystack.Client, string, error) {
	return f.client, "config-1", nil
}

type emailLookup struct{}

func (emailLookup) Email(context.Context, string) (string, error) { return "user@example.com", nil }

func TestFundingCreditsOnlyAfterServerVerification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/transaction/initialize" {
			_, _ = w.Write([]byte(`{"status":true,"data":{"authorization_url":"https://checkout.example/test","reference":"attempt-1"}}`))
			return
		}
		if r.URL.Path == "/transaction/verify/attempt-1" {
			_, _ = w.Write([]byte(`{"status":true,"data":{"status":"success","amount":5000,"currency":"NGN","reference":"attempt-1"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := paystack.NewClientWithBaseURL("secret", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	baseStore := &fakeStore{wallet: Wallet{ID: "wallet-1", UserID: "user-1", Currency: "NGN"}}
	walletSvc, _ := NewService(baseStore)
	store := &fundingStore{}
	svc, err := NewFundingService(walletSvc, store, fundingResolver{client}, emailLookup{}, "")
	if err != nil {
		t.Fatal(err)
	}
	initialized, err := svc.Initialize(context.Background(), "user-1", "NGN", "funding-key", 5000)
	if err != nil {
		t.Fatal(err)
	}
	if initialized.Attempt.Status != "pending" || store.settled {
		t.Fatalf("initialized=%+v settled=%v", initialized, store.settled)
	}
	verified, err := svc.Verify(context.Background(), "user-1", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Status != "success" || !store.settled {
		t.Fatalf("verified=%+v settled=%v", verified, store.settled)
	}
}

func TestFundingAmountMismatchNeverCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":true,"data":{"status":"success","amount":1,"currency":"NGN","reference":"attempt-1"}}`))
	}))
	defer server.Close()
	client, _ := paystack.NewClientWithBaseURL("secret", server.URL, server.Client())
	store := &fundingStore{attempt: FundingAttempt{ID: "attempt-1", UserID: "user-1", ProviderConfigID: "config-1", ProviderReference: "attempt-1", AmountMinorUnits: 5000, Currency: "NGN", Status: "pending"}}
	walletSvc, _ := NewService(&fakeStore{})
	svc, _ := NewFundingService(walletSvc, store, fundingResolver{client}, emailLookup{}, "")
	_, err := svc.Verify(context.Background(), "user-1", "attempt-1")
	if err != ErrAmountMismatch || store.settled {
		t.Fatalf("error=%v settled=%v", err, store.settled)
	}
}
