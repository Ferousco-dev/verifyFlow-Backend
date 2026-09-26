package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/testutil/dbtest"
)

type domainFixture struct {
	pool              *pgxpool.Pool
	userID            string
	telephonyConfigID string
	paymentConfigID   string
	planID            string
}

type domainExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func newDomainFixture(t *testing.T) domainFixture {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email) VALUES ('Domain Test', 'domain-test', 'domain@example.com') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	telephonyConfigID := insertProviderConfig(t, pool, "telephony", "twilio", "primary-telephony")
	paymentConfigID := insertProviderConfig(t, pool, "payment", "test-provider", "primary-payment")
	var planID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency)
		 VALUES ('hourly', 'One hour', 3600, 500, 'USD') RETURNING id::text`,
	).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	return domainFixture{pool: pool, userID: userID, telephonyConfigID: telephonyConfigID, paymentConfigID: paymentConfigID, planID: planID}
}

func insertProviderConfig(t *testing.T, pool *pgxpool.Pool, kind, key, name string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ($1, $2, $3, $4, 'test-key-v1') RETURNING id::text`,
		kind, key, name, []byte{1, 2, 3, 4},
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f domainFixture) insertOrder(t *testing.T) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, price_minor_units_snapshot, currency_snapshot)
		 VALUES ($1::uuid, $2::uuid, 'hourly', 'One hour', 3600, 500, 'USD') RETURNING id::text`,
		f.userID, f.planID,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f domainFixture) insertNumber(t *testing.T, reference, phoneNumber string) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type)
		 VALUES ($1::uuid, $2, $3, 'Local') RETURNING id::text`,
		f.telephonyConfigID, reference, phoneNumber,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertRental(ctx context.Context, execer domainExecer, orderID, userID, providerNumberID string) error {
	_, err := execer.Exec(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id) VALUES ($1::uuid, $2::uuid, $3::uuid)`,
		orderID, userID, providerNumberID,
	)
	return err
}

func reserveRental(t *testing.T, f domainFixture, orderID, providerNumberID string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'RESERVED' WHERE id = $1::uuid`, providerNumberID); err != nil {
		t.Fatal(err)
	}
	if err := insertRental(ctx, tx, orderID, f.userID, providerNumberID); err != nil {
		t.Fatal(err)
	}
	var rentalID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM rentals WHERE order_id = $1::uuid`, orderID).Scan(&rentalID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return rentalID
}

func activateRental(t *testing.T, f domainFixture, rentalID, providerNumberID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'ACTIVE' WHERE id = $1::uuid`, providerNumberID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE rentals SET activated_at = now(), expires_at = now() + interval '1 hour' WHERE id = $1::uuid`, rentalID,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func expireRental(t *testing.T, f domainFixture, rentalID, providerNumberID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'EXPIRED' WHERE id = $1::uuid`, providerNumberID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE rentals SET ended_at = now() WHERE id = $1::uuid`, rentalID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func cancelReservedRental(t *testing.T, f domainFixture, rentalID, providerNumberID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE provider_numbers SET status = 'AVAILABLE' WHERE id = $1::uuid`, providerNumberID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE rentals SET ended_at = now() WHERE id = $1::uuid`, rentalID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertPostgresError(t *testing.T, err error, sqlState, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected PostgreSQL error %s/%s, got %v", sqlState, constraint, err)
	}
	if pgErr.Code != sqlState || constraint != "" && pgErr.ConstraintName != constraint {
		t.Fatalf("PostgreSQL error = %s/%s, want %s/%s (%s)", pgErr.Code, pgErr.ConstraintName, sqlState, constraint, pgErr.Message)
	}
}

func TestDomainSchemaCreatesTablesAndEncryptedCredentialColumns(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	for _, table := range []string{
		"provider_configs", "provider_numbers", "rental_plans", "rentals",
		"orders", "payments", "inbound_messages", "provider_webhook_events",
	} {
		var name *string
		if err := f.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&name); err != nil || name == nil {
			t.Fatalf("table %s missing (err=%v)", table, err)
		}
	}
	var dataType string
	if err := f.pool.QueryRow(ctx,
		`SELECT data_type FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = 'provider_configs' AND column_name = 'credentials_ciphertext'`,
	).Scan(&dataType); err != nil || dataType != "bytea" {
		t.Fatalf("credential ciphertext type = %q (err=%v), want bytea", dataType, err)
	}
	var plaintextColumns int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = 'provider_configs'
		 AND column_name IN ('credentials', 'auth_token', 'account_sid')`,
	).Scan(&plaintextColumns); err != nil || plaintextColumns != 0 {
		t.Fatalf("provider config has %d plaintext credential columns (err=%v)", plaintextColumns, err)
	}
	_, err := f.pool.Exec(ctx,
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ('telephony', 'twilio', 'empty-ciphertext', ''::bytea, 'key-v1')`,
	)
	assertPostgresError(t, err, "23514", "provider_configs_ciphertext_check")
}

func TestDomainSchemaEnforcesIntegerMoneyAndImmutableOrderSnapshot(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency)
		 VALUES ('negative', 'Negative', 3600, -1, 'USD')`,
	)
	assertPostgresError(t, err, "23514", "rental_plans_price_check")
	_, err = f.pool.Exec(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency)
		 VALUES ('bad-currency', 'Bad currency', 3600, 500, 'usd')`,
	)
	assertPostgresError(t, err, "23514", "rental_plans_currency_check")

	orderID := f.insertOrder(t)
	_, err = f.pool.Exec(ctx, `UPDATE orders SET price_minor_units_snapshot = 1 WHERE id = $1::uuid`, orderID)
	assertPostgresError(t, err, "23514", "orders_immutable")
	_, err = f.pool.Exec(ctx, `DELETE FROM orders WHERE id = $1::uuid`, orderID)
	assertPostgresError(t, err, "23514", "orders_immutable")

	_, err = f.pool.Exec(ctx,
		`INSERT INTO payments (order_id, provider_config_id, amount_minor_units, currency)
		 VALUES ($1::uuid, $2::uuid, -1, 'USD')`, orderID, f.paymentConfigID,
	)
	assertPostgresError(t, err, "23514", "payments_amount_check")
	_, err = f.pool.Exec(ctx,
		`INSERT INTO payments (order_id, provider_config_id, amount_minor_units, currency)
		 VALUES ($1::uuid, $2::uuid, 500, 'usd')`, orderID, f.paymentConfigID,
	)
	assertPostgresError(t, err, "23514", "payments_currency_check")

	for _, column := range []struct{ table, name string }{
		{"rental_plans", "price_minor_units"},
		{"orders", "price_minor_units_snapshot"},
		{"payments", "amount_minor_units"},
	} {
		var got string
		if err := f.pool.QueryRow(ctx,
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, column.table, column.name,
		).Scan(&got); err != nil || got != "bigint" {
			t.Fatalf("%s.%s type = %q (err=%v), want bigint minor units", column.table, column.name, got, err)
		}
	}

	for _, reference := range []string{"attempt-1", "attempt-2"} {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO payments (order_id, provider_config_id, provider_reference, provider_status, amount_minor_units, currency)
			 VALUES ($1::uuid, $2::uuid, $3, 'opaque-provider-state', 500, 'USD')`, orderID, f.paymentConfigID, reference,
		); err != nil {
			t.Fatalf("multiple attempts per order must be allowed: %v", err)
		}
	}
}

