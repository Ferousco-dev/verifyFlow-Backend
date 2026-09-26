package providerconfig

import (
	"context"
	"errors"
	"testing"

	"migo/internal/providercrypto"
	"migo/internal/testutil/dbtest"
)

func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func newFixture(t *testing.T) *Service {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	keyRing, err := providercrypto.NewKeyRing(map[string][]byte{"v1": key(1)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(NewRepository(pool), keyRing)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestServiceCreateEncryptsAndRoundTripsCredentials(t *testing.T) {
	svc := newFixture(t)
	plaintext := []byte(`{"account_sid":"AC123","auth_token":"top-secret"}`)

	created, err := svc.Create(context.Background(), KindTelephony, "twilio", "primary", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Kind != KindTelephony || created.Key != "twilio" || created.Name != "primary" ||
		!created.IsEnabled || created.CredentialKeyVersion != "v1" {
		t.Fatalf("created = %+v", created)
	}

	got, err := svc.DecryptedCredentials(context.Background(), created.ID)
	if err != nil || string(got) != string(plaintext) {
		t.Fatalf("DecryptedCredentials = %s, %v", got, err)
	}
}

func TestServiceCreateRejectsInvalidFields(t *testing.T) {
	svc := newFixture(t)
	cases := []struct {
		name               string
		kind, key, cfgName string
		credentials        []byte
	}{
		{"bad kind", "sms", "twilio", "primary", []byte("x")},
		{"bad key format", KindTelephony, "Twilio!", "primary", []byte("x")},
		{"empty name", KindTelephony, "twilio", "", []byte("x")},
		{"empty credentials", KindTelephony, "twilio", "primary", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Create(context.Background(), tc.kind, tc.key, tc.cfgName, tc.credentials); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Create error = %v", err)
			}
		})
	}
}

func TestServiceCreateRejectsDuplicateKindKeyName(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, KindTelephony, "twilio", "primary", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, KindTelephony, "twilio", "primary", []byte("y")); !errors.Is(err, ErrDuplicateConfig) {
		t.Fatalf("duplicate create error = %v", err)
	}
}

func TestServiceListFiltersByKindAndOmitsCredentials(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, KindTelephony, "twilio", "primary", []byte("secret-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, KindPayment, "paystack", "primary", []byte("secret-b")); err != nil {
		t.Fatal(err)
	}

	telephony, err := svc.List(ctx, KindTelephony)
	if err != nil || len(telephony) != 1 || telephony[0].Key != "twilio" {
		t.Fatalf("List(telephony) = %+v, %v", telephony, err)
	}

	all, err := svc.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("List(all) = %+v, %v", all, err)
	}
}

func TestServiceSetEnabledTogglesAndReturnsNotFoundForUnknownID(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, KindTelephony, "twilio", "primary", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	disabled, err := svc.SetEnabled(ctx, created.ID, false)
	if err != nil || disabled.IsEnabled {
		t.Fatalf("SetEnabled(false) = %+v, %v", disabled, err)
	}

	if _, err := svc.SetEnabled(ctx, "00000000-0000-0000-0000-000000000000", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetEnabled unknown id error = %v", err)
	}
}

func TestDecryptedCredentialsUnknownIDReturnsNotFound(t *testing.T) {
	svc := newFixture(t)
	if _, err := svc.DecryptedCredentials(context.Background(), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DecryptedCredentials unknown id error = %v", err)
	}
}
