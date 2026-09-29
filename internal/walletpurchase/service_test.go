package walletpurchase

import (
	"context"
	"errors"
	"testing"

	"migo/internal/fulfillment"
	"migo/internal/rental"
	"migo/internal/wallet"
)

type fakeRentals struct {
	order rental.Order
	err   error
}

func (f *fakeRentals) Reserve(context.Context, string, rental.ReserveRequest) (rental.Reservation, error) {
	return rental.Reservation{Order: f.order}, f.err
}

type fakeWallets struct {
	purchaseErr error
	refundErr   error
	debited     *wallet.PurchaseDebit
	refunded    *wallet.RefundInput
}

func (f *fakeWallets) Purchase(_ context.Context, in wallet.PurchaseDebit) (wallet.Transaction, error) {
	f.debited = &in
	return wallet.Transaction{}, f.purchaseErr
}
func (f *fakeWallets) RefundPurchase(_ context.Context, in wallet.RefundInput) (wallet.Transaction, error) {
	f.refunded = &in
	return wallet.Transaction{}, f.refundErr
}

type fakeFulfiller struct{ err error }

func (f *fakeFulfiller) Fulfill(context.Context, string) error { return f.err }

func TestPurchaseDebitsWalletAndFulfills(t *testing.T) {
	order := rental.Order{ID: "order-1", PriceMinorUnits: 500, Currency: "NGN"}
	rentals := &fakeRentals{order: order}
	wallets := &fakeWallets{}
	fulfiller := &fakeFulfiller{}
	svc, _ := NewService(rentals, wallets, fulfiller)

	got, err := svc.Purchase(context.Background(), "user-1", PurchaseRequest{ProviderNumberID: "n1", RentalPlanID: "p1", IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != order.ID {
		t.Fatalf("got order %v, want %v", got, order)
	}
	if wallets.debited == nil || wallets.debited.AmountMinorUnits != 500 || wallets.debited.Reference != "order-1" {
		t.Fatalf("expected wallet debit for order price, got %+v", wallets.debited)
	}
	if wallets.refunded != nil {
		t.Fatalf("expected no refund on success, got %+v", wallets.refunded)
	}
}

func TestPurchaseInsufficientFundsLeavesReservation(t *testing.T) {
	order := rental.Order{ID: "order-2", PriceMinorUnits: 500, Currency: "NGN"}
	rentals := &fakeRentals{order: order}
	wallets := &fakeWallets{purchaseErr: wallet.ErrInsufficientFunds}
	fulfiller := &fakeFulfiller{}
	svc, _ := NewService(rentals, wallets, fulfiller)

	_, err := svc.Purchase(context.Background(), "user-1", PurchaseRequest{IdempotencyKey: "key-2"})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("got error %v, want ErrInsufficientFunds", err)
	}
}

func TestPurchaseRefundsOnHardFulfillmentFailure(t *testing.T) {
	order := rental.Order{ID: "order-3", PriceMinorUnits: 500, Currency: "NGN"}
	rentals := &fakeRentals{order: order}
	wallets := &fakeWallets{}
	fulfiller := &fakeFulfiller{err: errors.New("provider down")}
	svc, _ := NewService(rentals, wallets, fulfiller)

	_, err := svc.Purchase(context.Background(), "user-1", PurchaseRequest{IdempotencyKey: "key-3"})
	if err == nil {
		t.Fatal("expected fulfillment error")
	}
	if wallets.refunded == nil || wallets.refunded.AmountMinorUnits != 500 || wallets.refunded.OriginalIdempotencyKey != "key-3" {
		t.Fatalf("expected compensating refund tied to original idempotency key, got %+v", wallets.refunded)
	}
}

func TestPurchasePendingFulfillmentIsNotRefunded(t *testing.T) {
	order := rental.Order{ID: "order-4", PriceMinorUnits: 500, Currency: "NGN"}
	rentals := &fakeRentals{order: order}
	wallets := &fakeWallets{}
	fulfiller := &fakeFulfiller{err: fulfillment.ErrPendingFulfillment}
	svc, _ := NewService(rentals, wallets, fulfiller)

	_, err := svc.Purchase(context.Background(), "user-1", PurchaseRequest{IdempotencyKey: "key-4"})
	if !errors.Is(err, fulfillment.ErrPendingFulfillment) {
		t.Fatalf("got error %v, want ErrPendingFulfillment", err)
	}
	if wallets.refunded != nil {
		t.Fatalf("expected no refund while order is held for operator, got %+v", wallets.refunded)
	}
}
