package payment

import (
	"context"

	"migo/internal/auth"
)

// AuthUserEmails adapts auth.Service to the UserEmails interface this
// package depends on.
type AuthUserEmails struct {
	svc *auth.Service
}

func NewAuthUserEmails(svc *auth.Service) *AuthUserEmails { return &AuthUserEmails{svc: svc} }

func (a *AuthUserEmails) Email(ctx context.Context, userID string) (string, error) {
	u, err := a.svc.Me(ctx, userID)
	if err != nil {
		return "", err
	}
	return u.Email, nil
}

func (a *AuthUserEmails) FullName(ctx context.Context, userID string) (string, error) {
	u, err := a.svc.Me(ctx, userID)
	if err != nil {
		return "", err
	}
	return u.FullName, nil
}
