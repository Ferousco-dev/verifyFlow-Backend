package walletpurchase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/fulfillment"
	"migo/internal/rental"
	"migo/internal/testutil/dbtest"
	"migo/internal/wallet"
)

// concurrentFulfiller lets a test force every Fulfill call to succeed or to
// fail the same way, simulating N racing retries of the same purchase
// request (e.g. a client that times out and resubmits).
type concurrentFulfiller struct {
	err atomic.Value // error
}

func (f *concurrentFulfiller) setErr(err error) { f.err.Store(&err) }
func (f *concurrentFulfiller) Fulfill(context.Context, string) error {
	if v := f.err.Load(); v != nil {
		return *(v.(*error))
	}
	return nil
}

var fixturePhoneSequence atomic.Uint64

type purchaseFixture struct {
	pool     *pgxpool.Pool
	rentals  *rental.Service
	wallets  *wallet.Service
	userID   string
	planID   string
	configID string
	price    int64
}

func newPurchaseFixture(t *testing.T, fundedMinorUnits int64) purchaseFixture {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	ctx := context.Background()

	var userID, configID, planID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email) VALUES ('Purchase User', 'purchase-user', 'purchase@example.com') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ('telephony', 'twilio', 'primary', decode('010203', 'hex'), 'test-v1') RETURNING id::text`,
	).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	const price = int64(500)
	if err := pool.QueryRow(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, provider_cost_minor_units, price_minor_units, currency)
		 VALUES ('one-hour', 'One hour', 3600, 200, $1, 'USD') RETURNING id::text`, price,
	).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO wallets (user_id, currency, available_minor_units) VALUES ($1::uuid, 'USD', $2)`,
		userID, fundedMinorUnits,
	); err != nil {
		t.Fatal(err)
	}

	rentalSvc, err := rental.NewService(rental.NewRepository(pool), 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	walletSvc, err := wallet.NewService(wallet.NewRepository(pool))
	if err != nil {
		t.Fatal(err)
	}
	return purchaseFixture{pool: pool, rentals: rentalSvc, wallets: walletSvc, userID: userID, planID: planID, configID: configID, price: price}
}

func (f purchaseFixture) addNumber(t *testing.T) string {
	t.Helper()
	var id string
	phoneNumber := fmt.Sprintf("+1415555%04d", fixturePhoneSequence.Add(1)%10000)
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, sms_enabled)
		 VALUES ($1::uuid, $2, $3, 'Local', true) RETURNING id::text`,
		f.configID, "provider-ref-"+phoneNumber, phoneNumber,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestConcurrentPurchaseRetriesDebitExactlyOnce simulates a client that
// resubmits the same purchase request (same Idempotency-Key) several times
// concurrently, e.g. after a slow response makes it think the first attempt
// failed. Only one wallet debit for the order's price must ever land.
func TestConcurrentPurchaseRetriesDebitExactlyOnce(t *testing.T) {
	f := newPurchaseFixture(t, 500)
	numberID := f.addNumber(t)
	fulfiller := &concurrentFulfiller{}
	svc, err := NewService(f.rentals, f.wallets, fulfiller)
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	errs := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Purchase(context.Background(), f.userID, PurchaseRequest{
				ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "concurrent-key",
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", i, err)
		}
	}

	balance, err := f.wallets.Balance(context.Background(), f.userID, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableMinorUnits != 0 {
		t.Fatalf("wallet balance = %d, want 0 (debited exactly once for price %d)", balance.AvailableMinorUnits, f.price)
	}

	var purchaseCount int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wallet_transactions WHERE user_id = $1::uuid AND transaction_type = 'purchase'`, f.userID,
	).Scan(&purchaseCount); err != nil {
		t.Fatal(err)
	}
	if purchaseCount != 1 {
		t.Fatalf("purchase ledger rows = %d, want exactly 1", purchaseCount)
	}
}

// TestConcurrentPurchaseRetriesRefundExactlyOnce forces every fulfillment
// attempt to fail permanently and resubmits the same request concurrently.
// Exactly one compensating refund must land, restoring the wallet to its
// pre-purchase balance — never more, never less.
func TestConcurrentPurchaseRetriesRefundExactlyOnce(t *testing.T) {
	f := newPurchaseFixture(t, 500)
	numberID := f.addNumber(t)
	fulfiller := &concurrentFulfiller{}
	fulfiller.setErr(errors.New("provider permanently unavailable"))
	svc, err := NewService(f.rentals, f.wallets, fulfiller)
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every attempt is expected to surface the fulfillment error;
			// only the compensating-refund bookkeeping is under test.
			_, _ = svc.Purchase(context.Background(), f.userID, PurchaseRequest{
				ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "concurrent-refund-key",
			})
		}()
	}
	wg.Wait()

	balance, err := f.wallets.Balance(context.Background(), f.userID, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableMinorUnits != 500 {
		t.Fatalf("wallet balance = %d, want 500 (debit fully reversed by exactly one refund)", balance.AvailableMinorUnits)
	}

	var refundCount int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wallet_transactions WHERE user_id = $1::uuid AND transaction_type = 'refund'`, f.userID,
	).Scan(&refundCount); err != nil {
		t.Fatal(err)
	}
	if refundCount != 1 {
		t.Fatalf("refund ledger rows = %d, want exactly 1", refundCount)
	}
}

// TestPendingFulfillmentIsNeverRefunded guards against a regression where a
// held-for-operator order (fulfillment.ErrPendingFulfillment) is mistakenly
// treated as a hard failure and refunded — the customer was reserved a
// number and still owes for it; an operator, not a refund, resolves this.
func TestPendingFulfillmentIsNeverRefunded(t *testing.T) {
	f := newPurchaseFixture(t, 500)
	numberID := f.addNumber(t)
	fulfiller := &concurrentFulfiller{}
	fulfiller.setErr(fulfillment.ErrPendingFulfillment)
	svc, err := NewService(f.rentals, f.wallets, fulfiller)
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Purchase(context.Background(), f.userID, PurchaseRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "pending-key",
	})
	if !errors.Is(err, fulfillment.ErrPendingFulfillment) {
		t.Fatalf("got error %v, want ErrPendingFulfillment", err)
	}

	balance, err := f.wallets.Balance(context.Background(), f.userID, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableMinorUnits != 0 {
		t.Fatalf("wallet balance = %d, want 0 (debit stands while order is held for operator)", balance.AvailableMinorUnits)
	}
}
