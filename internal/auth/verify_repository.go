package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type VerifyOutcome int

const (
	VerifyOK VerifyOutcome = iota
	VerifyNotFound
	VerifyUsed
	VerifyExpired
	VerifyInactive
	VerifyEmailChanged // the account's email no longer matches the address this token was issued for
)

type VerifyResult struct {
	Outcome VerifyOutcome
	UserID  string
}

// VerifyStore persists single-use email verification tokens.
type VerifyStore interface {
	// Create stores a token bound to email, replacing any earlier unused one.
	Create(ctx context.Context, userID, email string, tokenHash []byte, expiresAt, now time.Time) error
	// Consume atomically spends the token and marks the user's email verified.
	Consume(ctx context.Context, tokenHash []byte, now time.Time) (VerifyResult, error)
}

type VerifyRepository struct {
	pool *pgxpool.Pool
}

func NewVerifyRepository(pool *pgxpool.Pool) *VerifyRepository { return &VerifyRepository{pool: pool} }

func (r *VerifyRepository) Create(ctx context.Context, userID, email string, tokenHash []byte, expiresAt, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM email_verification_tokens
		  WHERE (user_id = $1::uuid AND used_at IS NULL) OR expires_at < $2`,
		userID, now.Add(-7*24*time.Hour)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO email_verification_tokens (user_id, email, token_hash, expires_at)
		 VALUES ($1::uuid, $2::text, $3::bytea, $4::timestamptz)`,
		userID, email, tokenHash, expiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *VerifyRepository) Consume(ctx context.Context, tokenHash []byte, now time.Time) (VerifyResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VerifyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, userID string
		expiresAt  time.Time
		usedAt     *time.Time
		active     bool
		emailMatch bool
	)
	err = tx.QueryRow(ctx,
		`SELECT t.id::text, t.user_id::text, t.expires_at, t.used_at, u.is_active, (t.email = u.email)
		   FROM email_verification_tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = $1
		    FOR UPDATE OF t`, tokenHash).Scan(&id, &userID, &expiresAt, &usedAt, &active, &emailMatch)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyResult{Outcome: VerifyNotFound}, nil
	}
	if err != nil {
		return VerifyResult{}, err
	}
	switch {
	case usedAt != nil:
		return VerifyResult{Outcome: VerifyUsed, UserID: userID}, nil
	case !expiresAt.After(now):
		return VerifyResult{Outcome: VerifyExpired, UserID: userID}, nil
	case !active:
		return VerifyResult{Outcome: VerifyInactive, UserID: userID}, nil
	case !emailMatch:
		return VerifyResult{Outcome: VerifyEmailChanged, UserID: userID}, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET email_verified = true, updated_at = $2 WHERE id = $1::uuid`, userID, now); err != nil {
		return VerifyResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE email_verification_tokens SET used_at = $2 WHERE id = $1::uuid`, id, now); err != nil {
		return VerifyResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM email_verification_tokens WHERE user_id = $1::uuid AND id <> $2::uuid`, userID, id); err != nil {
		return VerifyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{Outcome: VerifyOK, UserID: userID}, nil
}
