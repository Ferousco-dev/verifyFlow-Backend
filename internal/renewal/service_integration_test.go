package renewal

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/rental"
	"migo/internal/testutil/dbtest"
	"migo/internal/wallet"
)

var renewalPhoneSequence atomic.Uint64

type renewalFixture struct {
	pool    *pgxpool.Pool
	rentals *rental.Service
	wallets *wallet.Service
	svc     *Service
	userID  string
	price   int64
}

// newRenewalFixture seeds one user, one provider number, and one plan
// (price 500), then activates a rental for that user with expiresAt already
// in the past — i.e. its first billing period has already ended and it is
// immediately due for renewal.
func newRenewalFixture(t *testing.T, fundedMinorUnits int64, expiresAt time.Time) renewalFixture {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	ctx := context.Background()

	var userID, configID, planID, numberID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email) VALUES ('Renewal User', 'renewal-user', 'renewal@example.com') RETURNING id::text`,
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
		 VALUES ('one-month', 'One month', 2592000, 200, $1, 'USD') RETURNING id::text`, price,
	).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	phoneNumber := fmt.Sprintf("+1415555%04d", renewalPhoneSequence.Add(1)%10000)
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, sms_enabled)
		 VALUES ($1::uuid, $2, $3, 'Local', true) RETURNING id::text`,
		configID, "provider-ref-"+phoneNumber, phoneNumber,
	).Scan(&numberID); err != nil {
		t.Fatal(err)
	}
	// The provider_numbers <-> rentals consistency check is a deferred
	// constraint trigger, so the RESERVED -> ACTIVE walk, the order, and the
	// rentals row all have to land in one transaction: only the final state
	// (ACTIVE, one activated rental) is checked, at commit.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'RESERVED' WHERE id = $1::uuid`, numberID); err != nil {
		t.Fatal(err)
	}
	var orderID, rentalID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, provider_cost_minor_units_snapshot, price_minor_units_snapshot, currency_snapshot, idempotency_key, order_status)
		 VALUES ($1::uuid, $2::uuid, 'one-month', 'One month', 2592000, 200, $3, 'USD', 'seed-key', 'PENDING') RETURNING id::text`,
		userID, planID, price,
	).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	reservedAt := expiresAt.Add(-30 * 24 * time.Hour)
	reservationExpiresAt := reservedAt.Add(time.Minute)
	if err := tx.QueryRow(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id, reserved_at, reservation_expires_at, activated_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $4, $6) RETURNING id::text`,
		orderID, userID, numberID, reservedAt, reservationExpiresAt, expiresAt,
	).Scan(&rentalID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'ACTIVE' WHERE id = $1::uuid`, numberID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
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
	svc, err := NewService(rentalSvc, walletSvc)
	if err != nil {
		t.Fatal(err)
	}
	return renewalFixture{pool: pool, rentals: rentalSvc, wallets: walletSvc, svc: svc, userID: userID, price: price}
}

func TestRunDueRenewsWhenWalletCanPay(t *testing.T) {
	f := newRenewalFixture(t, 1000, time.Now().Add(-time.Hour))

	result, err := f.svc.RunDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Renewed != 1 || result.Expired != 0 {
		t.Fatalf("result=%+v, want exactly one renewal", result)
	}

	balance, err := f.wallets.Balance(context.Background(), f.userID, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableMinorUnits != 1000-f.price {
		t.Fatalf("balance=%d, want %d (debited exactly one renewal period)", balance.AvailableMinorUnits, 1000-f.price)
	}

	var renewalCount int32
	var newExpiresAt time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT renewal_count, expires_at FROM rentals WHERE user_id = $1::uuid`, f.userID,
	).Scan(&renewalCount, &newExpiresAt); err != nil {
		t.Fatal(err)
	}
	if renewalCount != 1 {
		t.Fatalf("renewal_count=%d, want 1", renewalCount)
	}
	if !newExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at=%v, want a future date (extended by one period)", newExpiresAt)
	}

	// Running again immediately must be a no-op: the rental is no longer due.
	result2, err := f.svc.RunDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result2.Renewed != 0 && result2.Expired != 0 {
		t.Fatalf("second run=%+v, want no-op (rental not yet due again)", result2)
	}
}

func TestRunDueExpiresWhenWalletCannotPay(t *testing.T) {
	f := newRenewalFixture(t, 100, time.Now().Add(-time.Hour)) // funded for less than the price

	result, err := f.svc.RunDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Expired != 1 || result.Renewed != 0 {
		t.Fatalf("result=%+v, want exactly one expiry", result)
	}

	balance, err := f.wallets.Balance(context.Background(), f.userID, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableMinorUnits != 100 {
		t.Fatalf("balance=%d, want 100 (failed debit must not touch the balance)", balance.AvailableMinorUnits)
	}

	var endedAt *time.Time
	var numberStatus string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT r.ended_at, pn.status FROM rentals r JOIN provider_numbers pn ON pn.id = r.provider_number_id WHERE r.user_id = $1::uuid`,
		f.userID,
	).Scan(&endedAt, &numberStatus); err != nil {
		t.Fatal(err)
	}
	if endedAt == nil {
		t.Fatal("expected rental.ended_at to be set")
	}
	if numberStatus != "EXPIRED" {
		t.Fatalf("provider_numbers.status = %q, want EXPIRED", numberStatus)
	}
}
