package providercrypto

import (
	"bytes"
	"errors"
	"testing"
)

func key(b byte) []byte {
	k := make([]byte, keyLen)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestKeyRingEncryptDecryptRoundTrip(t *testing.T) {
	kr, err := NewKeyRing(map[string][]byte{"v1": key(1)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"account_sid":"AC123","auth_token":"secret"}`)
	ciphertext, version, err := kr.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1" {
		t.Fatalf("version = %q", version)
	}
	if bytes.Contains(ciphertext, []byte("secret")) {
		t.Fatal("ciphertext must not contain the plaintext")
	}
	got, err := kr.Decrypt(ciphertext, version)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("Decrypt = %s, %v", got, err)
	}
}

func TestKeyRingDecryptsOlderVersionsAfterRotation(t *testing.T) {
	kr, err := NewKeyRing(map[string][]byte{"v1": key(1), "v2": key(2)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("old-secret")
	ciphertext, version, err := kr.Encrypt(plaintext)
	if err != nil || version != "v1" {
		t.Fatalf("Encrypt = %v, %v", version, err)
	}

	rotated, err := NewKeyRing(map[string][]byte{"v1": key(1), "v2": key(2)}, "v2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Decrypt(ciphertext, version)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("Decrypt after rotation = %s, %v", got, err)
	}
	newCiphertext, newVersion, err := rotated.Encrypt(plaintext)
	if err != nil || newVersion != "v2" {
		t.Fatalf("Encrypt after rotation = %v, %v", newVersion, err)
	}
	if bytes.Equal(newCiphertext, ciphertext) {
		t.Fatal("re-encryption under a new key must not equal the old ciphertext")
	}
}

func TestKeyRingRejectsUnknownVersion(t *testing.T) {
	kr, err := NewKeyRing(map[string][]byte{"v1": key(1)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Decrypt([]byte("whatever"), "v99"); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("unknown version error = %v", err)
	}
}

func TestKeyRingRejectsTamperedCiphertext(t *testing.T) {
	kr, err := NewKeyRing(map[string][]byte{"v1": key(1)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, version, err := kr.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := kr.Decrypt(tampered, version); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("tampered ciphertext error = %v", err)
	}
}

func TestKeyRingRejectsWrongKeyForVersion(t *testing.T) {
	kr, err := NewKeyRing(map[string][]byte{"v1": key(1)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, version, err := kr.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewKeyRing(map[string][]byte{"v1": key(2)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt(ciphertext, version); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("wrong key error = %v", err)
	}
}

func TestNewKeyRingValidatesKeyLengthAndCurrentVersion(t *testing.T) {
	if _, err := NewKeyRing(map[string][]byte{"v1": []byte("too-short")}, "v1"); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := NewKeyRing(map[string][]byte{"v1": key(1)}, "v2"); err == nil {
		t.Fatal("expected error for missing current version")
	}
	if _, err := NewKeyRing(map[string][]byte{}, "v1"); err == nil {
		t.Fatal("expected error for empty key set")
	}
}
