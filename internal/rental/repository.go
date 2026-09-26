package rental

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) SearchNumbers(ctx context.Context, filter NumberFilter) (NumbersPage, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id::text, phone_number, number_type, sms_enabled, mms_enabled, voice_enabled
		   FROM provider_numbers
		  WHERE status = 'AVAILABLE'
		    AND ($1 = '' OR number_type = $1)
		    AND (NOT $2 OR sms_enabled)
		    AND (NOT $3 OR mms_enabled)
		    AND (NOT $4 OR voice_enabled)
		    AND ($5 = '' OR id > $5::uuid)
		  ORDER BY id
		  LIMIT $6`,
		filter.NumberType, filter.RequireSMS, filter.RequireMMS, filter.RequireVoice, filter.Cursor, filter.Limit+1,
	)
	if err != nil {
		return NumbersPage{}, err
	}
	defer rows.Close()

	var page NumbersPage
	for rows.Next() {
		var n NumberSummary
		if err := rows.Scan(&n.ID, &n.PhoneNumber, &n.NumberType, &n.SMSEnabled, &n.MMSEnabled, &n.VoiceEnabled); err != nil {
			return NumbersPage{}, err
		}
		page.Numbers = append(page.Numbers, n)
	}
	if err := rows.Err(); err != nil {
		return NumbersPage{}, err
	}
	if len(page.Numbers) > filter.Limit {
		page.NextCursor = page.Numbers[filter.Limit-1].ID
		page.Numbers = page.Numbers[:filter.Limit]
	}
	return page, nil
}

func (r *Repository) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id::text, plan_code, name, duration_seconds, price_minor_units, currency
		   FROM rental_plans WHERE is_active ORDER BY plan_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var plans []Plan
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.DurationSeconds, &p.PriceMinorUnits, &p.Currency); err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

func (r *Repository) Reserve(ctx context.Context, input ReserveInput) (Reservation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize retries for the same user/key before checking the existing order.
	lockKey := input.UserID + ":" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return Reservation{}, err
	}
	if existing, err := scanOrder(tx.QueryRow(ctx, orderSelect+`
		WHERE o.user_id = $1::uuid AND o.idempotency_key = $2`, input.UserID, input.IdempotencyKey)); err == nil {
		if existing.ProviderNumberID != input.ProviderNumberID || existing.RentalPlanID != input.RentalPlanID {
			return Reservation{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Reservation{}, err
		}
		return Reservation{Order: existing}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Reservation{}, err
	}

	var active bool
	if err := tx.QueryRow(ctx, `SELECT is_active FROM users WHERE id = $1::uuid FOR SHARE`, input.UserID).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Reservation{}, ErrNotFound
		}
		return Reservation{}, err
	}
	if !active {
		return Reservation{}, ErrNotFound
	}

	var numberStatus string
	err = tx.QueryRow(ctx,
		`SELECT status FROM provider_numbers WHERE id = $1::uuid FOR UPDATE`, input.ProviderNumberID,
	).Scan(&numberStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrNumberNotFound
	}
	if err != nil {
		return Reservation{}, err
	}
	if numberStatus != "AVAILABLE" {
		return Reservation{}, ErrNumberUnavailable
	}

	var plan struct {
		ID              string
		Code            string
		Name            string
		DurationSeconds int32
		PriceMinorUnits int64
		Currency        string
	}
	err = tx.QueryRow(ctx,
		`SELECT id::text, plan_code, name, duration_seconds, price_minor_units, currency
		   FROM rental_plans WHERE id = $1::uuid AND is_active FOR SHARE`, input.RentalPlanID,
	).Scan(&plan.ID, &plan.Code, &plan.Name, &plan.DurationSeconds, &plan.PriceMinorUnits, &plan.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrPlanUnavailable
	}
	if err != nil {
		return Reservation{}, err
	}

	var orderID string
	err = tx.QueryRow(ctx,
		`INSERT INTO orders (user_id, rental_plan_id, plan_code_snapshot, plan_name_snapshot,
		 duration_seconds_snapshot, price_minor_units_snapshot, currency_snapshot, idempotency_key, order_status)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, 'PENDING') RETURNING id::text`,
		input.UserID, plan.ID, plan.Code, plan.Name, plan.DurationSeconds, plan.PriceMinorUnits, plan.Currency, input.IdempotencyKey,
	).Scan(&orderID)
	if err != nil {
		return Reservation{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE provider_numbers SET status = 'RESERVED', updated_at = $2 WHERE id = $1::uuid`,
		input.ProviderNumberID, input.ReservedAt,
	); err != nil {
		return Reservation{}, err
	}
	var rentalID string
	err = tx.QueryRow(ctx,
		`INSERT INTO rentals (order_id, user_id, provider_number_id, reserved_at, reservation_expires_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5) RETURNING id::text`,
		orderID, input.UserID, input.ProviderNumberID, input.ReservedAt, input.ReservationExpiresAt,
	).Scan(&rentalID)
	if err != nil {
		return Reservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, err
	}

	return r.GetReservation(ctx, input.UserID, orderID, rentalID)
}

