package payment

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const attemptColumns = `id::text, order_id::text, provider_config_id::text, provider_reference,
	provider_status, amount_minor_units, currency, created_at`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	var reference *string
	err := row.Scan(&a.ID, &a.OrderID, &a.ProviderConfigID, &reference, &a.ProviderStatus, &a.AmountMinorUnits, &a.Currency, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if reference != nil {
		a.ProviderReference = *reference
	}
	return a, err
}

func (r *Repository) CreateAttempt(ctx context.Context, orderID, providerConfigID string, amountMinorUnits int64, currency string) (Attempt, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO payments (order_id, provider_config_id, provider_reference, provider_status, amount_minor_units, currency)
		 VALUES ($1::uuid, $2::uuid, NULL, 'initializing', $3, $4)
		 RETURNING `+attemptColumns,
		orderID, providerConfigID, amountMinorUnits, currency,
	)
	return scanAttempt(row)
}

func (r *Repository) SetReference(ctx context.Context, id, reference, status string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE payments SET provider_reference = $2, provider_status = $3 WHERE id = $1::uuid`, id, reference, status)
	return err
}

func (r *Repository) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE payments SET provider_status = $2 WHERE id = $1::uuid`, id, status)
	return err
}

func (r *Repository) GetLatestForOrder(ctx context.Context, orderID string) (Attempt, error) {
	return scanAttempt(r.pool.QueryRow(ctx,
		`SELECT `+attemptColumns+` FROM payments WHERE order_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, orderID))
}

func (r *Repository) GetByReference(ctx context.Context, providerConfigID, reference string) (Attempt, error) {
	return scanAttempt(r.pool.QueryRow(ctx,
		`SELECT `+attemptColumns+` FROM payments WHERE provider_config_id = $1::uuid AND provider_reference = $2`,
		providerConfigID, reference))
}

func (r *Repository) HasSuccessfulPayment(ctx context.Context, orderID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM payments WHERE order_id = $1::uuid AND provider_status = 'success')`, orderID,
	).Scan(&exists)
	return exists, err
}

func (r *Repository) RecordWebhookEvent(ctx context.Context, providerConfigID, idempotencyKey, providerReference, eventType string, payload []byte) (bool, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`INSERT INTO provider_webhook_events (provider_config_id, provider_kind, idempotency_key, provider_reference, event_type, payload)
		 VALUES ($1::uuid, 'payment', $2, $3, $4, $5::jsonb)
		 ON CONFLICT (provider_config_id, idempotency_key) DO NOTHING
		 RETURNING id::text`,
		providerConfigID, idempotencyKey, providerReference, eventType, payload,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *Repository) MarkWebhookProcessed(ctx context.Context, providerConfigID, idempotencyKey string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE provider_webhook_events SET processed_at = now()
		  WHERE provider_config_id = $1::uuid AND idempotency_key = $2`, providerConfigID, idempotencyKey)
	return err
}
