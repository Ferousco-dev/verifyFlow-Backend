package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ChangeOutcome int

const (
	ChangeOK ChangeOutcome = iota
	// ChangeConflict: the stored password hash is no longer the one the caller
	// verified (e.g. a reset or another change landed in between).
	ChangeConflict
)

// PasswordChange describes an authenticated password change.
type PasswordChange struct {
	UserID       string
	ExpectedHash string // the hash the current password was verified against
	NewHash      string
	Session      NewSession // the caller's replacement session
}

type CredentialStore interface {
	// ChangePassword atomically: compare-and-swaps the password hash, revokes
	// ALL of the user's refresh sessions, deletes their pending reset tokens
	// and starts the replacement session.
	ChangePassword(ctx context.Context, c PasswordChange, now time.Time) (ChangeOutcome, error)
}

type CredentialRepository struct {
	pool *pgxpool.Pool
}

func NewCredentialRepository(pool *pgxpool.Pool) *CredentialRepository {
	return &CredentialRepository{pool: pool}
}

func (r *CredentialRepository) ChangePassword(ctx context.Context, c PasswordChange, now time.Time) (ChangeOutcome, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Compare-and-swap. The current password was verified (slowly, outside the
	// transaction) against ExpectedHash. If the hash changed since, e.g. the
	// real owner just reset a compromised password, this update matches no row
	// and the stale request cannot overwrite the newer credentials.
	tag, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $3, updated_at = $4
		  WHERE id = $1::uuid AND password_hash = $2 AND is_active`,
		c.UserID, c.ExpectedHash, c.NewHash, now)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return ChangeConflict, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_sessions SET revoked_at = $2, revoked_reason = 'password_changed'
		  WHERE user_id = $1::uuid AND revoked_at IS NULL`, c.UserID, now); err != nil {
		return 0, err
	}
	// A reset link requested before the change must not outlive it.
	if _, err := tx.Exec(ctx, `DELETE FROM password_reset_tokens WHERE user_id = $1::uuid`, c.UserID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`WITH n AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO refresh_sessions (id, family_id, user_id, token_hash, expires_at, user_agent, ip)
		 SELECT n.id, n.id, $1::uuid, $2::bytea, $3::timestamptz, $4::text, $5::text FROM n`,
		c.UserID, c.Session.TokenHash, c.Session.ExpiresAt, c.Session.UserAgent, c.Session.IP); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return ChangeOK, nil
}
