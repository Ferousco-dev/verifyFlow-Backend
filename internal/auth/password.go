// Package auth implements registration, login, JWT access tokens,
// rotating refresh sessions, and authentication middleware.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. They are encoded into every hash,
// so they can be raised later; NeedsRehash detects hashes below the current
// parameters and Login transparently upgrades them.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams: 64 MiB, 3 passes, 2 lanes (above the OWASP minimum).
func DefaultParams() Params {
	return Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}
}

// Hasher hashes and verifies passwords with Argon2id. A semaphore bounds
// concurrent hashing so a burst of login requests cannot exhaust memory.
type Hasher struct {
	params Params
	sem    chan struct{}
}

func NewHasher(p Params, maxConcurrent int) *Hasher {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Hasher{params: p, sem: make(chan struct{}, maxConcurrent)}
}

func (h *Hasher) acquire(ctx context.Context) (func(), error) {
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Hash returns a PHC-format string: $argon2id$v=19$m=..,t=..,p=..$salt$hash
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

var errInvalidHash = errors.New("invalid password hash format")

type parsedHash struct {
	params Params
	salt   []byte
	key    []byte
}

func parseHash(encoded string) (parsedHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return parsedHash{}, errInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return parsedHash{}, errInvalidHash
	}
	var p Params
	var threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &threads); err != nil || threads == 0 || threads > 255 {
		return parsedHash{}, errInvalidHash
	}
	p.Threads = uint8(threads)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return parsedHash{}, errInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return parsedHash{}, errInvalidHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return parsedHash{params: p, salt: salt, key: key}, nil
}

// Verify checks password against an encoded hash in constant time using the
// parameters stored in the hash. needsRehash reports outdated parameters.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	ph, err := parseHash(encoded)
	if err != nil {
		return false, false, err
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return false, false, err
	}
	defer release()

	key := argon2.IDKey([]byte(password), ph.salt, ph.params.Time, ph.params.Memory, ph.params.Threads, ph.params.KeyLen)
	if subtle.ConstantTimeCompare(key, ph.key) != 1 {
		return false, false, nil
	}
	return true, ph.params != h.params, nil
}