func TestProviderNumberLifecycleConstraints(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx,
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, status)
		 VALUES ($1::uuid, 'invalid-initial-state', '+14155550100', 'Local', 'EXPIRED')`, f.telephonyConfigID,
	)
	assertPostgresError(t, err, "23514", "provider_numbers_status_transition")

	numberID := f.insertNumber(t, "number-ref", "+14155550100")
	firstRentalID := reserveRental(t, f, f.insertOrder(t), numberID)
	activateRental(t, f, firstRentalID, numberID)
	expireRental(t, f, firstRentalID, numberID)
	setStatus := func(status string) error {
		_, err := f.pool.Exec(ctx, `UPDATE provider_numbers SET status = $2 WHERE id = $1::uuid`, numberID, status)
		return err
	}
	assertPostgresError(t, setStatus("AVAILABLE"), "23514", "provider_numbers_status_transition")
	if err := setStatus("QUARANTINED"); err != nil {
		t.Fatal(err)
	}
	if err := setStatus("REVIEW"); err != nil {
		t.Fatal(err)
	}
	assertPostgresError(t, setStatus("AVAILABLE"), "23514", "provider_numbers_status_transition")
	if err := setStatus("REUSABLE"); err != nil {
		t.Fatal(err)
	}
	if err := setStatus("AVAILABLE"); err != nil {
		t.Fatal(err)
	}
	secondRentalID := reserveRental(t, f, f.insertOrder(t), numberID)
	cancelReservedRental(t, f, secondRentalID, numberID)
	thirdRentalID := reserveRental(t, f, f.insertOrder(t), numberID)
	activateRental(t, f, thirdRentalID, numberID)
	expireRental(t, f, thirdRentalID, numberID)
	for _, status := range []string{"QUARANTINED", "REVIEW", "RETIRED"} {
		if err := setStatus(status); err != nil {
			t.Fatalf("transition to %s: %v", status, err)
		}
	}
	assertPostgresError(t, setStatus("AVAILABLE"), "23514", "provider_numbers_status_transition")
}

func TestDomainSchemaEnforcesRentalOwnershipAndWebhookIdempotency(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	orderID := f.insertOrder(t)
	numberID := f.insertNumber(t, "number-ref", "+14155550100")
	rentalID := reserveRental(t, f, orderID, numberID)
	_, err := f.pool.Exec(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id) VALUES ($1::uuid, $2::uuid, $3::uuid)`,
		orderID, f.userID, numberID,
	)
	assertPostgresError(t, err, "23505", "rentals_order_id_key")

	_, err = f.pool.Exec(ctx,
		`INSERT INTO inbound_messages (rental_id, provider_number_id, provider_reference, from_number, to_number, body)
		 VALUES ($1::uuid, $2::uuid, 'SM-inbound-1', '+14155550101', '+14155550100', 'hello')`, rentalID, numberID,
	)
	if err != nil {
		t.Fatalf("insert inbound message: %v", err)
	}
	_, err = f.pool.Exec(ctx,
		`INSERT INTO inbound_messages (rental_id, provider_number_id, provider_reference, from_number, to_number, body)
		 VALUES ($1::uuid, $2::uuid, 'SM-inbound-1', '+14155550101', '+14155550100', 'duplicate')`, rentalID, numberID,
	)
	assertPostgresError(t, err, "23505", "inbound_messages_rental_provider_ref_key")

	_, err = f.pool.Exec(ctx,
		`INSERT INTO provider_webhook_events (provider_config_id, provider_kind, idempotency_key, provider_reference, event_type, payload)
		 VALUES ($1::uuid, 'telephony', 'message:SM-inbound-1', 'SM-inbound-1', 'sms.inbound', '{"body":"hello"}')`, f.telephonyConfigID,
	)
	if err != nil {
		t.Fatalf("insert webhook event: %v", err)
	}
	_, err = f.pool.Exec(ctx,
		`INSERT INTO provider_webhook_events (provider_config_id, provider_kind, idempotency_key, event_type)
		 VALUES ($1::uuid, 'telephony', 'message:SM-inbound-1', 'sms.inbound')`, f.telephonyConfigID,
	)
	assertPostgresError(t, err, "23505", "provider_webhook_events_idempotency_key")
	_, err = f.pool.Exec(ctx,
		`INSERT INTO provider_webhook_events (provider_config_id, provider_kind, idempotency_key, event_type, payload)
		 VALUES ($1::uuid, 'telephony', 'bad-payload', 'sms.inbound', '[]')`, f.telephonyConfigID,
	)
	assertPostgresError(t, err, "23514", "provider_webhook_events_payload_check")

	var otherUserID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email) VALUES ('Other User', 'other-domain-user', 'other-domain@example.com') RETURNING id::text`,
	).Scan(&otherUserID); err != nil {
		t.Fatal(err)
	}
	otherOrderID := f.insertOrder(t)
	otherNumberID := f.insertNumber(t, "other-number-ref", "+14155550102")
	_, err = f.pool.Exec(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id) VALUES ($1::uuid, $2::uuid, $3::uuid)`,
		otherOrderID, otherUserID, otherNumberID,
	)
	if err == nil {
		t.Fatal("order ownership must match rental ownership")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != "rentals_order_user_fkey" {
		t.Fatalf("cross-user rental error = %v", err)
	}
}

