package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func b64Key(b byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestLoadWithoutProviderCredentialKeysLeavesThemUnset(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCredentialKeys != nil || cfg.ProviderCredentialKeyVersion != "" {
		t.Fatalf("expected unset provider credential keys, got %+v", cfg)
	}
}

func TestLoadParsesProviderCredentialKeys(t *testing.T) {
	m := base()
	m["PROVIDER_CREDENTIAL_KEYS"] = "v1:" + b64Key(1) + ",v2:" + b64Key(2)
	m["PROVIDER_CREDENTIAL_KEY_VERSION"] = "v2"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderCredentialKeyVersion != "v2" || len(cfg.ProviderCredentialKeys) != 2 {
		t.Fatalf("got %+v", cfg)
	}
	if len(cfg.ProviderCredentialKeys["v1"]) != 32 || len(cfg.ProviderCredentialKeys["v2"]) != 32 {
		t.Fatalf("key lengths: %+v", cfg.ProviderCredentialKeys)
	}
}

func TestLoadRejectsMalformedProviderCredentialKeys(t *testing.T) {
	cases := map[string]map[string]string{
		"version without version tag": {"PROVIDER_CREDENTIAL_KEYS": "not-a-pair", "PROVIDER_CREDENTIAL_KEY_VERSION": "v1"},
		"bad base64":                  {"PROVIDER_CREDENTIAL_KEYS": "v1:not-base64!!", "PROVIDER_CREDENTIAL_KEY_VERSION": "v1"},
		"wrong key length":            {"PROVIDER_CREDENTIAL_KEYS": "v1:" + base64.StdEncoding.EncodeToString([]byte("too-short")), "PROVIDER_CREDENTIAL_KEY_VERSION": "v1"},
		"missing current version":     {"PROVIDER_CREDENTIAL_KEYS": "v1:" + b64Key(1)},
		"current version not in set":  {"PROVIDER_CREDENTIAL_KEYS": "v1:" + b64Key(1), "PROVIDER_CREDENTIAL_KEY_VERSION": "v9"},
		"version set without keys":    {"PROVIDER_CREDENTIAL_KEY_VERSION": "v1"},
	}
	for name, extra := range cases {
		m := base()
		for k, v := range extra {
			m[k] = v
		}
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseProviderCredentialKeysIgnoresBlankEntries(t *testing.T) {
	raw := "v1:" + b64Key(1) + ", ,v2:" + b64Key(2)
	keys, version, err := parseProviderCredentialKeys(raw, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || version != "v1" {
		t.Fatalf("got %v, %q", keys, version)
	}
	if !strings.Contains(raw, "v2") {
		t.Fatal("sanity check on test data")
	}
}