const orderSelect = `SELECT o.id::text, o.user_id::text, r.id::text, r.provider_number_id::text,
	o.rental_plan_id::text, o.order_status, o.plan_code_snapshot, o.plan_name_snapshot,
	o.duration_seconds_snapshot, o.price_minor_units_snapshot, o.currency_snapshot,
	r.reservation_expires_at, o.created_at
	FROM orders o JOIN rentals r ON r.order_id = o.id `

func scanOrder(row pgx.Row) (Order, error) {
	var order Order
	err := row.Scan(&order.ID, &order.UserID, &order.RentalID, &order.ProviderNumberID,
		&order.RentalPlanID, &order.Status, &order.PlanCode, &order.PlanName,
		&order.DurationSeconds, &order.PriceMinorUnits, &order.Currency,
		&order.ReservationExpiresAt, &order.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return order, err
}

func (r *Repository) GetOrder(ctx context.Context, userID, orderID string) (Order, error) {
	return scanOrder(r.pool.QueryRow(ctx, orderSelect+`WHERE o.user_id = $1::uuid AND o.id = $2::uuid`, userID, orderID))
}

func (r *Repository) GetOrderByID(ctx context.Context, orderID string) (Order, error) {
	return scanOrder(r.pool.QueryRow(ctx, orderSelect+`WHERE o.id = $1::uuid`, orderID))
}

func (r *Repository) GetReservation(ctx context.Context, userID, orderID, rentalID string) (Reservation, error) {
	order, err := scanOrder(r.pool.QueryRow(ctx,
		orderSelect+`WHERE o.user_id = $1::uuid AND o.id = $2::uuid AND r.id = $3::uuid`, userID, orderID, rentalID))
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{Order: order}, nil
}

func (r *Repository) GetFulfillmentSnapshot(ctx context.Context, orderID string) (FulfillmentSnapshot, error) {
	var s FulfillmentSnapshot
	err := r.pool.QueryRow(ctx,
		`SELECT o.id::text, r.id::text, pn.id::text, pn.provider_config_id::text, pn.phone_number, pn.number_type,
		        pn.sms_enabled, pn.mms_enabled, pn.voice_enabled, o.duration_seconds_snapshot, r.activated_at
		   FROM orders o
		   JOIN rentals r ON r.order_id = o.id
		   JOIN provider_numbers pn ON pn.id = r.provider_number_id
		  WHERE o.id = $1::uuid`, orderID,
	).Scan(&s.OrderID, &s.RentalID, &s.ProviderNumberID, &s.ProviderConfigID, &s.PhoneNumber, &s.NumberType,
		&s.SMSEnabled, &s.MMSEnabled, &s.VoiceEnabled, &s.DurationSeconds, &s.ActivatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return FulfillmentSnapshot{}, ErrNotFound
	}
	return s, err
}

func (r *Repository) SwapToReplacementNumber(ctx context.Context, orderID, failedNumberID, numberType string, sms, mms, voice bool, now time.Time) (string, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rentalID string
	if err := tx.QueryRow(ctx,
		`SELECT id::text FROM rentals WHERE order_id = $1::uuid FOR UPDATE`, orderID,
	).Scan(&rentalID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return "", false, err
	}

	var replacementID string
	err = tx.QueryRow(ctx,
		`SELECT id::text FROM provider_numbers
		  WHERE status = 'AVAILABLE' AND number_type = $1 AND sms_enabled = $2 AND mms_enabled = $3 AND voice_enabled = $4
		    AND id <> $5::uuid
		  ORDER BY id
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1`,
		numberType, sms, mms, voice, failedNumberID,
	).Scan(&replacementID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx,
			`UPDATE orders SET order_status = 'PENDING_FULFILLMENT' WHERE id = $1::uuid AND order_status = 'PENDING'`, orderID,
		); err != nil {
			return "", false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE provider_numbers SET status = 'AVAILABLE', updated_at = $2 WHERE id = $1::uuid`, failedNumberID, now,
	); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE provider_numbers SET status = 'RESERVED', updated_at = $2 WHERE id = $1::uuid`, replacementID, now,
	); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE rentals SET provider_number_id = $2 WHERE id = $1::uuid`, rentalID, replacementID,
	); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return replacementID, true, nil
}

func (r *Repository) ActivateRental(ctx context.Context, orderID, providerNumberID, providerReference string, expiresAt, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE rentals SET activated_at = $2, expires_at = $3
		  WHERE order_id = $1::uuid AND activated_at IS NULL`, orderID, now, expiresAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyActivated
	}
	tag, err = tx.Exec(ctx,
		`UPDATE provider_numbers SET status = 'ACTIVE', provider_reference = $2, updated_at = $3
		  WHERE id = $1::uuid AND status = 'RESERVED'`, providerNumberID, providerReference, now,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNumberUnavailable
	}
	return tx.Commit(ctx)
}

