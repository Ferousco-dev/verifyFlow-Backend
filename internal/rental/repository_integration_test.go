package rental

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/testutil/dbtest"
)

type rentalFixture struct {
	pool       *pgxpool.Pool
	repository *Repository
	service    *Service
	userID     string
	configID   string
	planID     string
}

var fixturePhoneSequence atomic.Uint64

func newRentalFixture(t *testing.T, ttl time.Duration) rentalFixture {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	ctx := context.Background()
	var userID, configID, planID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email) VALUES ('Rental User', 'rental-user', 'rental@example.com') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ('telephony', 'twilio', 'primary', decode('010203', 'hex'), 'test-v1') RETURNING id::text`,
	).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency)
		 VALUES ('one-hour', 'One hour', 3600, 1299, 'USD') RETURNING id::text`,
	).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool)
	service, err := NewService(repository, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return rentalFixture{pool: pool, repository: repository, service: service, userID: userID, configID: configID, planID: planID}
}

func (f rentalFixture) addNumber(t *testing.T, suffix string) string {
	t.Helper()
	var id string
	phoneNumber := "+1415555" + fmt.Sprintf("%04d", fixturePhoneSequence.Add(1)%10000)
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, sms_enabled)
		 VALUES ($1::uuid, $2, $3, 'Local', true) RETURNING id::text`,
		f.configID, "provider-ref-"+suffix, phoneNumber,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRepositoryReserveCreatesPendingOrderWithServerPrice(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	numberID := f.addNumber(t, "successful")
	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "successful-checkout",
	})
	if err != nil {
		t.Fatal(err)
	}
	order := reservation.Order
	if order.ID == "" || order.RentalID == "" || order.UserID != f.userID || order.ProviderNumberID != numberID ||
		order.RentalPlanID != f.planID || order.Status != "PENDING" || order.PriceMinorUnits != 1299 || order.Currency != "USD" ||
		order.PlanCode != "one-hour" || order.PlanName != "One hour" || order.DurationSeconds != 3600 {
		t.Fatalf("reservation/order = %+v", order)
	}
	var numberStatus string
	var rentalNumberID, orderStatus string
	var reservationExpiresAt time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT pn.status, r.provider_number_id::text, r.reservation_expires_at, o.order_status
		 FROM provider_numbers pn JOIN rentals r ON r.provider_number_id = pn.id JOIN orders o ON o.id = r.order_id
		 WHERE o.id = $1::uuid`, order.ID,
	).Scan(&numberStatus, &rentalNumberID, &reservationExpiresAt, &orderStatus); err != nil {
		t.Fatal(err)
	}
	if numberStatus != "RESERVED" || rentalNumberID != numberID || orderStatus != "PENDING" || !reservationExpiresAt.After(time.Now()) {
		t.Fatalf("persisted reservation state=%s number=%s order=%s expiry=%s", numberStatus, rentalNumberID, orderStatus, reservationExpiresAt)
	}

	if _, err := f.pool.Exec(context.Background(), `UPDATE rental_plans SET price_minor_units = 5000 WHERE id = $1::uuid`, f.planID); err != nil {
		t.Fatal(err)
	}
	loaded, err := f.service.GetOrder(context.Background(), f.userID, order.ID)
	if err != nil || loaded.PriceMinorUnits != 1299 || loaded.Currency != "USD" {
		t.Fatalf("order pricing snapshot changed after plan update: %+v, %v", loaded, err)
	}
	_, err = f.pool.Exec(context.Background(), `UPDATE orders SET price_minor_units_snapshot = 1 WHERE id = $1::uuid`, order.ID)
	assertRentalPGError(t, err, "23514", "orders_immutable")
}

