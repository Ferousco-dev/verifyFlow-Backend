package auth

import (
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minPasswordLen = 10
	maxPasswordLen = 128 // bounds Argon2 work per request
	maxNameLen     = 100
	maxEmailLen    = 254
)

// ValidationError carries per-field messages for a 422 response.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "validation failed" }

// NormalizeEmail trims and lowercases. Applied before every persistence
// or comparison so "A@B.com " and "a@b.com" are the same account.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validateEmail(email string) string {
	if email == "" {
		return "is required"
	}
	if len(email) > maxEmailLen {
		return "is too long"
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || !strings.Contains(email[strings.LastIndex(email, "@"):], ".") {
		return "must be a valid email address"
	}
	return ""
}

func validatePassword(p string) string {
	switch {
	case len(p) < minPasswordLen:
		return "must be at least 10 characters"
	case len(p) > maxPasswordLen:
		return "must be at most 128 characters"
	}
	return ""
}

func validateFullName(n string) string {
	if n == "" {
		return "is required"
	}
	if utf8.RuneCountInString(n) > maxNameLen {
		return "is too long"
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return "contains invalid characters"
		}
	}
	return ""
}
