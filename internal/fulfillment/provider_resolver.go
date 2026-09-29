package fulfillment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"migo/internal/providerconfig"
	"migo/internal/telephony"
)

// ProviderConfigResolver resolves an enabled telephony
// provider_configs row into a live client, decrypting its credentials on
// every call so admin key rotation and enable/disable take effect
// immediately without a restart.
type ProviderConfigResolver struct {
	svc        *providerconfig.Service
	baseURL    string
	httpClient *http.Client
}

func NewProviderConfigResolver(svc *providerconfig.Service) *ProviderConfigResolver {
	return &ProviderConfigResolver{svc: svc}
}

// SetBaseURL points every client this resolver builds at a test double of
// the Twilio API instead of the real production endpoint. For tests only.
func (r *ProviderConfigResolver) SetBaseURL(baseURL string, httpClient *http.Client) {
	r.baseURL = baseURL
	r.httpClient = httpClient
}

func (r *ProviderConfigResolver) Resolve(ctx context.Context, providerConfigID string) (telephony.Provider, error) {
	if providerConfigID == "" {
		return nil, ErrProviderConfigMissing
	}
	cfg, err := r.svc.Get(ctx, providerConfigID)
	if err != nil {
		return nil, err
	}
	if cfg.Kind != providerconfig.KindTelephony || !cfg.IsEnabled {
		return nil, ErrProviderConfigMissing
	}
	plaintext, err := r.svc.DecryptedCredentials(ctx, cfg.ID)
	if err != nil {
		return nil, err
	}
	switch cfg.Key {
	case "twilio":
		var creds struct {
			AccountSID string `json:"account_sid"`
			AuthToken  string `json:"auth_token"`
		}
		if err := json.Unmarshal(plaintext, &creds); err != nil {
			return nil, fmt.Errorf("%w: stored Twilio credentials are malformed", ErrProviderConfigMissing)
		}
		if r.baseURL != "" {
			provider, err := telephony.NewTwilioWithBaseURL(creds.AccountSID, creds.AuthToken, r.baseURL, r.httpClient)
			if err != nil {
				return nil, err
			}
			return provider, nil
		}
		provider, err := telephony.NewTwilio(creds.AccountSID, creds.AuthToken)
		if err != nil {
			return nil, err
		}
		return provider, nil
	case "vonage":
		var creds struct {
			APIKey          string `json:"api_key"`
			APISecret       string `json:"api_secret"`
			SignatureSecret string `json:"signature_secret"`
			DefaultCountry  string `json:"default_country"`
		}
		if err := json.Unmarshal(plaintext, &creds); err != nil {
			return nil, fmt.Errorf("%w: stored Vonage credentials are malformed", ErrProviderConfigMissing)
		}
		if r.baseURL != "" {
			return telephony.NewVonageWithBaseURL(creds.APIKey, creds.APISecret, creds.SignatureSecret, creds.DefaultCountry, r.baseURL, r.httpClient)
		}
		return telephony.NewVonage(creds.APIKey, creds.APISecret, creds.SignatureSecret, creds.DefaultCountry)
	case "telnyx":
		var creds struct {
			APIKey             string `json:"api_key"`
			PublicKey          string `json:"public_key"`
			MessagingProfileID string `json:"messaging_profile_id"`
		}
		if err := json.Unmarshal(plaintext, &creds); err != nil {
			return nil, fmt.Errorf("%w: stored Telnyx credentials are malformed", ErrProviderConfigMissing)
		}
		if r.baseURL != "" {
			return telephony.NewTelnyxWithBaseURL(creds.APIKey, creds.PublicKey, creds.MessagingProfileID, r.baseURL, r.httpClient)
		}
		return telephony.NewTelnyx(creds.APIKey, creds.PublicKey, creds.MessagingProfileID)
	default:
		return nil, fmt.Errorf("%w: unsupported telephony provider %q", ErrProviderConfigMissing, cfg.Key)
	}
}