func (r *Repository) ExpireReservation(ctx context.Context, rentalID string, now time.Time) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var providerNumberID string
	err = tx.QueryRow(ctx, `SELECT provider_number_id::text FROM rentals WHERE id = $1::uuid`, rentalID).Scan(&providerNumberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	var numberStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM provider_numbers WHERE id = $1::uuid FOR UPDATE`, providerNumberID,
	).Scan(&numberStatus); err != nil {
		return false, err
	}

	var orderID, orderStatus string
	var activatedAt, endedAt *time.Time
	var expiresAt time.Time
	err = tx.QueryRow(ctx,
		`SELECT order_id::text, order_status, activated_at, ended_at, reservation_expires_at
		   FROM rentals JOIN orders ON orders.id = rentals.order_id
		  WHERE rentals.id = $1::uuid FOR UPDATE OF rentals, orders`, rentalID,
	).Scan(&orderID, &orderStatus, &activatedAt, &endedAt, &expiresAt)
	if err != nil {
		return false, err
	}
	if endedAt != nil && orderStatus == "EXPIRED" && numberStatus == "AVAILABLE" {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if endedAt != nil || activatedAt != nil || orderStatus != "PENDING" {
		return false, ErrNotReservation
	}
	if now.Before(expiresAt) {
		return false, ErrReservationNotExpired
	}
	if numberStatus != "RESERVED" {
		return false, fmt.Errorf("rental: reserved number has unexpected state %q", numberStatus)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE provider_numbers SET status = 'AVAILABLE', updated_at = $2 WHERE id = $1::uuid`, providerNumberID, now,
	); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE rentals SET ended_at = $2 WHERE id = $1::uuid`, rentalID, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET order_status = 'EXPIRED' WHERE id = $1::uuid AND order_status = 'PENDING'`, orderID,
	); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
