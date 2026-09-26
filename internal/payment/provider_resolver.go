package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"migo/internal/paystack"
	"migo/internal/providerconfig"
)

const paystackProviderKey = "paystack"

// ProviderConfigResolver resolves the currently enabled Paystack
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
// the Paystack API instead of the real production endpoint. For tests only.
func (r *ProviderConfigResolver) SetBaseURL(baseURL string, httpClient *http.Client) {
	r.baseURL = baseURL
	r.httpClient = httpClient
}

func (r *ProviderConfigResolver) Resolve(ctx context.Context) (*paystack.Client, string, error) {
	configs, err := r.svc.List(ctx, providerconfig.KindPayment)
	if err != nil {
		return nil, "", err
	}
	for _, cfg := range configs {
		if cfg.Key != paystackProviderKey || !cfg.IsEnabled {
			continue
		}
		plaintext, err := r.svc.DecryptedCredentials(ctx, cfg.ID)
		if err != nil {
			return nil, "", err
		}
		var creds struct {
			SecretKey string `json:"secret_key"`
		}
		if err := json.Unmarshal(plaintext, &creds); err != nil {
			return nil, "", fmt.Errorf("%w: stored Paystack credentials are malformed", ErrProviderConfigMissing)
		}
		var client *paystack.Client
		if r.baseURL != "" {
			client, err = paystack.NewClientWithBaseURL(creds.SecretKey, r.baseURL, r.httpClient)
		} else {
			client, err = paystack.NewClient(creds.SecretKey)
		}
		if err != nil {
			return nil, "", err
		}
		return client, cfg.ID, nil
	}
	return nil, "", ErrProviderConfigMissing
}
