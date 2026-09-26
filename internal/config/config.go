// Package config loads and validates runtime configuration from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppEnv            string
	Port              string
	DatabaseURL       string
	AccessTokenSecret []byte
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	AutoMigrate       bool

	// AllowedOrigins are the browser origins allowed by CORS (FRONTEND_URL).
	AllowedOrigins []string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is honoured.
	TrustedProxies   []*net.IPNet
	RateLimitEnabled bool

	ResendAPIKey string
	MailFrom     string
	// PasswordResetURL is the frontend page that receives the reset token,
	// appended as a #token=... fragment (never sent to servers or logs).
	PasswordResetURL string
	PasswordResetTTL time.Duration

	// EmailVerificationURL is the frontend page that receives the verification
	// token (#token=...). EmailVerificationTTL is the link lifetime.
	EmailVerificationURL string
	EmailVerificationTTL time.Duration

	// RentalReservationTTL is how long a number reservation holds before it
	// is eligible for expiry (freeing the number back to AVAILABLE).
	RentalReservationTTL time.Duration

	// ProviderCredentialKeys are the AES-256 keys (32 raw bytes each) used to
	// encrypt provider_configs.credentials_ciphertext, keyed by version.
	// ProviderCredentialKeyVersion names which key new encryptions use.
	// Rotation is manual: add a new PROVIDER_CREDENTIAL_KEYS entry, point
	// PROVIDER_CREDENTIAL_KEY_VERSION at it, re-encrypt existing rows, then
	// drop the old entry once nothing references it anymore.
	ProviderCredentialKeys       map[string][]byte
	ProviderCredentialKeyVersion string
}

const minSecretLen = 32