func TestRepositoryRejectsUnavailableNumberAndMissingPlan(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	firstNumber := f.addNumber(t, "first")
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: firstNumber, RentalPlanID: f.planID, IdempotencyKey: "first-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: firstNumber, RentalPlanID: f.planID, IdempotencyKey: "second-key",
	}); !errors.Is(err, ErrNumberUnavailable) {
		t.Fatalf("second reservation error = %v", err)
	}
	secondNumber := f.addNumber(t, "second")
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: secondNumber, RentalPlanID: "00000000-0000-0000-0000-000000000000", IdempotencyKey: "missing-plan",
	}); !errors.Is(err, ErrPlanUnavailable) {
		t.Fatalf("missing plan error = %v", err)
	}
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM provider_numbers WHERE id = $1::uuid`, secondNumber).Scan(&status); err != nil || status != "AVAILABLE" {
		t.Fatalf("number with invalid plan status=%q err=%v", status, err)
	}
}

func TestRepositoryIdempotencyAndOrderOwnership(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	numberID := f.addNumber(t, "idempotent")
	request := ReserveRequest{ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "same-request"}
	first, err := f.service.Reserve(context.Background(), f.userID, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.service.Reserve(context.Background(), f.userID, request)
	if err != nil || retry.Order.ID != first.Order.ID || retry.Order.RentalID != first.Order.RentalID {
		t.Fatalf("idempotent retry = %+v, %v; first = %+v", retry, err, first)
	}
	otherNumber := f.addNumber(t, "other")
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: otherNumber, RentalPlanID: f.planID, IdempotencyKey: request.IdempotencyKey,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reused key with changed selection error = %v", err)
	}
	if _, err := f.service.GetOrder(context.Background(), "00000000-0000-0000-0000-000000000001", first.Order.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user order lookup error = %v", err)
	}
	var orders, rentals int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders WHERE user_id = $1::uuid`, f.userID).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM rentals WHERE order_id IN (SELECT id FROM orders WHERE user_id = $1::uuid)`, f.userID).Scan(&rentals); err != nil {
		t.Fatal(err)
	}
	if orders != 1 || rentals != 1 {
		t.Fatalf("idempotency created orders=%d rentals=%d", orders, rentals)
	}
}

func TestRepositoryReservationFailureRollsBackAllWrites(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	numberID := f.addNumber(t, "rollback")
	now := time.Now().UTC()
	_, err := f.repository.Reserve(context.Background(), ReserveInput{
		UserID: f.userID, ProviderNumberID: numberID, RentalPlanID: f.planID,
		IdempotencyKey: "rollback-key", ReservedAt: now, ReservationExpiresAt: now,
	})
	if err == nil {
		t.Fatal("invalid reservation expiry must fail")
	}
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM provider_numbers WHERE id = $1::uuid`, numberID).Scan(&status); err != nil || status != "AVAILABLE" {
		t.Fatalf("reservation rollback number status=%q err=%v", status, err)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders WHERE user_id = $1::uuid`, f.userID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("reservation rollback left %d orders (err=%v)", count, err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM rentals`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("reservation rollback left %d rentals (err=%v)", count, err)
	}
}

func TestRepositoryConstraintsRejectInvalidReservationAndDuplicateIdempotency(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	numberID := f.addNumber(t, "constraint")
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "constraint-key",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, price_minor_units_snapshot, currency_snapshot, idempotency_key, order_status)
		 VALUES ($1::uuid, $2::uuid, 'one-hour', 'One hour', 3600, 1299, 'USD', 'constraint-key', 'PENDING')`, f.userID, f.planID,
	)
	assertRentalPGError(t, err, "23505", "orders_user_idempotency_key")
	_, err = f.pool.Exec(context.Background(), `UPDATE orders SET order_status = 'PAID' WHERE idempotency_key = 'constraint-key'`)
	assertRentalPGError(t, err, "23514", "orders_status_transition")
	_, err = f.pool.Exec(context.Background(),
		`UPDATE rentals SET reservation_expires_at = reserved_at WHERE order_id = (SELECT id FROM orders WHERE idempotency_key = 'constraint-key')`,
	)
	assertRentalPGError(t, err, "23514", "rentals_reservation_expiry_check")
}

func TestRepositoryExpiresOnlyUnpaidReservations(t *testing.T) {
	f := newRentalFixture(t, 25*time.Millisecond)
	numberID := f.addNumber(t, "expires")
	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "expiring-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ExpireReservation(context.Background(), reservation.Order.RentalID); !errors.Is(err, ErrReservationNotExpired) {
		t.Fatalf("early expiration error = %v", err)
	}
	changed, err := f.repository.ExpireReservation(context.Background(), reservation.Order.RentalID, time.Now().Add(time.Hour))
	if err != nil || !changed {
		t.Fatalf("expiration = changed %t, err %v", changed, err)
	}
	var numberStatus, orderStatus string
	var endedAt *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT pn.status, o.order_status, r.ended_at
		 FROM provider_numbers pn JOIN rentals r ON r.provider_number_id = pn.id JOIN orders o ON o.id = r.order_id
		 WHERE r.id = $1::uuid`, reservation.Order.RentalID,
	).Scan(&numberStatus, &orderStatus, &endedAt); err != nil {
		t.Fatal(err)
	}
	if numberStatus != "AVAILABLE" || orderStatus != "EXPIRED" || endedAt == nil {
		t.Fatalf("expiration did not atomically release reservation: number=%s order=%s ended=%v", numberStatus, orderStatus, endedAt)
	}
	changed, err = f.repository.ExpireReservation(context.Background(), reservation.Order.RentalID, time.Now().Add(2*time.Hour))
	if err != nil || changed {
		t.Fatalf("idempotent expiration = %t, %v", changed, err)
	}
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "fresh-after-expiration",
	}); err != nil {
		t.Fatalf("number should be reservable after unpaid reservation expires: %v", err)
	}
}

