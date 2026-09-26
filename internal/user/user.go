// Package user contains the user model, username generation and persistence.
package user

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("user not found")
	ErrEmailTaken    = errors.New("email already registered")
	ErrUsernameTaken = errors.New("username already taken")
)

// Roles are exhaustive and match the users_role_check DB constraint.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// IsValidRole reports whether role is one of the known roles.
func IsValidRole(role string) bool {
	return role == RoleUser || role == RoleAdmin
}

type User struct {
	ID            string
	FullName      string
	Username      string
	Email         string
	PasswordHash  *string // nil for OAuth-only accounts (future)
	EmailVerified bool
	IsActive      bool
	Role          string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const maxSlugLen = 30

// Slugify converts a full name into a username base:
// "Feranmi Oresajo" -> "feranmi-oresajo". Only [a-z0-9-] survive, so the
// result always satisfies the users_username_format DB constraint.
// Names with no ASCII letters/digits fall back to "user".
func Slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // suppresses leading hyphens
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlugLen {
		s = strings.Trim(s[:maxSlugLen], "-")
	}
	if s == "" {
		return "user"
	}
	return s
}