// Load reads configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		AppEnv:      get("APP_ENV", "development"),
		Port:        get("PORT", "8080"),
		DatabaseURL: get("DATABASE_URL", ""),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if _, err := strconv.Atoi(cfg.Port); err != nil {
		errs = append(errs, fmt.Errorf("PORT must be a number: %q", cfg.Port))
	}

	secret := get("ACCESS_TOKEN_SECRET", "")
	switch {
	case secret == "" || secret == "CHANGE_ME":
		errs = append(errs, errors.New("ACCESS_TOKEN_SECRET is required (generate with: openssl rand -hex 32)"))
	case len(secret) < minSecretLen:
		errs = append(errs, fmt.Errorf("ACCESS_TOKEN_SECRET must be at least %d characters", minSecretLen))
	default:
		cfg.AccessTokenSecret = []byte(secret)
	}

	var err error
	if cfg.AccessTokenTTL, err = parseDuration(get("ACCESS_TOKEN_TTL", "15m"), "ACCESS_TOKEN_TTL"); err != nil {
		errs = append(errs, err)
	}
	if cfg.RefreshTokenTTL, err = parseDuration(get("REFRESH_TOKEN_TTL", "720h"), "REFRESH_TOKEN_TTL"); err != nil {
		errs = append(errs, err)
	}

	cfg.AutoMigrate = cfg.AppEnv == "development"
	if v := get("AUTO_MIGRATE", ""); v != "" {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			errs = append(errs, fmt.Errorf("AUTO_MIGRATE must be a boolean: %q", v))
		}
		cfg.AutoMigrate = b
	}

	origins := get("FRONTEND_URL", "")
	if origins == "" && cfg.AppEnv == "development" {
		origins = "http://localhost:3000,http://localhost:5173"
	}
	if cfg.AllowedOrigins, err = parseOrigins(origins, cfg.AppEnv); err != nil {
		errs = append(errs, err)
	}
	if cfg.TrustedProxies, err = parseProxies(get("TRUSTED_PROXIES", "")); err != nil {
		errs = append(errs, err)
	}

	cfg.RateLimitEnabled = true
	if v := get("RATE_LIMIT_ENABLED", ""); v != "" {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_ENABLED must be a boolean: %q", v))
		}
		cfg.RateLimitEnabled = b
	}

	// --- email + password reset ---
	cfg.ResendAPIKey = get("RESEND_API_KEY", "")
	mailFrom := get("MAIL_FROM", "")
	if cfg.ResendAPIKey == "" && cfg.AppEnv != "development" {
		errs = append(errs, errors.New("RESEND_API_KEY is required outside development (transactional email needs delivery)"))
	}
	if cfg.ResendAPIKey != "" && mailFrom == "" {
		errs = append(errs, errors.New("MAIL_FROM is required when RESEND_API_KEY is set"))
	} else if mailFrom != "" {
		if addr, perr := mail.ParseAddress(mailFrom); perr != nil || strings.ContainsAny(mailFrom, "\r\n") {
			errs = append(errs, fmt.Errorf("MAIL_FROM is not a valid address: %q", mailFrom))
		} else {
			cfg.MailFrom = addr.String()
		}
	}

	if cfg.PasswordResetTTL, err = parseDuration(get("PASSWORD_RESET_TTL", "30m"), "PASSWORD_RESET_TTL"); err != nil {
		errs = append(errs, err)
	} else if cfg.PasswordResetTTL < 5*time.Minute || cfg.PasswordResetTTL > 24*time.Hour {
		errs = append(errs, errors.New("PASSWORD_RESET_TTL must be between 5m and 24h"))
	}

	resetURL := get("PASSWORD_RESET_URL", "")
	if resetURL == "" && len(cfg.AllowedOrigins) > 0 {
		resetURL = cfg.AllowedOrigins[0] + "/reset-password"
	}
	if resetURL == "" {
		errs = append(errs, errors.New("PASSWORD_RESET_URL (or FRONTEND_URL) is required so reset emails can link to your frontend"))
	} else if cfg.PasswordResetURL, err = parseFrontendURL(resetURL, cfg.AppEnv, "PASSWORD_RESET_URL"); err != nil {
		errs = append(errs, err)
	}

	if cfg.EmailVerificationTTL, err = parseDuration(get("EMAIL_VERIFICATION_TTL", "24h"), "EMAIL_VERIFICATION_TTL"); err != nil {
		errs = append(errs, err)
	} else if cfg.EmailVerificationTTL < time.Hour || cfg.EmailVerificationTTL > 7*24*time.Hour {
		errs = append(errs, errors.New("EMAIL_VERIFICATION_TTL must be between 1h and 168h"))
	}

	if cfg.RentalReservationTTL, err = parseDuration(get("RENTAL_RESERVATION_TTL", "15m"), "RENTAL_RESERVATION_TTL"); err != nil {
		errs = append(errs, err)
	} else if cfg.RentalReservationTTL < time.Minute || cfg.RentalReservationTTL > time.Hour {
		errs = append(errs, errors.New("RENTAL_RESERVATION_TTL must be between 1m and 1h"))
	}
	verifyURL := get("EMAIL_VERIFICATION_URL", "")
	if verifyURL == "" && len(cfg.AllowedOrigins) > 0 {
		verifyURL = cfg.AllowedOrigins[0] + "/verify-email"
	}
	if verifyURL == "" {
		errs = append(errs, errors.New("EMAIL_VERIFICATION_URL (or FRONTEND_URL) is required so verification emails can link to your frontend"))
	} else if cfg.EmailVerificationURL, err = parseFrontendURL(verifyURL, cfg.AppEnv, "EMAIL_VERIFICATION_URL"); err != nil {
		errs = append(errs, err)
	}

	cfg.ProviderCredentialKeys, cfg.ProviderCredentialKeyVersion, err = parseProviderCredentialKeys(
		get("PROVIDER_CREDENTIAL_KEYS", ""), get("PROVIDER_CREDENTIAL_KEY_VERSION", ""))
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// parseProviderCredentialKeys parses "v1:base64key,v2:base64key" into a
// version->key map. Both arguments may be empty, meaning provider credential
// encryption is not configured (fine until an admin tries to store one).
func parseProviderCredentialKeys(raw, currentVersion string) (map[string][]byte, string, error) {
	if strings.TrimSpace(raw) == "" {
		if currentVersion != "" {
			return nil, "", errors.New("PROVIDER_CREDENTIAL_KEY_VERSION set without any PROVIDER_CREDENTIAL_KEYS")
		}
		return nil, "", nil
	}
	keys := map[string][]byte{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		version, encoded, ok := strings.Cut(entry, ":")
		version = strings.TrimSpace(version)
		if !ok || version == "" || encoded == "" {
			return nil, "", fmt.Errorf("PROVIDER_CREDENTIAL_KEYS entry %q must be formatted version:base64key", entry)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, "", fmt.Errorf("PROVIDER_CREDENTIAL_KEYS entry %q: %w", version, err)
		}
		if len(key) != 32 {
			return nil, "", fmt.Errorf("PROVIDER_CREDENTIAL_KEYS entry %q: key must decode to 32 bytes, got %d", version, len(key))
		}
		keys[version] = key
	}
	if currentVersion == "" {
		return nil, "", errors.New("PROVIDER_CREDENTIAL_KEY_VERSION is required when PROVIDER_CREDENTIAL_KEYS is set")
	}
	if _, ok := keys[currentVersion]; !ok {
		return nil, "", fmt.Errorf("PROVIDER_CREDENTIAL_KEY_VERSION %q has no matching PROVIDER_CREDENTIAL_KEYS entry", currentVersion)
	}
	return keys, currentVersion, nil
}

func parseDuration(s, name string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 15m or 720h: %q", name, s)
	}
	return d, nil
}

// parseOrigins validates a comma-separated list of CORS origins such as
// "https://app.example.com". Wildcards, paths and credentials are rejected,
// and plain http is only accepted for loopback hosts outside development.
func parseOrigins(raw, appEnv string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("FRONTEND_URL entry %q must be an origin like https://app.example.com", p)
		}
		if u.Scheme == "http" && appEnv != "development" && !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("FRONTEND_URL entry %q must use https outside development", p)
		}
		origin := u.Scheme + "://" + strings.ToLower(u.Host)
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	return out, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseProxies parses a comma-separated list of IPs or CIDRs.
func parseProxies(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			ip := net.ParseIP(p)
			if ip == nil {
				return nil, fmt.Errorf("TRUSTED_PROXIES entry %q is not a valid IP or CIDR", p)
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES entry %q is not a valid IP or CIDR", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func parseFrontendURL(raw, appEnv, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%s %q must be an absolute http(s) URL without a fragment", name, raw)
	}
	if u.Scheme == "http" && appEnv != "development" && !isLoopback(u.Hostname()) {
		return "", fmt.Errorf("%s %q must use https outside development", name, raw)
	}
	return u.String(), nil
}
