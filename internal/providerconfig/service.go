// Package providerconfig manages encrypted provider credentials
// (provider_configs): telephony (Twilio) and payment (Paystack) API keys,
// stored only as authenticated-encryption ciphertext.
package providerconfig

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalidRequest  = errors.New("providerconfig: invalid request")
	ErrDuplicateConfig = errors.New("providerconfig: kind/key/name already exists")
	ErrNotFound        = errors.New("providerconfig: not found")
)

const (
	KindTelephony = "telephony"
	KindPayment   = "payment"
)

var providerKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config is provider_configs metadata. Credentials are never included; use
// Service.DecryptedCredentials for the internal callers (e.g. building a
// Twilio client) that need the plaintext.
type Config struct {
	ID                   string
	Kind                 string
	Key                  string
	Name                 string
	IsEnabled            bool
	CredentialKeyVersion string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Encryptor is satisfied by *providercrypto.KeyRing.
type Encryptor interface {
	Encrypt(plaintext []byte) (ciphertext []byte, keyVersion string, err error)
	Decrypt(ciphertext []byte, keyVersion string) ([]byte, error)
}

type Store interface {
	Create(ctx context.Context, kind, key, name string, ciphertext []byte, keyVersion string) (Config, error)
	List(ctx context.Context, kind string) ([]Config, error)
	Get(ctx context.Context, id string) (Config, error)
	GetCiphertext(ctx context.Context, id string) (ciphertext []byte, keyVersion string, err error)
	SetEnabled(ctx context.Context, id string, enabled bool) error
}

type Service struct {
	store  Store
	crypto Encryptor
}

func NewService(store Store, crypto Encryptor) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalidRequest)
	}
	if crypto == nil {
		return nil, fmt.Errorf("%w: an encryptor is required", ErrInvalidRequest)
	}
	return &Service{store: store, crypto: crypto}, nil
}

// Create validates and encrypts credentials, then persists the ciphertext.
// credentials is opaque to this package (e.g. a JSON blob of provider-specific
// fields); it is never logged or returned.
func (s *Service) Create(ctx context.Context, kind, key, name string, credentials []byte) (Config, error) {
	kind = strings.TrimSpace(kind)
	key = strings.TrimSpace(key)
	name = strings.TrimSpace(name)
	if (kind != KindTelephony && kind != KindPayment) ||
		!providerKeyPattern.MatchString(key) || name == "" || len(credentials) == 0 {
		return Config{}, ErrInvalidRequest
	}
	ciphertext, keyVersion, err := s.crypto.Encrypt(credentials)
	if err != nil {
		return Config{}, err
	}
	return s.store.Create(ctx, kind, key, name, ciphertext, keyVersion)
}

func (s *Service) List(ctx context.Context, kind string) ([]Config, error) {
	kind = strings.TrimSpace(kind)
	if kind != "" && kind != KindTelephony && kind != KindPayment {
		return nil, ErrInvalidRequest
	}
	return s.store.List(ctx, kind)
}

func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool) (Config, error) {
	if strings.TrimSpace(id) == "" {
		return Config{}, ErrInvalidRequest
	}
	if err := s.store.SetEnabled(ctx, id, enabled); err != nil {
		return Config{}, err
	}
	return s.store.Get(ctx, id)
}

// DecryptedCredentials returns the plaintext credentials for internal use
// only (e.g. constructing a telephony/payment provider client). It must
// never be exposed through an HTTP response.
func (s *Service) DecryptedCredentials(ctx context.Context, id string) ([]byte, error) {
	ciphertext, keyVersion, err := s.store.GetCiphertext(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.crypto.Decrypt(ciphertext, keyVersion)
}
