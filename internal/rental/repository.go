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

func (r *Repository) GetReservation(ctx context.Context, userID, orderID, rentalID string) (Reservation, error) {
	order, err := scanOrder(r.pool.QueryRow(ctx,
		orderSelect+`WHERE o.user_id = $1::uuid AND o.id = $2::uuid AND r.id = $3::uuid`, userID, orderID, rentalID))
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{Order: order}, nil
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
