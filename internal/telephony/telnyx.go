package telephony

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const telnyxAPIBaseURL = "https://api.telnyx.com/v2"

type Telnyx struct {
	apiKey, messagingProfileID string
	publicKey                  ed25519.PublicKey
	baseURL                    string
	client                     *http.Client
}

var _ Provider = (*Telnyx)(nil)
var _ InboundMessageParser = (*Telnyx)(nil)

func NewTelnyx(apiKey, publicKey, messagingProfileID string) (*Telnyx, error) {
	return NewTelnyxWithBaseURL(apiKey, publicKey, messagingProfileID, telnyxAPIBaseURL, &http.Client{Timeout: 10 * time.Second})
}

func NewTelnyxWithBaseURL(apiKey, publicKey, messagingProfileID, baseURL string, client *http.Client) (*Telnyx, error) {
	key, err := decodeEd25519Key(publicKey)
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(messagingProfileID) == "" || err != nil || client == nil {
		return nil, fmt.Errorf("%w: valid Telnyx API key, public key, messaging profile and HTTP client are required", ErrInvalidRequest)
	}
	return &Telnyx{apiKey: apiKey, publicKey: key, messagingProfileID: messagingProfileID, baseURL: strings.TrimRight(baseURL, "/"), client: client}, nil
}

