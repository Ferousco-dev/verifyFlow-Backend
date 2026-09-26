package payment

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/testutil/dbtest"
)

type paymentFixture struct {
	pool             *pgxpool.Pool
	repo             *Repository
	userID           string
	planID           string
	numberID         string
	providerConfigID string
}

func newPaymentFixture(t *testing.T) paymentFixture {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	ctx := context.Background()

	var userID, configID, planID, numberID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (full_name, username, email) VALUES ('Payer', 'payer', 'payer@example.com') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ('payment', 'paystack', 'primary', decode('010203', 'hex'), 'test-v1') RETURNING id::text`,
	).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency)
		 VALUES ('one-hour', 'One hour', 3600, 1299, 'USD') RETURNING id::text`,
	).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	var telephonyConfigID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ('telephony', 'twilio', 'primary', decode('010203', 'hex'), 'test-v1') RETURNING id::text`,
	).Scan(&telephonyConfigID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, sms_enabled)
		 VALUES ($1::uuid, 'ref-1', '+14155550001', 'Local', true) RETURNING id::text`,
		telephonyConfigID,
	).Scan(&numberID); err != nil {
		t.Fatal(err)
	}
	return paymentFixture{pool: pool, repo: NewRepository(pool), userID: userID, planID: planID, numberID: numberID, providerConfigID: configID}
}

// createOrder inserts an order+rental directly (bypassing the rental
// service, since this package only needs an existing order to hang payments
// off of).
func (f paymentFixture) createOrder(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var orderID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, price_minor_units_snapshot, currency_snapshot, idempotency_key, order_status)
		 VALUES ($1::uuid, $2::uuid, 'one-hour', 'One hour', 3600, 1299, 'USD', $3, 'PENDING') RETURNING id::text`,
		f.userID, f.planID, "order-key-"+f.numberID,
	).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE provider_numbers SET status = 'RESERVED' WHERE id = $1::uuid`, f.numberID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id, reservation_expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, now() + interval '15 minutes')`,
		orderID, f.userID, f.numberID,
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return orderID
}

func TestRepositoryCreateAttemptAndLookups(t *testing.T) {
	f := newPaymentFixture(t)
	orderID := f.createOrder(t)
	ctx := context.Background()

	attempt, err := f.repo.CreateAttempt(ctx, orderID, f.providerConfigID, 1299, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ID == "" || attempt.OrderID != orderID || attempt.ProviderStatus != "initializing" || attempt.ProviderReference != "" {
		t.Fatalf("created attempt = %+v", attempt)
	}

	if err := f.repo.SetReference(ctx, attempt.ID, "paystack-ref-1", "pending"); err != nil {
		t.Fatal(err)
	}
	latest, err := f.repo.GetLatestForOrder(ctx, orderID)
	if err != nil || latest.ProviderReference != "paystack-ref-1" || latest.ProviderStatus != "pending" {
		t.Fatalf("GetLatestForOrder = %+v, %v", latest, err)
	}

	byRef, err := f.repo.GetByReference(ctx, f.providerConfigID, "paystack-ref-1")
	if err != nil || byRef.ID != attempt.ID {
		t.Fatalf("GetByReference = %+v, %v", byRef, err)
	}

	if err := f.repo.UpdateStatus(ctx, attempt.ID, "success"); err != nil {
		t.Fatal(err)
	}
	hasSuccess, err := f.repo.HasSuccessfulPayment(ctx, orderID)
	if err != nil || !hasSuccess {
		t.Fatalf("HasSuccessfulPayment = %v, %v", hasSuccess, err)
	}
}

func TestRepositoryGetLatestForOrderReturnsNotFoundWhenNoAttempts(t *testing.T) {
	f := newPaymentFixture(t)
	orderID := f.createOrder(t)
	if _, err := f.repo.GetLatestForOrder(context.Background(), orderID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestRepositoryOnlyOneSuccessfulPaymentPerOrder(t *testing.T) {
	f := newPaymentFixture(t)
	orderID := f.createOrder(t)
	ctx := context.Background()

	first, err := f.repo.CreateAttempt(ctx, orderID, f.providerConfigID, 1299, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.UpdateStatus(ctx, first.ID, "success"); err != nil {
		t.Fatal(err)
	}

	second, err := f.repo.CreateAttempt(ctx, orderID, f.providerConfigID, 1299, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.UpdateStatus(ctx, second.ID, "success"); err == nil {
		t.Fatal("expected the DB to reject a second successful payment for the same order")
	}
}

func TestRepositoryWebhookEventIdempotency(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()

	isNew, err := f.repo.RecordWebhookEvent(ctx, f.providerConfigID, "evt-1", "ref-1", "charge.success", []byte(`{"event":"charge.success"}`))
	if err != nil || !isNew {
		t.Fatalf("first record: isNew=%v, err=%v", isNew, err)
	}
	isNew, err = f.repo.RecordWebhookEvent(ctx, f.providerConfigID, "evt-1", "ref-1", "charge.success", []byte(`{"event":"charge.success"}`))
	if err != nil || isNew {
		t.Fatalf("redelivery: isNew=%v, err=%v", isNew, err)
	}

	if err := f.repo.MarkWebhookProcessed(ctx, f.providerConfigID, "evt-1"); err != nil {
		t.Fatal(err)
	}
	var processedAt *string
	if err := f.pool.QueryRow(ctx,
		`SELECT processed_at::text FROM provider_webhook_events WHERE provider_config_id = $1::uuid AND idempotency_key = 'evt-1'`,
		f.providerConfigID,
	).Scan(&processedAt); err != nil {
		t.Fatal(err)
	}
	if processedAt == nil {
		t.Fatal("expected processed_at to be set")
	}
}
