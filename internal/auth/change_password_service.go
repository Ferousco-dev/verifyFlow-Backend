package auth

import (
	"context"
	"errors"
	"log/slog"

	"migo/internal/mailer"
)

var (
	// ErrPasswordNotSet: the account has no password (e.g. Google-only), so
	// there is no "current password" to check. Use forgot-password to set one.
	ErrPasswordNotSet = errors.New("account has no password")
	// ErrConcurrentChange: the password changed between verification and update.
	ErrConcurrentChange = errors.New("password was changed concurrently")
)

type changeConfig struct {
	store  CredentialStore
	mailer mailer.Sender
	log    *slog.Logger
}

// EnableChangePassword turns on authenticated password changes.
func (s *Service) EnableChangePassword(store CredentialStore, m mailer.Sender, log *slog.Logger) {
	s.change = &changeConfig{store: store, mailer: m, log: log}
}

func (s *Service) ChangePasswordEnabled() bool { return s.change != nil }

type ChangePasswordInput struct{ CurrentPassword, NewPassword string }

// ChangePassword verifies the current password, sets the new one, signs the
// user out everywhere and returns fresh tokens for the calling device so it
// stays signed in. (Access tokens already issued remain valid until they
// expire, at most ACCESS_TOKEN_TTL; JWTs are stateless.)
func (s *Service) ChangePassword(ctx context.Context, userID string, in ChangePasswordInput, meta Meta) (Tokens, error) {
	if s.change == nil {
		return Tokens{}, errors.New("change password is not configured")
	}
	fields := map[string]string{}
	if in.CurrentPassword == "" {
		fields["current_password"] = "is required"
	}
	if m := validatePassword(in.NewPassword); m != "" {
		fields["new_password"] = m
	}
	if len(fields) > 0 {
		return Tokens{}, &ValidationError{Fields: fields}
	}

	u, err := s.users.GetByID(ctx, userID)
	if err != nil || !u.IsActive {
		if err != nil && !errors.Is(err, userNotFound) {
			return Tokens{}, err
		}
		return Tokens{}, ErrUnauthorized
	}
	if u.PasswordHash == nil {
		return Tokens{}, ErrPasswordNotSet
	}

	ok, _, verr := s.hasher.Verify(ctx, in.CurrentPassword, *u.PasswordHash)
	if verr != nil {
		return Tokens{}, verr
	}
	if !ok {
		return Tokens{}, &ValidationError{Fields: map[string]string{"current_password": "is incorrect"}}
	}
	if in.CurrentPassword == in.NewPassword {
		return Tokens{}, &ValidationError{Fields: map[string]string{"new_password": "must be different from the current password"}}
	}

	newHash, err := s.hasher.Hash(ctx, in.NewPassword)
	if err != nil {
		return Tokens{}, err
	}
	raw, tokenHash, err := NewRefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	outcome, err := s.change.store.ChangePassword(ctx, PasswordChange{
		UserID: u.ID, ExpectedHash: *u.PasswordHash, NewHash: newHash,
		Session: NewSession{UserID: u.ID, TokenHash: tokenHash, ExpiresAt: now.Add(s.refreshTTL), UserAgent: meta.UserAgent, IP: meta.IP},
	}, now)
	if err != nil {
		return Tokens{}, err
	}
	if outcome != ChangeOK {
		return Tokens{}, ErrConcurrentChange
	}

	access, _, err := s.tokens.Issue(u.ID)
	if err != nil {
		return Tokens{}, err
	}
	if err := s.change.mailer.Send(ctx, passwordChangedEmail(u.Email, u.FullName)); err != nil {
		s.change.log.Error("could not queue password-changed email", "error", err)
	}
	return Tokens{AccessToken: access, RefreshToken: raw, ExpiresIn: int(s.tokens.TTL().Seconds())}, nil
}
