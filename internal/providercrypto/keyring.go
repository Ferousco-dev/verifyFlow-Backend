// Package providercrypto encrypts provider credentials (Twilio auth tokens,
// Paystack secret keys, ...) at rest using versioned AES-256-GCM keys loaded
// from the environment. Keys are never stored in the database; only the
// version tag that selects which key decrypts a given ciphertext is.
package providercrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrUnknownKeyVersion is returned when ciphertext names a key version
	// this KeyRing was not configured with (e.g. rotated out too early).
	ErrUnknownKeyVersion = errors.New("providercrypto: unknown key version")
	// ErrDecryptionFailed covers any ciphertext that fails authentication:
	// wrong key, truncated data, or tampering.
	ErrDecryptionFailed = errors.New("providercrypto: decryption failed")
)

const keyLen = 32 // AES-256

// KeyRing holds every key version this process can decrypt with, and the
// version new encryptions are written under. Rotation is manual: add a new
// version, point CurrentVersion at it, keep the old version around until
// every row has been re-encrypted, then drop it.
type KeyRing struct {
	keys           map[string][]byte
	currentVersion string
}

// NewKeyRing validates that every key is exactly 32 bytes (AES-256) and that
// currentVersion names a key present in keys.
func NewKeyRing(keys map[string][]byte, currentVersion string) (*KeyRing, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("providercrypto: at least one key is required")
	}
	for version, key := range keys {
		if len(key) != keyLen {
			return nil, fmt.Errorf("providercrypto: key %q must be %d bytes, got %d", version, keyLen, len(key))
		}
	}
	if _, ok := keys[currentVersion]; !ok {
		return nil, fmt.Errorf("providercrypto: current version %q has no corresponding key", currentVersion)
	}
	copied := make(map[string][]byte, len(keys))
	for version, key := range keys {
		copied[version] = append([]byte(nil), key...)
	}
	return &KeyRing{keys: copied, currentVersion: currentVersion}, nil
}

// CurrentVersion is the key version Encrypt writes new ciphertext under.
func (k *KeyRing) CurrentVersion() string { return k.currentVersion }

// Encrypt seals plaintext under the current key version. The returned
// ciphertext is self-contained (nonce prefixed); keyVersion must be stored
// alongside it so Decrypt knows which key to use later.
func (k *KeyRing) Encrypt(plaintext []byte) (ciphertext []byte, keyVersion string, err error) {
	gcm, err := k.gcmFor(k.currentVersion)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return sealed, k.currentVersion, nil
}

// Decrypt opens ciphertext produced by Encrypt under keyVersion.
func (k *KeyRing) Decrypt(ciphertext []byte, keyVersion string) ([]byte, error) {
	gcm, err := k.gcmFor(keyVersion)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, ErrDecryptionFailed
	}
	nonce, sealed := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

func (k *KeyRing) gcmFor(version string) (cipher.AEAD, error) {
	key, ok := k.keys[version]
	if !ok {
		return nil, ErrUnknownKeyVersion
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
