package telephony

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const vonageAPIBaseURL = "https://rest.nexmo.com"

type Vonage struct {
	apiKey, apiSecret, signatureSecret string
	defaultCountry                     string
	baseURL                            string
	client                             *http.Client
}

var _ Provider = (*Vonage)(nil)
var _ InboundMessageParser = (*Vonage)(nil)

func NewVonage(apiKey, apiSecret, signatureSecret, defaultCountry string) (*Vonage, error) {
	return NewVonageWithBaseURL(apiKey, apiSecret, signatureSecret, defaultCountry, vonageAPIBaseURL, &http.Client{Timeout: 10 * time.Second})
}

func NewVonageWithBaseURL(apiKey, apiSecret, signatureSecret, defaultCountry, baseURL string, client *http.Client) (*Vonage, error) {
	defaultCountry = strings.ToUpper(strings.TrimSpace(defaultCountry))
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(apiSecret) == "" || strings.TrimSpace(signatureSecret) == "" || len(defaultCountry) != 2 || client == nil {
		return nil, fmt.Errorf("%w: Vonage API key, API secret, signature secret, default country and HTTP client are required", ErrInvalidRequest)
	}
	return &Vonage{apiKey: apiKey, apiSecret: apiSecret, signatureSecret: signatureSecret, defaultCountry: defaultCountry, baseURL: strings.TrimRight(baseURL, "/"), client: client}, nil
}

func (v *Vonage) SearchNumbers(ctx context.Context, in SearchNumbersRequest) (NumberSearchPage, error) {
	country := strings.ToUpper(strings.TrimSpace(in.CountryCode))
	if len(country) != 2 {
		return NumberSearchPage{}, fmt.Errorf("%w: country code must be two letters", ErrInvalidRequest)
	}
	size := in.PageSize
	if size == 0 {
		size = 50
	}
	if size < 1 || size > 100 {
		return NumberSearchPage{}, ErrInvalidRequest
	}
	page := 1
	if in.Cursor != "" {
		n, err := strconv.Atoi(in.Cursor)
		if err != nil || n < 1 {
			return NumberSearchPage{}, ErrInvalidRequest
		}
		page = n
	}
	q := url.Values{"country": {country}, "size": {strconv.Itoa(size)}, "index": {strconv.Itoa(page)}}
	if in.AreaCode != "" {
		q.Set("pattern", in.AreaCode)
		q.Set("search_pattern", "1")
	}
	if in.Require.SMS {
		q.Set("features", "SMS")
	}
	var out struct {
		Count   int `json:"count"`
		Numbers []struct {
			MSISDN, Type string
			Features     []string `json:"features"`
		} `json:"numbers"`
	}
	if err := v.request(ctx, http.MethodGet, "/number/search", q, &out); err != nil {
		return NumberSearchPage{}, err
	}
	result := NumberSearchPage{Numbers: make([]AvailableNumber, 0, len(out.Numbers))}
	for _, n := range out.Numbers {
		caps := Capabilities{}
		for _, f := range n.Features {
			switch strings.ToUpper(f) {
			case "SMS":
				caps.SMS = true
			case "MMS":
				caps.MMS = true
			case "VOICE":
				caps.Voice = true
			}
		}
		if in.Require.SMS && !caps.SMS || in.Require.MMS && !caps.MMS || in.Require.Voice && !caps.Voice {
			continue
		}
		phone := n.MSISDN
		if !strings.HasPrefix(phone, "+") {
			phone = "+" + phone
		}
		result.Numbers = append(result.Numbers, AvailableNumber{PhoneNumber: phone, Name: n.Type, Capabilities: caps})
	}
	if len(out.Numbers) == size {
		result.NextCursor = strconv.Itoa(page + 1)
	}
	return result, nil
}