func decodeEd25519Key(value string) (ed25519.PublicKey, error) {
	value = strings.TrimSpace(value)
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		b, err = hex.DecodeString(value)
	}
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errorsNew("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

func (t *Telnyx) SearchNumbers(ctx context.Context, in SearchNumbersRequest) (NumberSearchPage, error) {
	country := strings.ToUpper(strings.TrimSpace(in.CountryCode))
	if len(country) != 2 {
		return NumberSearchPage{}, ErrInvalidRequest
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
	q := url.Values{"filter[country_code]": {country}, "page[size]": {strconv.Itoa(size)}, "page[number]": {strconv.Itoa(page)}}
	if in.AreaCode != "" {
		q.Set("filter[national_destination_code]", in.AreaCode)
	}
	features := []string{}
	if in.Require.SMS {
		features = append(features, "sms")
	}
	if in.Require.MMS {
		features = append(features, "mms")
	}
	if in.Require.Voice {
		features = append(features, "voice")
	}
	if len(features) > 0 {
		q.Set("filter[features]", strings.Join(features, ","))
	}
	var out struct {
		Data []struct {
			PhoneNumber string `json:"phone_number"`
			RecordType  string `json:"record_type"`
			Features    []struct {
				Name string `json:"name"`
			} `json:"features"`
		} `json:"data"`
		Meta struct {
			PageNumber int `json:"page_number"`
			TotalPages int `json:"total_pages"`
		} `json:"meta"`
	}
	if err := t.request(ctx, http.MethodGet, "/available_phone_numbers", q, nil, &out); err != nil {
		return NumberSearchPage{}, err
	}
	result := NumberSearchPage{Numbers: make([]AvailableNumber, 0, len(out.Data))}
	for _, n := range out.Data {
		caps := Capabilities{}
		for _, f := range n.Features {
			switch strings.ToLower(f.Name) {
			case "sms":
				caps.SMS = true
			case "mms":
				caps.MMS = true
			case "voice":
				caps.Voice = true
			}
		}
		result.Numbers = append(result.Numbers, AvailableNumber{PhoneNumber: n.PhoneNumber, Name: n.RecordType, Capabilities: caps})
	}
	if out.Meta.PageNumber < out.Meta.TotalPages {
		result.NextCursor = strconv.Itoa(out.Meta.PageNumber + 1)
	}
	return result, nil
}

func (t *Telnyx) ProvisionNumber(ctx context.Context, in ProvisionNumberRequest) (Number, error) {
	if strings.TrimSpace(in.PhoneNumber) == "" {
		return Number{}, ErrInvalidRequest
	}
	payload := map[string]any{"phone_numbers": []map[string]string{{"phone_number": in.PhoneNumber}}, "messaging_profile_id": t.messagingProfileID}
	var out struct {
		Data struct {
			ID           string `json:"id"`
			PhoneNumbers []struct {
				ID          string `json:"id"`
				PhoneNumber string `json:"phone_number"`
			} `json:"phone_numbers"`
		} `json:"data"`
	}
	if err := t.request(ctx, http.MethodPost, "/number_orders", nil, payload, &out); err != nil {
		return Number{}, err
	}
	if out.Data.ID == "" {
		return Number{}, fmt.Errorf("%w: incomplete Telnyx number order", ErrProviderRejected)
	}
	phone := in.PhoneNumber
	providerReference := out.Data.ID
	if len(out.Data.PhoneNumbers) > 0 && out.Data.PhoneNumbers[0].PhoneNumber != "" {
		phone = out.Data.PhoneNumbers[0].PhoneNumber
		if out.Data.PhoneNumbers[0].ID != "" {
			providerReference = out.Data.PhoneNumbers[0].ID
		}
	}
	return Number{ProviderReference: providerReference, PhoneNumber: phone}, nil
}

func (t *Telnyx) ReleaseNumber(ctx context.Context, ref string) error {
	if strings.TrimSpace(ref) == "" {
		return ErrInvalidRequest
	}
	return t.request(ctx, http.MethodDelete, "/phone_numbers/"+url.PathEscape(ref), nil, nil, nil)
}

func (t *Telnyx) SendMessage(ctx context.Context, in SendMessageRequest) (MessageReceipt, error) {
	if strings.TrimSpace(in.From) == "" || strings.TrimSpace(in.To) == "" || strings.TrimSpace(in.Body) == "" {
		return MessageReceipt{}, ErrInvalidRequest
	}
	payload := map[string]string{"from": in.From, "to": in.To, "text": in.Body, "messaging_profile_id": t.messagingProfileID}
	var out struct {
		Data struct{ ID, Direction, Type string } `json:"data"`
	}
	if err := t.request(ctx, http.MethodPost, "/messages", nil, payload, &out); err != nil {
		return MessageReceipt{}, err
	}
	if out.Data.ID == "" {
		return MessageReceipt{}, fmt.Errorf("%w: incomplete Telnyx message response", ErrProviderRejected)
	}
	return MessageReceipt{ProviderReference: out.Data.ID, Status: "queued"}, nil
}

func (t *Telnyx) ParseInboundMessage(_ context.Context, webhook InboundWebhook) (InboundMessage, error) {
	if len(webhook.Body) > MaxInboundWebhookBytes {
		return InboundMessage{}, ErrInvalidRequest
	}
	parts := strings.SplitN(webhook.Signature, ".", 2)
	if len(parts) != 2 {
		return InboundMessage{}, ErrInvalidWebhookSignature
	}
	timestamp, signature := parts[0], parts[1]
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return InboundMessage{}, ErrInvalidWebhookSignature
	}
	message := append([]byte(timestamp+"|"), webhook.Body...)
	if !ed25519.Verify(t.publicKey, message, sig) {
		return InboundMessage{}, ErrInvalidWebhookSignature
	}
	var event struct {
		Data struct {
			EventType string `json:"event_type"`
			ID        string `json:"id"`
			Payload   struct {
				ID, Text string
				From, To struct {
					PhoneNumber string `json:"phone_number"`
				}
			} `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(webhook.Body, &event); err != nil || event.Data.EventType != "message.received" {
		return InboundMessage{}, ErrInvalidRequest
	}
	id := event.Data.Payload.ID
	if id == "" {
		id = event.Data.ID
	}
	if id == "" || event.Data.Payload.From.PhoneNumber == "" || event.Data.Payload.To.PhoneNumber == "" {
		return InboundMessage{}, ErrInvalidRequest
	}
	return InboundMessage{ProviderReference: id, From: event.Data.Payload.From.PhoneNumber, To: event.Data.Payload.To.PhoneNumber, Body: event.Data.Payload.Text}, nil
}

func (t *Telnyx) request(ctx context.Context, method, path string, query url.Values, payload, result any) error {
	endpoint := t.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return ErrInvalidRequest
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return ErrInvalidRequest
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: Telnyx request failed", ErrProviderUnavailable)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ErrProviderUnavailable
	}
	if len(b) > maxResponseBytes {
		return ErrProviderRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providerHTTPError(resp.StatusCode)
	}
	if result != nil && len(b) > 0 {
		if err := json.Unmarshal(b, result); err != nil {
			return fmt.Errorf("%w: malformed Telnyx response", ErrProviderRejected)
		}
	}
	return nil
}

func errorsNew(message string) error { return fmt.Errorf("%s", message) }
