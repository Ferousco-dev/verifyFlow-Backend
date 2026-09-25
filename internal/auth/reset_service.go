package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"migo/internal/mailer"
	"migo/internal/user"
)

var ErrInvalidResetToken = errors.New("invalid or expired reset token")

const maxResetTokenLen = 200

// resetConfig is set by EnablePasswordReset; nil means the feature is off.
type resetConfig struct {
	store   ResetStore
	mailer  mailer.Sender
	baseURL string // frontend page; the token is appended as #token=...
	ttl     time.Duration
	log     *slog.Logger
}

// EnablePasswordReset turns on the forgot/reset flow.
func (s *Service) EnablePasswordReset(store ResetStore, m mailer.Sender, baseURL string, ttl time.Duration, log *slog.Logger) {
	s.reset = &resetConfig{store: store, mailer: m, baseURL: baseURL, ttl: ttl, log: log}
}

func (s *Service) PasswordResetEnabled() bool { return s.reset != nil }

// RequestPasswordReset starts a reset for email. It returns nil whether or not
// an account exists (and whether or not mail could be queued), so callers can
// answer identically and never reveal which emails are registered. Only a
// malformed address produces an error.
func (s *Service) RequestPasswordReset(ctx context.Context, email string, meta Meta) error {
	if s.reset == nil {
		return errors.New("password reset is not configured")
	}
	email = NormalizeEmail(email)
	if m := validateEmail(email); m != "" {
		return &ValidationError{Fields: map[string]string{"email": m}}
	}

	u, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, user.ErrNotFound) || (err == nil && !u.IsActive) {
		return nil
	}
	if err != nil {
		return err
	}

	raw, hash, err := NewRefreshToken() // same primitive: 256-bit random, SHA-256 digest stored
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.reset.store.Create(ctx, u.ID, hash, now.Add(s.reset.ttl), meta.IP, now); err != nil {
		return err
	}
	link := s.reset.baseURL + "#token=" + raw
	// Mail goes through the async queue; delivery problems are logged, never surfaced.
	if err := s.reset.mailer.Send(ctx, resetEmail(u.Email, u.FullName, link, s.reset.ttl)); err != nil {
		s.reset.log.Error("could not queue password reset email", "error", err)
	}
	return nil
}

// ResetPassword spends a reset token and sets a new password. All existing
// sessions are revoked, so the user must sign in again.
func (s *Service) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	if s.reset == nil {
		return errors.New("password reset is not configured")
	}
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" || len(rawToken) > maxResetTokenLen {
		return ErrInvalidResetToken
	}
	hash := HashRefreshToken(rawToken)

	// Reject bad tokens before spending ~50ms/64MiB on Argon2.
	ok, err := s.reset.store.Valid(ctx, hash, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidResetToken
	}
	// Validate the password BEFORE consuming the token, so a too-weak password
	// doesn't burn the user's link.
	if m := validatePassword(newPassword); m != "" {
		return &ValidationError{Fields: map[string]string{"new_password": m}}
	}
	pwHash, err := s.hasher.Hash(ctx, newPassword)
	if err != nil {
		return err
	}

	res, err := s.reset.store.Consume(ctx, hash, pwHash, s.now())
	if err != nil {
		return err
	}
	if res.Outcome != ConsumeOK {
		return ErrInvalidResetToken
	}

	if u, err := s.users.GetByID(ctx, res.UserID); err == nil {
		if err := s.reset.mailer.Send(ctx, passwordChangedEmail(u.Email, u.FullName)); err != nil {
			s.reset.log.Error("could not queue password-changed email", "error", err)
		}
	}
	return nil
}