func (v *Vonage) ProvisionNumber(ctx context.Context, in ProvisionNumberRequest) (Number, error) {
	phone := strings.TrimPrefix(strings.TrimSpace(in.PhoneNumber), "+")
	if phone == "" {
		return Number{}, ErrInvalidRequest
	}
	q := url.Values{"country": {v.defaultCountry}, "msisdn": {phone}}
	if err := v.request(ctx, http.MethodPost, "/number/buy", q, nil); err != nil {
		return Number{}, err
	}
	return Number{ProviderReference: phone, PhoneNumber: "+" + phone}, nil
}

func (v *Vonage) ReleaseNumber(ctx context.Context, ref string) error {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "+")
	if ref == "" {
		return ErrInvalidRequest
	}
	return v.request(ctx, http.MethodPost, "/number/cancel", url.Values{"country": {v.defaultCountry}, "msisdn": {ref}}, nil)
}

func (v *Vonage) SendMessage(ctx context.Context, in SendMessageRequest) (MessageReceipt, error) {
	if strings.TrimSpace(in.From) == "" || strings.TrimSpace(in.To) == "" || strings.TrimSpace(in.Body) == "" {
		return MessageReceipt{}, ErrInvalidRequest
	}
	q := url.Values{"from": {strings.TrimPrefix(in.From, "+")}, "to": {strings.TrimPrefix(in.To, "+")}, "text": {in.Body}}
	if in.StatusCallbackURL != "" {
		q.Set("callback", in.StatusCallbackURL)
	}
	var out struct {
		Messages []struct {
			ID     string `json:"message-id"`
			Status string `json:"status"`
		} `json:"messages"`
	}
	if err := v.request(ctx, http.MethodPost, "/sms/json", q, &out); err != nil {
		return MessageReceipt{}, err
	}
	if len(out.Messages) == 0 || out.Messages[0].ID == "" || out.Messages[0].Status != "0" {
		return MessageReceipt{}, fmt.Errorf("%w: Vonage rejected message", ErrProviderRejected)
	}
	return MessageReceipt{ProviderReference: out.Messages[0].ID, Status: "queued"}, nil
}

func (v *Vonage) ParseInboundMessage(_ context.Context, webhook InboundWebhook) (InboundMessage, error) {
	if len(webhook.Body) > MaxInboundWebhookBytes {
		return InboundMessage{}, ErrInvalidRequest
	}
	values, err := url.ParseQuery(string(webhook.Body))
	if err != nil {
		return InboundMessage{}, ErrInvalidRequest
	}
	sig := values.Get("sig")
	if sig == "" {
		sig = webhook.Signature
	}
	if !v.validSignature(values, sig) {
		return InboundMessage{}, ErrInvalidWebhookSignature
	}
	id, from, to, body := values.Get("messageId"), values.Get("msisdn"), values.Get("to"), values.Get("text")
	if id == "" || from == "" || to == "" {
		return InboundMessage{}, ErrInvalidRequest
	}
	if !strings.HasPrefix(from, "+") {
		from = "+" + from
	}
	if !strings.HasPrefix(to, "+") {
		to = "+" + to
	}
	return InboundMessage{ProviderReference: id, From: from, To: to, Body: body}, nil
}

func (v *Vonage) validSignature(values url.Values, supplied string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(supplied))
	if err != nil {
		return false
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if k != "sig" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteByte('&')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values.Get(k))
	}
	mac := hmac.New(sha256.New, []byte(v.signatureSecret))
	_, _ = mac.Write([]byte(b.String()))
	return hmac.Equal(decoded, mac.Sum(nil))
}

func (v *Vonage) request(ctx context.Context, method, path string, values url.Values, result any) error {
	values.Set("api_key", v.apiKey)
	values.Set("api_secret", v.apiSecret)
	endpoint := v.baseURL + path
	var body io.Reader
	if method == http.MethodGet {
		endpoint += "?" + values.Encode()
	} else {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return ErrInvalidRequest
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: Vonage request failed", ErrProviderUnavailable)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ErrProviderUnavailable
	}
	if len(payload) > maxResponseBytes {
		return ErrProviderRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providerHTTPError(resp.StatusCode)
	}
	if result != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, result); err != nil {
			return fmt.Errorf("%w: malformed Vonage response", ErrProviderRejected)
		}
	}
	return nil
}
