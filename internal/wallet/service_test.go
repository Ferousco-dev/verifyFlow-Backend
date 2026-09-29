package wallet

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct {
	wallet      Wallet
	transaction Transaction
	adjustment  Adjustment
	err         error
}

func (f *fakeStore) GetOrCreate(context.Context, string, string) (Wallet, error) {
	return f.wallet, f.err
}
func (f *fakeStore) ListTransactions(context.Context, string, string, int) ([]Transaction, error) {
	return nil, f.err
}
func (f *fakeStore) Adjust(_ context.Context, in Adjustment) (Transaction, error) {
	f.adjustment = in
	return f.transaction, f.err
}
func (f *fakeStore) Purchase(context.Context, PurchaseDebit) (Transaction, error) {
	return f.transaction, f.err
}
func (f *fakeStore) RefundPurchase(context.Context, RefundInput) (Transaction, error) {
	return f.transaction, f.err
}
func (f *fakeStore) Renew(context.Context, PurchaseDebit) (Transaction, error) {
	return f.transaction, f.err
}

func TestBalanceNormalizesCurrency(t *testing.T) {
	store := &fakeStore{wallet: Wallet{Currency: "NGN"}}
	svc, _ := NewService(store)
	got, err := svc.Balance(context.Background(), "user", " ngn ")
	if err != nil || got.Currency != "NGN" {
		t.Fatalf("balance=%+v err=%v", got, err)
	}
}
func TestAdjustmentRequiresReasonAndPositiveAmount(t *testing.T) {
	svc, _ := NewService(&fakeStore{})
	_, err := svc.Adjust(context.Background(), Adjustment{ActorUserID: "admin", UserID: "user", Currency: "NGN", Type: "adjustment_credit", AmountMinorUnits: 100, Reference: "ref", IdempotencyKey: "key"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error=%v", err)
	}
}
func TestDebitAdjustmentIsPassedToStore(t *testing.T) {
	store := &fakeStore{transaction: Transaction{ID: "tx"}}
	svc, _ := NewService(store)
	got, err := svc.Adjust(context.Background(), Adjustment{ActorUserID: "admin", UserID: "user", Currency: "ngn", Type: "adjustment_debit", AmountMinorUnits: 100, Reference: "ref", IdempotencyKey: "key", Reason: "chargeback"})
	if err != nil || got.ID != "tx" || store.adjustment.Currency != "NGN" {
		t.Fatalf("transaction=%+v adjustment=%+v err=%v", got, store.adjustment, err)
	}
}
