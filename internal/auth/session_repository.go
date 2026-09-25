package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type NewSession struct {
	UserID    string
	TokenHash []byte
	ExpiresAt time.Time
	UserAgent string
	IP        string
}

type RotateOutcome int

const (
	RotateOK RotateOutcome = iota
	RotateNotFound
	RotateExpired
	RotateRevoked
	RotateReuseDetected // an already-rotated token was replayed; family revoked
)

type RotateResult struct {
	Outcome RotateOutcome
	UserID  string
}

// SessionStore persists refresh sessions.
type SessionStore interface {
	// Create starts a new session family (a fresh login).
	Create(ctx context.Context, s NewSession) error
	// Rotate atomically replaces the session identified by oldHash with next.
	Rotate(ctx context.Context, oldHash []byte, next NewSession, now time.Time) (RotateResult, error)
	// RevokeFamily revokes every live session in the family of tokenHash.
	RevokeFamily(ctx context.Context, tokenHash []byte, reason string, now time.Time) error
}

type SessionRepository struct {
	pool *pgxpool.Pool
}

func NewSessionRepository(pool *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{pool: pool}
}

func (r *SessionRepository) Create(ctx context.Context, s NewSession) error {
	_, err := r.pool.Exec(ctx,
		`WITH n AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO refresh_sessions (id, family_id, user_id, token_hash, expires_at, user_agent, ip)
		 SELECT n.id, n.id, $1::uuid, $2::bytea, $3::timestamptz, $4::text, $5::text FROM n`,
		s.UserID, s.TokenHash, s.ExpiresAt, s.UserAgent, s.IP)
	return err
}

func (r *SessionRepository) Rotate(ctx context.Context, oldHash []byte, next NewSession, now time.Time) (RotateResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RotateResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, userID, familyID string
		expiresAt            time.Time
		revokedAt            *time.Time
		replacedBy           *string
		active               bool
	)
	// FOR UPDATE serializes concurrent use of the same token.
	err = tx.QueryRow(ctx,
		`SELECT s.id::text, s.user_id::text, s.family_id::text, s.expires_at,
		        s.revoked_at, s.replaced_by::text, u.is_active
		   FROM refresh_sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1
		    FOR UPDATE OF s`, oldHash).
		Scan(&id, &userID, &familyID, &expiresAt, &revokedAt, &replacedBy, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return RotateResult{Outcome: RotateNotFound}, nil
	}
	if err != nil {
		return RotateResult{}, err
	}

	revokeFamily := func(reason string) error {
		_, err := tx.Exec(ctx,
			`UPDATE refresh_sessions SET revoked_at = $2, revoked_reason = $3
			  WHERE family_id = $1::uuid AND revoked_at IS NULL`, familyID, now, reason)
		return err
	}

	switch {
	case replacedBy != nil:
		// This token was already rotated: someone is replaying it. Kill the
		// whole family and commit (so the revocation persists).
		if err := revokeFamily("reuse_detected"); err != nil {
			return RotateResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RotateResult{}, err
		}
		return RotateResult{Outcome: RotateReuseDetected, UserID: userID}, nil
	case revokedAt != nil:
		return RotateResult{Outcome: RotateRevoked, UserID: userID}, nil
	case !expiresAt.After(now):
		return RotateResult{Outcome: RotateExpired, UserID: userID}, nil
	case !active:
		if err := revokeFamily("user_inactive"); err != nil {
			return RotateResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RotateResult{}, err
		}
		return RotateResult{Outcome: RotateRevoked, UserID: userID}, nil
	}

	var newID string
	err = tx.QueryRow(ctx,
		`INSERT INTO refresh_sessions (family_id, user_id, token_hash, expires_at, user_agent, ip)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6) RETURNING id::text`,
		familyID, userID, next.TokenHash, next.ExpiresAt, next.UserAgent, next.IP).Scan(&newID)
	if err != nil {
		return RotateResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_sessions
		    SET revoked_at = $2, revoked_reason = 'rotated', replaced_by = $3::uuid, last_used_at = $2
		  WHERE id = $1::uuid`, id, now, newID); err != nil {
		return RotateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RotateResult{}, err
	}
	return RotateResult{Outcome: RotateOK, UserID: userID}, nil
}

func (r *SessionRepository) RevokeFamily(ctx context.Context, tokenHash []byte, reason string, now time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE refresh_sessions SET revoked_at = $3, revoked_reason = $2
		  WHERE revoked_at IS NULL
		    AND family_id = (SELECT family_id FROM refresh_sessions WHERE token_hash = $1)`,
		tokenHash, reason, now)
	return err
}
