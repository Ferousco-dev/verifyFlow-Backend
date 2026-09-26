package fulfillment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"migo/internal/providerconfig"
	"migo/internal/telephony"
)

const twilioProviderKey = "twilio"

// ProviderConfigResolver resolves the currently enabled Twilio
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

func (r *ProviderConfigResolver) Resolve(ctx context.Context) (telephony.Provider, error) {
	configs, err := r.svc.List(ctx, providerconfig.KindTelephony)
	if err != nil {
		return nil, err
	}
	for _, cfg := range configs {
		if cfg.Key != twilioProviderKey || !cfg.IsEnabled {
			continue
		}
		plaintext, err := r.svc.DecryptedCredentials(ctx, cfg.ID)
		if err != nil {
			return nil, err
		}
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
	}
	return nil, ErrProviderConfigMissing
}
