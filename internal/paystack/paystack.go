// Package paystack is a minimal client for the Paystack transaction API:
// initializing a checkout, server-side verification, and webhook signature
// validation. It never trusts a webhook body as proof of payment on its
// own — Verify always calls Paystack directly.
package paystack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrInvalidRequest          = errors.New("paystack: invalid request")
	ErrProviderUnavailable     = errors.New("paystack: provider unavailable")
	ErrProviderRejected        = errors.New("paystack: provider rejected request")
	ErrInvalidWebhookSignature = errors.New("paystack: invalid webhook signature")
)

const (
	defaultBaseURL   = "https://api.paystack.co"
	maxResponseBytes = 1 << 20
)

type Client struct {
	secretKey string
	baseURL   string
	client    *http.Client
}

func NewClient(secretKey string) (*Client, error) {
	return NewClientWithBaseURL(secretKey, defaultBaseURL, &http.Client{Timeout: 10 * time.Second})
}

// NewClientWithBaseURL is for pointing at a test double of the Paystack API
// (e.g. httptest.NewServer) from other packages' tests.
func NewClientWithBaseURL(secretKey, baseURL string, httpClient *http.Client) (*Client, error) {
	secretKey = strings.TrimSpace(secretKey)
	if secretKey == "" {
		return nil, fmt.Errorf("%w: a Paystack secret key is required", ErrInvalidRequest)
	}
	return &Client{secretKey: secretKey, baseURL: baseURL, client: httpClient}, nil
}

// VerifySignature checks the X-Paystack-Signature header (hex-encoded
// HMAC-SHA512 of the raw request body under the secret key) in constant time.
func (c *Client) VerifySignature(body []byte, signatureHeader string) bool {
	if signatureHeader == "" {
		return false
	}
	given, err := hex.DecodeString(signatureHeader)
	if err != nil {
		return false
	}
	mac := hmac.New(sha512.New, []byte(c.secretKey))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), given)
}

type InitializeRequest struct {
	Email            string
	AmountMinorUnits int64
	Currency         string
	Reference        string
	CallbackURL      string
}

type InitializeResult struct {
	AuthorizationURL string
	AccessCode       string
	Reference        string
}

func (c *Client) Initialize(ctx context.Context, req InitializeRequest) (InitializeResult, error) {
	req.Email = strings.TrimSpace(req.Email)
	req.Currency = strings.TrimSpace(strings.ToUpper(req.Currency))
	req.Reference = strings.TrimSpace(req.Reference)
	if req.Email == "" || req.AmountMinorUnits <= 0 || req.Currency == "" || req.Reference == "" {
		return InitializeResult{}, fmt.Errorf("%w: email, amount, currency and reference are required", ErrInvalidRequest)
	}
	payload := map[string]any{
		"email":     req.Email,
		"amount":    req.AmountMinorUnits,
		"currency":  req.Currency,
		"reference": req.Reference,
	}
	if req.CallbackURL != "" {
		payload["callback_url"] = req.CallbackURL
	}
	var response struct {
		Status  bool   `json:"status"`
		Message string `json:"message"`
		Data    struct {
			AuthorizationURL string `json:"authorization_url"`
			AccessCode       string `json:"access_code"`
			Reference        string `json:"reference"`
		} `json:"data"`
	}
	if err := c.request(ctx, http.MethodPost, "/transaction/initialize", payload, &response); err != nil {
		return InitializeResult{}, err
	}
	if !response.Status {
		return InitializeResult{}, fmt.Errorf("%w: %s", ErrProviderRejected, response.Message)
	}
	return InitializeResult{
		AuthorizationURL: response.Data.AuthorizationURL,
		AccessCode:       response.Data.AccessCode,
		Reference:        response.Data.Reference,
	}, nil
}

type VerifyResult struct {
	Status           string // Paystack's transaction status: success, failed, abandoned, ...
	AmountMinorUnits int64
	Currency         string
	Reference        string
}

func (c *Client) Verify(ctx context.Context, reference string) (VerifyResult, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return VerifyResult{}, fmt.Errorf("%w: reference is required", ErrInvalidRequest)
	}
	var response struct {
		Status  bool   `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Status    string `json:"status"`
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
			Reference string `json:"reference"`
		} `json:"data"`
	}
	path := "/transaction/verify/" + pathEscapeReference(reference)
	if err := c.request(ctx, http.MethodGet, path, nil, &response); err != nil {
		return VerifyResult{}, err
	}
	if !response.Status {
		return VerifyResult{}, fmt.Errorf("%w: %s", ErrProviderRejected, response.Message)
	}
	return VerifyResult{
		Status:           response.Data.Status,
		AmountMinorUnits: response.Data.Amount,
		Currency:         response.Data.Currency,
		Reference:        response.Data.Reference,
	}, nil
}

func pathEscapeReference(reference string) string {
	// References are our own generated UUIDs; still escape defensively.
	escaped := strings.ReplaceAll(reference, "/", "%2F")
	return strings.ReplaceAll(escaped, " ", "%20")
}

func (c *Client) request(ctx context.Context, method, path string, payload any, result any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: could not encode request body", ErrInvalidRequest)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("%w: could not create provider request", ErrInvalidRequest)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: Paystack request failed: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: could not read provider response", ErrProviderUnavailable)
	}
	if len(responseBody) > maxResponseBytes {
		return fmt.Errorf("%w: oversized provider response", ErrProviderRejected)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%w: HTTP %d", ErrProviderUnavailable, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: HTTP %d", ErrProviderUnavailable, resp.StatusCode)
	}
	// 4xx bodies (other than 429) still decode: Paystack returns
	// {"status":false,"message":"..."} which callers turn into
	// ErrProviderRejected with the message intact.
	if err := json.Unmarshal(responseBody, result); err != nil {
		return fmt.Errorf("%w: could not parse provider response", ErrProviderUnavailable)
	}
	return nil
}
