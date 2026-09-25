package config

import (
	"net"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":        "postgresql://u@localhost/migo",
		"ACCESS_TOKEN_SECRET": strings.Repeat("a", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8080" || cfg.AccessTokenTTL != 15*time.Minute || cfg.RefreshTokenTTL != 720*time.Hour {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !cfg.AutoMigrate {
		t.Fatal("AutoMigrate should default to true in development")
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	good := strings.Repeat("a", 32)
	cases := map[string]map[string]string{
		"missing db":       {"ACCESS_TOKEN_SECRET": good},
		"placeholder":      {"DATABASE_URL": "x", "ACCESS_TOKEN_SECRET": "CHANGE_ME"},
		"short secret":     {"DATABASE_URL": "x", "ACCESS_TOKEN_SECRET": "short"},
		"bad ttl":          {"DATABASE_URL": "x", "ACCESS_TOKEN_SECRET": good, "ACCESS_TOKEN_TTL": "-5m"},
		"bad port":         {"DATABASE_URL": "x", "ACCESS_TOKEN_SECRET": good, "PORT": "abc"},
		"bad auto_migrate": {"DATABASE_URL": "x", "ACCESS_TOKEN_SECRET": good, "AUTO_MIGRATE": "maybe"},
	}
	for name, m := range cases {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAutoMigrateOffOutsideDevelopment(t *testing.T) {
	cfg, err := Load(env(prod(base())))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoMigrate {
		t.Fatal("AutoMigrate should default to false in production")
	}
}

func base() map[string]string {
	return map[string]string{"DATABASE_URL": "x", "ACCESS_TOKEN_SECRET": strings.Repeat("a", 32)}
}

func TestOriginsDefaultsAndParsing(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != "http://localhost:3000" {
		t.Fatalf("dev defaults: %v", cfg.AllowedOrigins)
	}

	m := prod(base())
	m["FRONTEND_URL"] = " https://App.Example.com/ , https://app.example.com, https://admin.example.com:8443 "
	cfg, err = Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://app.example.com", "https://admin.example.com:8443"}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != want[0] || cfg.AllowedOrigins[1] != want[1] {
		t.Fatalf("normalized/deduped origins: %v", cfg.AllowedOrigins)
	}

	m = prod(base())
	delete(m, "FRONTEND_URL")
	m["PASSWORD_RESET_URL"] = "https://app.example.com/reset-password"
	m["EMAIL_VERIFICATION_URL"] = "https://app.example.com/verify-email"
	cfg, err = Load(env(m))
	if err != nil || len(cfg.AllowedOrigins) != 0 {
		t.Fatalf("production with no FRONTEND_URL must allow no origins: %v %v", cfg.AllowedOrigins, err)
	}
}

func TestOriginsRejectBad(t *testing.T) {
	bad := []string{"*", "app.example.com", "https://app.example.com/path", "ftp://x.com",
		"https://user:pw@x.com", "https://x.com?q=1", "https://x.com#frag", "https://"}
	for _, b := range bad {
		m := base()
		m["FRONTEND_URL"] = b
		if _, err := Load(env(m)); err == nil {
			t.Errorf("FRONTEND_URL %q should be rejected", b)
		}
	}
	m := prod(base())
	m["FRONTEND_URL"] = "http://app.example.com"
	if _, err := Load(env(m)); err == nil {
		t.Error("plain http must be rejected outside development")
	}
	m["FRONTEND_URL"] = "http://localhost:3000"
	if _, err := Load(env(m)); err != nil {
		t.Errorf("loopback http is fine: %v", err)
	}
}

func TestTrustedProxiesAndRateLimitFlag(t *testing.T) {
	m := base()
	m["TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.5, ::1, 2001:db8::/32"
	cfg, err := Load(env(m))
	if err != nil || len(cfg.TrustedProxies) != 4 {
		t.Fatalf("%v %v", cfg.TrustedProxies, err)
	}
	if !cfg.TrustedProxies[1].Contains(net.ParseIP("192.168.1.5")) || cfg.TrustedProxies[1].Contains(net.ParseIP("192.168.1.6")) {
		t.Fatal("single IP must become a /32")
	}
	if !cfg.RateLimitEnabled {
		t.Fatal("rate limiting must default to on")
	}
	m["RATE_LIMIT_ENABLED"] = "false"
	if cfg, _ = Load(env(m)); cfg.RateLimitEnabled {
		t.Fatal("RATE_LIMIT_ENABLED=false ignored")
	}
	for _, bad := range []string{"not-an-ip", "10.0.0.0/99", "10.0.0.1, nope"} {
		m := base()
		m["TRUSTED_PROXIES"] = bad
		if _, err := Load(env(m)); err == nil {
			t.Errorf("TRUSTED_PROXIES %q should be rejected", bad)
		}
	}
	m = base()
	m["RATE_LIMIT_ENABLED"] = "perhaps"
	if _, err := Load(env(m)); err == nil {
		t.Error("bad RATE_LIMIT_ENABLED must be rejected")
	}
}

// prod returns a valid production-mode environment on top of m.
func prod(m map[string]string) map[string]string {
	m["APP_ENV"] = "production"
	m["RESEND_API_KEY"] = "re_test_api_key"
	m["MAIL_FROM"] = "Migo <no-reply@migo.example>"
	m["FRONTEND_URL"] = "https://app.example.com"
	return m
}

func TestEmailConfigDevelopmentDefaults(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResendAPIKey != "" || cfg.PasswordResetTTL != 30*time.Minute || cfg.PasswordResetURL != "http://localhost:3000/reset-password" {
		t.Fatalf("dev defaults: resend key configured=%t url=%s", cfg.ResendAPIKey != "", cfg.PasswordResetURL)
	}
}

func TestEmailConfigProduction(t *testing.T) {
	m := prod(base())
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResendAPIKey != "re_test_api_key" || cfg.MailFrom != `"Migo" <no-reply@migo.example>` && cfg.MailFrom != "Migo <no-reply@migo.example>" {
		t.Fatalf("resend key configured=%t mail from=%q", cfg.ResendAPIKey != "", cfg.MailFrom)
	}
	if cfg.PasswordResetURL != "https://app.example.com/reset-password" {
		t.Fatalf("derived reset URL: %s", cfg.PasswordResetURL)
	}
	m["PASSWORD_RESET_URL"] = "https://app.example.com/account/reset"
	if cfg, _ = Load(env(m)); cfg.PasswordResetURL != "https://app.example.com/account/reset" {
		t.Fatalf("explicit URL: %s", cfg.PasswordResetURL)
	}
}

func TestEmailConfigRejectsBad(t *testing.T) {
	cases := map[string]func(map[string]string){
		"no Resend API key":   func(m map[string]string) { delete(m, "RESEND_API_KEY") },
		"no MAIL_FROM":        func(m map[string]string) { delete(m, "MAIL_FROM") },
		"bad MAIL_FROM":       func(m map[string]string) { m["MAIL_FROM"] = "not an address" },
		"ttl too short":       func(m map[string]string) { m["PASSWORD_RESET_TTL"] = "1m" },
		"ttl too long":        func(m map[string]string) { m["PASSWORD_RESET_TTL"] = "48h" },
		"reset url fragment":  func(m map[string]string) { m["PASSWORD_RESET_URL"] = "https://a.example/reset#x" },
		"reset url http":      func(m map[string]string) { m["PASSWORD_RESET_URL"] = "http://a.example/reset" },
		"reset url relative":  func(m map[string]string) { m["PASSWORD_RESET_URL"] = "/reset" },
		"no reset url source": func(m map[string]string) { delete(m, "FRONTEND_URL") },
	}
	for name, mutate := range cases {
		m := prod(base())
		mutate(m)
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEmailVerificationConfig(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmailVerificationTTL != 24*time.Hour || cfg.EmailVerificationURL != "http://localhost:3000/verify-email" {
		t.Fatalf("dev defaults: %v %s", cfg.EmailVerificationTTL, cfg.EmailVerificationURL)
	}
	m := prod(base())
	if cfg, err = Load(env(m)); err != nil || cfg.EmailVerificationURL != "https://app.example.com/verify-email" {
		t.Fatalf("derived URL: %v %v", cfg.EmailVerificationURL, err)
	}
	m["EMAIL_VERIFICATION_URL"] = "https://app.example.com/confirm"
	m["EMAIL_VERIFICATION_TTL"] = "48h"
	if cfg, err = Load(env(m)); err != nil || cfg.EmailVerificationURL != "https://app.example.com/confirm" || cfg.EmailVerificationTTL != 48*time.Hour {
		t.Fatalf("explicit: %v %v %v", cfg.EmailVerificationURL, cfg.EmailVerificationTTL, err)
	}
	bad := map[string]string{
		"EMAIL_VERIFICATION_URL=frag": "https://a.example/v#x",
		"EMAIL_VERIFICATION_URL=http": "http://a.example/v",
		"EMAIL_VERIFICATION_URL=rel":  "/verify",
		"TTL short":                   "10m",
		"TTL long":                    "200h",
	}
	for name, v := range bad {
		m := prod(base())
		if strings.HasPrefix(name, "TTL") {
			m["EMAIL_VERIFICATION_TTL"] = v
		} else {
			m["EMAIL_VERIFICATION_URL"] = v
		}
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
	m = prod(base())
	delete(m, "FRONTEND_URL")
	m["PASSWORD_RESET_URL"] = "https://app.example.com/reset"
	if _, err := Load(env(m)); err == nil {
		t.Error("verification URL must be required when it can't be derived")
	}
}
