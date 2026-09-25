package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ConsumeOutcome int

const (
	ConsumeOK ConsumeOutcome = iota
	ConsumeNotFound
	ConsumeUsed
	ConsumeExpired
	ConsumeInactive
)

type ConsumeResult struct {
	Outcome ConsumeOutcome
	UserID  string
}

// ResetStore persists single-use password reset tokens.
type ResetStore interface {
	// Create stores a token for the user, replacing any earlier unused one.
	Create(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time, ip string, now time.Time) error
	// Valid is a cheap read-only pre-check, so invalid tokens can be rejected
	// before the (expensive) password hash is computed.
	Valid(ctx context.Context, tokenHash []byte, now time.Time) (bool, error)
	// Consume atomically: spends the token, sets the new password, marks the
	// email verified, revokes ALL of the user's refresh sessions and deletes
	// their other reset tokens. Either everything happens or nothing does.
	Consume(ctx context.Context, tokenHash []byte, newPasswordHash string, now time.Time) (ConsumeResult, error)
}

type ResetRepository struct {
	pool *pgxpool.Pool
}

func NewResetRepository(pool *pgxpool.Pool) *ResetRepository { return &ResetRepository{pool: pool} }

func (r *ResetRepository) Create(ctx context.Context, userID string, tokenHash []byte, expiresAt time.Time, ip string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Only one live token per user: a newer request invalidates the older link.
	// Long-expired rows are swept opportunistically.
	if _, err := tx.Exec(ctx,
		`DELETE FROM password_reset_tokens
		  WHERE (user_id = $1::uuid AND used_at IS NULL) OR expires_at < $2`,
		userID, now.Add(-24*time.Hour)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO password_reset_tokens (user_id, token_hash, expires_at, requested_ip)
		 VALUES ($1::uuid, $2::bytea, $3::timestamptz, $4::text)`,
		userID, tokenHash, expiresAt, ip); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *ResetRepository) Valid(ctx context.Context, tokenHash []byte, now time.Time) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM password_reset_tokens t JOIN users u ON u.id = t.user_id
		    WHERE t.token_hash = $1 AND t.used_at IS NULL AND t.expires_at > $2 AND u.is_active)`,
		tokenHash, now).Scan(&ok)
	return ok, err
}

func (r *ResetRepository) Consume(ctx context.Context, tokenHash []byte, newPasswordHash string, now time.Time) (ConsumeResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ConsumeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, userID string
		expiresAt  time.Time
		usedAt     *time.Time
		active     bool
	)
	// FOR UPDATE: concurrent attempts with the same token serialize, and the
	// loser re-reads the row after the winner commits and sees used_at set.
	err = tx.QueryRow(ctx,
		`SELECT t.id::text, t.user_id::text, t.expires_at, t.used_at, u.is_active
		   FROM password_reset_tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = $1
		    FOR UPDATE OF t`, tokenHash).Scan(&id, &userID, &expiresAt, &usedAt, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsumeResult{Outcome: ConsumeNotFound}, nil
	}
	if err != nil {
		return ConsumeResult{}, err
	}
	switch {
	case usedAt != nil:
		return ConsumeResult{Outcome: ConsumeUsed, UserID: userID}, nil
	case !expiresAt.After(now):
		return ConsumeResult{Outcome: ConsumeExpired, UserID: userID}, nil
	case !active:
		return ConsumeResult{Outcome: ConsumeInactive, UserID: userID}, nil
	}

	// Being able to receive this email proves control of the address, so it is
	// also marked verified. Revoking every session evicts anyone who had
	// pre-registered this email or stolen an old session.
	if _, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, email_verified = true, updated_at = $3 WHERE id = $1::uuid`,
		userID, newPasswordHash, now); err != nil {
		return ConsumeResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE password_reset_tokens SET used_at = $2 WHERE id = $1::uuid`, id, now); err != nil {
		return ConsumeResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM password_reset_tokens WHERE user_id = $1::uuid AND id <> $2::uuid`, userID, id); err != nil {
		return ConsumeResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_sessions SET revoked_at = $2, revoked_reason = 'password_reset'
		  WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID, now); err != nil {
		return ConsumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConsumeResult{}, err
	}
	return ConsumeResult{Outcome: ConsumeOK, UserID: userID}, nil
}
