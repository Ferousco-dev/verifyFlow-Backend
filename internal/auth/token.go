package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrInvalidToken = errors.New("invalid access token")

// TokenManager issues and verifies HS256 JWT access tokens using only the
// standard library. Claims are deliberately minimal: iss, sub, iat, exp.
type TokenManager struct {
	secret []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenManager(secret []byte, issuer string, ttl time.Duration) *TokenManager {
	return &TokenManager{secret: secret, issuer: issuer, ttl: ttl, now: time.Now}
}

type claims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

var b64 = base64.RawURLEncoding

// The only header we accept. Comparing the raw segment rejects alg=none,
// alg confusion, and any other header shape outright.
var headerSegment = b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func (m *TokenManager) TTL() time.Duration { return m.ttl }

func (m *TokenManager) sign(input string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(input))
	return b64.EncodeToString(mac.Sum(nil))
}

// Issue returns a signed token for userID and its expiry.
func (m *TokenManager) Issue(userID string) (string, time.Time, error) {
	now := m.now()
	exp := now.Add(m.ttl)
	payload, err := json.Marshal(claims{Iss: m.issuer, Sub: userID, Iat: now.Unix(), Exp: exp.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	signingInput := headerSegment + "." + b64.EncodeToString(payload)
	return signingInput + "." + m.sign(signingInput), exp, nil
}

// Parse verifies signature, issuer and expiry and returns the user ID.
func (m *TokenManager) Parse(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != headerSegment {
		return "", ErrInvalidToken
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return "", ErrInvalidToken
	}
	expected, _ := b64.DecodeString(m.sign(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, expected) {
		return "", ErrInvalidToken
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return "", ErrInvalidToken
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", ErrInvalidToken
	}
	if c.Iss != m.issuer || c.Sub == "" || !m.now().Before(time.Unix(c.Exp, 0)) {
		return "", ErrInvalidToken
	}
	return c.Sub, nil
}