func TestConcurrentOpenRentalsForProviderNumber(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	numberID := f.insertNumber(t, "number-ref", "+14155550100")
	firstOrderID := f.insertOrder(t)
	secondOrderID := f.insertOrder(t)

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `UPDATE provider_numbers SET status = 'RESERVED' WHERE id = $1::uuid`, numberID); err != nil {
		t.Fatal(err)
	}
	if err := insertRental(ctx, holder, firstOrderID, f.userID, numberID); err != nil {
		t.Fatal(err)
	}

	competitor, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backendPID := competitor.Conn().PgConn().PID()
	secondInsert := make(chan error, 1)
	go func() {
		defer competitor.Release()
		secondInsert <- insertRental(ctx, competitor, secondOrderID, f.userID, numberID)
	}()

	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		var waitEventType *string
		if err := f.pool.QueryRow(ctx,
			`SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, int32(backendPID),
		).Scan(&waitEventType); err != nil {
			t.Fatal(err)
		}
		if waitEventType != nil && *waitEventType == "Lock" {
			break
		}
		select {
		case err := <-secondInsert:
			t.Fatalf("competing insert returned before waiting on the unique index: %v", err)
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("competing insert never waited on the unique index")
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondInsert:
		assertPostgresError(t, err, "23505", "rentals_one_open_per_provider_number_key")
	case <-time.After(5 * time.Second):
		t.Fatal("competing rental insert did not finish after the first transaction committed")
	}
}

func TestDomainSchemaCurrencyAndStatusNamesAreConstrained(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	numberID := f.insertNumber(t, "number-ref", "+14155550100")
	_, err := f.pool.Exec(ctx, `UPDATE provider_numbers SET status = 'EXPIRED' WHERE id = $1::uuid`, numberID)
	assertPostgresError(t, err, "23514", "provider_numbers_status_transition")

	_, err = f.pool.Exec(ctx,
		`INSERT INTO payments (order_id, provider_config_id, provider_reference, amount_minor_units, currency)
		 VALUES ($1::uuid, $2::uuid, 'wrong-kind', 500, 'USD')`, f.insertOrder(t), f.telephonyConfigID,
	)
	assertPostgresError(t, err, "23503", "payments_config_kind_fkey")

	_, err = f.pool.Exec(ctx,
		`INSERT INTO provider_webhook_events (provider_config_id, provider_kind, idempotency_key, event_type)
		 VALUES ($1::uuid, 'payment', 'wrong-kind', 'payment.event')`, f.telephonyConfigID,
	)
	assertPostgresError(t, err, "23503", "provider_webhook_events_config_kind_fkey")
}

func TestDomainSchemaUniqueWebhookKeyScopesToConfiguration(t *testing.T) {
	f := newDomainFixture(t)
	ctx := context.Background()
	for _, configID := range []string{f.telephonyConfigID, f.paymentConfigID} {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO provider_webhook_events (provider_config_id, provider_kind, idempotency_key, event_type)
			 VALUES ($1::uuid, $2, 'same-key', 'event')`, configID,
			map[string]string{f.telephonyConfigID: "telephony", f.paymentConfigID: "payment"}[configID],
		); err != nil {
			t.Fatalf("same idempotency key on a different provider config should be allowed: %v", err)
		}
	}
}