func TestRepositoryDoesNotExpireActivatedRental(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	numberID := f.addNumber(t, "active")
	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "active-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'ACTIVE' WHERE id = $1::uuid`, numberID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE rentals SET activated_at = now(), expires_at = now() + interval '1 hour' WHERE id = $1::uuid`, reservation.Order.RentalID,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repository.ExpireReservation(ctx, reservation.Order.RentalID, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrNotReservation) {
		t.Fatalf("active rental expiration error = %v", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE orders SET order_status = 'EXPIRED' WHERE id = $1::uuid`, reservation.Order.ID)
	assertRentalPGError(t, err, "23514", "orders_status_transition")
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM provider_numbers WHERE id = $1::uuid`, numberID).Scan(&status); err != nil || status != "ACTIVE" {
		t.Fatalf("active number state changed to %q (err=%v)", status, err)
	}
}

func TestRepositoryRejectsDuplicateOpenRentalConstraint(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	numberID := f.addNumber(t, "duplicate-open")
	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "open-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(),
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, price_minor_units_snapshot, currency_snapshot, idempotency_key, order_status)
		 VALUES ($1::uuid, $2::uuid, 'one-hour', 'One hour', 3600, 1299, 'USD', 'open-two', 'PENDING')`, f.userID, f.planID,
	)
	if err != nil {
		t.Fatal(err)
	}
	var secondOrderID string
	if err := tx.QueryRow(context.Background(), `SELECT id::text FROM orders WHERE idempotency_key = 'open-two'`).Scan(&secondOrderID); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(),
		`INSERT INTO rentals (order_id, user_id, provider_number_id, reserved_at, reservation_expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, now(), now() + interval '1 hour')`, secondOrderID, f.userID, numberID,
	)
	if err == nil {
		err = tx.Commit(context.Background())
	}
	if err == nil {
		t.Fatal("duplicate open rental for one provider number must fail")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "rentals_one_open_per_provider_number_key" {
		t.Fatalf("duplicate open rental error = %v", err)
	}
	if reservation.Order.Status != "PENDING" {
		t.Fatalf("first order status = %s", reservation.Order.Status)
	}
}

func TestDeterministicConcurrentReservationWaitsForLockedNumber(t *testing.T) {
	f := newRentalFixture(t, time.Hour)
	numberID := f.addNumber(t, "concurrent")
	holder, orderID := f.reserveUnderHeldLock(t, numberID)
	result := make(chan error, 1)
	go func() {
		_, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
			ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "concurrent-contender",
		})
		result <- err
	}()

	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		var waiting bool
		if err := f.pool.QueryRow(context.Background(),
			`SELECT EXISTS (
			 SELECT 1 FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock'
			   AND query LIKE '%FROM provider_numbers WHERE id = $1::uuid FOR UPDATE%'
		 )`,
		).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("competing reservation returned without waiting on provider-number row lock: %v", err)
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("competing reservation never waited on PostgreSQL lock")
		}
	}
	if err := holder.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrNumberUnavailable) {
			t.Fatalf("competing reservation after lock release error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("competing reservation did not finish after holder committed")
	}
	var orderCount, rentalCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders WHERE idempotency_key IN ('held-winner', 'concurrent-contender')`).Scan(&orderCount); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM rentals WHERE order_id = $1::uuid`, orderID).Scan(&rentalCount); err != nil {
		t.Fatal(err)
	}
	if orderCount != 1 || rentalCount != 1 {
		t.Fatalf("orders=%d rentals=%d after contention", orderCount, rentalCount)
	}
}

func (f rentalFixture) reserveUnderHeldLock(t *testing.T, providerNumberID string) (pgx.Tx, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lockedNumberID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM provider_numbers WHERE id = $1::uuid FOR UPDATE`, providerNumberID).Scan(&lockedNumberID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'RESERVED' WHERE id = $1::uuid`, providerNumberID); err != nil {
		t.Fatal(err)
	}
	var orderID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, price_minor_units_snapshot, currency_snapshot, idempotency_key, order_status)
		 VALUES ($1::uuid, $2::uuid, 'one-hour', 'One hour', 3600, 1299, 'USD', 'held-winner', 'PENDING') RETURNING id::text`,
		f.userID, f.planID,
	).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id, reserved_at, reservation_expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, now(), now() + interval '1 hour')`, orderID, f.userID, providerNumberID,
	); err != nil {
		t.Fatal(err)
	}
	return tx, orderID
}

func assertRentalPGError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || constraint != "" && pgErr.ConstraintName != constraint {
		t.Fatalf("PostgreSQL error = %v; want %s/%s", err, code, constraint)
	}
}
