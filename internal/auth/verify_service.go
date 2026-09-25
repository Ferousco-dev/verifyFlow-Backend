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

var ErrInvalidVerificationToken = errors.New("invalid or expired verification token")

type verifyConfig struct {
	store   VerifyStore
	mailer  mailer.Sender
	baseURL string // frontend page; the token is appended as #token=...
	ttl     time.Duration
	log     *slog.Logger
}

// EnableEmailVerification turns on verification emails at sign-up and the
// verify/resend flows.
func (s *Service) EnableEmailVerification(store VerifyStore, m mailer.Sender, baseURL string, ttl time.Duration, log *slog.Logger) {
	s.verify = &verifyConfig{store: store, mailer: m, baseURL: baseURL, ttl: ttl, log: log}
}

func (s *Service) EmailVerificationEnabled() bool { return s.verify != nil }

// sendVerification creates a fresh token (cancelling any earlier one) and
// queues the email. Mail-queue problems are logged, not returned.
func (s *Service) sendVerification(ctx context.Context, u user.User) error {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.verify.store.Create(ctx, u.ID, u.Email, hash, now.Add(s.verify.ttl), now); err != nil {
		return err
	}
	link := s.verify.baseURL + "#token=" + raw
	if err := s.verify.mailer.Send(ctx, verificationEmail(u.Email, u.FullName, link, s.verify.ttl)); err != nil {
		s.verify.log.Error("could not queue verification email", "error", err)
	}
	return nil
}

// VerifyEmail spends a verification token and marks the address verified.
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) error {
	if s.verify == nil {
		return errors.New("email verification is not configured")
	}
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" || len(rawToken) > maxResetTokenLen {
		return ErrInvalidVerificationToken
	}
	res, err := s.verify.store.Consume(ctx, HashRefreshToken(rawToken), s.now())
	if err != nil {
		return err
	}
	if res.Outcome != VerifyOK {
		return ErrInvalidVerificationToken
	}
	return nil
}

// ResendVerification sends a new link to the signed-in user. It reports
// whether an email was queued (false: the address is already verified).
func (s *Service) ResendVerification(ctx context.Context, userID string) (bool, error) {
	if s.verify == nil {
		return false, errors.New("email verification is not configured")
	}
	u, err := s.users.GetByID(ctx, userID)
	if errors.Is(err, user.ErrNotFound) {
		return false, ErrUnauthorized
	}
	if err != nil {
		return false, err
	}
	if !u.IsActive {
		return false, ErrUnauthorized
	}
	if u.EmailVerified {
		return false, nil
	}
	if err := s.sendVerification(ctx, u); err != nil {
		return false, err
	}
	return true, nil
}
