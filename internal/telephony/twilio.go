package telephony

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	twilioclient "github.com/twilio/twilio-go/client"
)

const (
	twilioAPIBaseURL = "https://api.twilio.com"
	twilioAPIVersion = "2010-04-01"
	maxResponseBytes = 1 << 20
	twilioPageToken  = "token:"
	twilioPageNumber = "page:"
)

type Twilio struct {
	accountSID string
	authToken  string
	client     *http.Client
	baseURL    string
	validator  twilioclient.RequestValidator
}

var _ Provider = (*Twilio)(nil)
var _ InboundMessageParser = (*Twilio)(nil)
var _ WebhookConfigurer = (*Twilio)(nil)

func NewTwilio(accountSID, authToken string) (*Twilio, error) {
	return NewTwilioWithBaseURL(accountSID, authToken, twilioAPIBaseURL, &http.Client{Timeout: 10 * time.Second})
}

// NewTwilioWithBaseURL is for pointing at a test double of the Twilio API
// (e.g. httptest.NewServer) from other packages' tests.
func NewTwilioWithBaseURL(accountSID, authToken, baseURL string, httpClient *http.Client) (*Twilio, error) {
	accountSID = strings.TrimSpace(accountSID)
	authToken = strings.TrimSpace(authToken)
	if accountSID == "" || authToken == "" {
		return nil, fmt.Errorf("%w: Twilio account SID and auth token are required", ErrInvalidRequest)
	}
	return &Twilio{
		accountSID: accountSID,
		authToken:  authToken,
		client:     httpClient,
		baseURL:    baseURL,
		validator:  twilioclient.NewRequestValidator(authToken),
	}, nil
}

func (t *Twilio) SearchNumbers(ctx context.Context, request SearchNumbersRequest) (NumberSearchPage, error) {
	countryCode := strings.ToUpper(strings.TrimSpace(request.CountryCode))
	if len(countryCode) != 2 || countryCode[0] < 'A' || countryCode[0] > 'Z' || countryCode[1] < 'A' || countryCode[1] > 'Z' {
		return NumberSearchPage{}, fmt.Errorf("%w: country code must be two letters", ErrInvalidRequest)
	}
	numberType := request.Type
	if numberType == "" {
		numberType = NumberTypeLocal
	}
	switch numberType {
	case NumberTypeLocal, NumberTypeTollFree, NumberTypeMobile:
	default:
		return NumberSearchPage{}, fmt.Errorf("%w: unsupported number type", ErrInvalidRequest)
	}
	areaCode := strings.TrimSpace(request.AreaCode)
	for _, digit := range areaCode {
		if digit < '0' || digit > '9' {
			return NumberSearchPage{}, fmt.Errorf("%w: area code must contain digits only", ErrInvalidRequest)
		}
	}
	if len(areaCode) > 10 {
		return NumberSearchPage{}, fmt.Errorf("%w: area code is too long", ErrInvalidRequest)
	}
	pageSize := request.PageSize
	if pageSize == 0 {
		pageSize = 50
	}
	if pageSize < 1 || pageSize > 1000 {
		return NumberSearchPage{}, fmt.Errorf("%w: page size must be between 1 and 1000", ErrInvalidRequest)
	}
	if len(request.Cursor) > 2048 {
		return NumberSearchPage{}, fmt.Errorf("%w: cursor is too long", ErrInvalidRequest)
	}

	query := url.Values{}
	query.Set("PageSize", fmt.Sprint(pageSize))
	if request.Cursor != "" {
		switch {
		case strings.HasPrefix(request.Cursor, twilioPageToken):
			token := strings.TrimPrefix(request.Cursor, twilioPageToken)
			if token == "" {
				return NumberSearchPage{}, fmt.Errorf("%w: empty page token", ErrInvalidRequest)
			}
			query.Set("PageToken", token)
		case strings.HasPrefix(request.Cursor, twilioPageNumber):
			page := strings.TrimPrefix(request.Cursor, twilioPageNumber)
			pageNumber, err := strconv.Atoi(page)
			if err != nil || pageNumber < 0 {
				return NumberSearchPage{}, fmt.Errorf("%w: invalid page cursor", ErrInvalidRequest)
			}
			query.Set("Page", page)
		default:
			return NumberSearchPage{}, fmt.Errorf("%w: invalid page cursor", ErrInvalidRequest)
		}
	}
	if areaCode != "" {
		query.Set("AreaCode", areaCode)
	}
	if request.Require.SMS {
		query.Set("SmsEnabled", "true")
	}
	if request.Require.Voice {
		query.Set("VoiceEnabled", "true")
	}
	path := fmt.Sprintf("/AvailablePhoneNumbers/%s/%s.json", url.PathEscape(countryCode), url.PathEscape(string(numberType)))
	var response struct {
		NextPageURI string `json:"next_page_uri"`
		Numbers     []struct {
			PhoneNumber  string `json:"phone_number"`
			FriendlyName string `json:"friendly_name"`
			Capabilities struct {
				SMS   bool `json:"sms"`
				MMS   bool `json:"mms"`
				Voice bool `json:"voice"`
			} `json:"capabilities"`
		} `json:"available_phone_numbers"`
	}
	if err := t.request(ctx, http.MethodGet, path, query, &response); err != nil {
		return NumberSearchPage{}, err
	}
	numbers := make([]AvailableNumber, 0, len(response.Numbers))
	for _, number := range response.Numbers {
		capabilities := Capabilities{SMS: number.Capabilities.SMS, MMS: number.Capabilities.MMS, Voice: number.Capabilities.Voice}
		if request.Require.SMS && !capabilities.SMS || request.Require.MMS && !capabilities.MMS || request.Require.Voice && !capabilities.Voice {
			continue
		}
		numbers = append(numbers, AvailableNumber{
			PhoneNumber:  number.PhoneNumber,
			Name:         number.FriendlyName,
			Capabilities: capabilities,
		})
	}
	page := NumberSearchPage{Numbers: numbers}
	if response.NextPageURI != "" {
		nextPage, err := url.Parse(response.NextPageURI)
		if err != nil {
			return NumberSearchPage{}, fmt.Errorf("%w: malformed next-page cursor", ErrProviderRejected)
		}
		nextQuery := nextPage.Query()
		if token := nextQuery.Get("PageToken"); token != "" {
			page.NextCursor = twilioPageToken + token
		} else if pageNumber := nextQuery.Get("Page"); pageNumber != "" {
			if value, err := strconv.Atoi(pageNumber); err != nil || value < 0 {
				return NumberSearchPage{}, fmt.Errorf("%w: malformed next-page cursor", ErrProviderRejected)
			}
			page.NextCursor = twilioPageNumber + pageNumber
		} else {
			return NumberSearchPage{}, fmt.Errorf("%w: malformed next-page cursor", ErrProviderRejected)
		}
	}
	return page, nil
}

func (t *Twilio) ProvisionNumber(ctx context.Context, request ProvisionNumberRequest) (Number, error) {
	if strings.TrimSpace(request.PhoneNumber) == "" {
		return Number{}, fmt.Errorf("%w: phone number is required", ErrInvalidRequest)
	}
	form := url.Values{"PhoneNumber": {request.PhoneNumber}}
	if request.SMSWebhookURL != "" {
		form.Set("SmsUrl", request.SMSWebhookURL)
	}
	if request.StatusCallbackURL != "" {
		form.Set("StatusCallback", request.StatusCallbackURL)
	}
	var response struct {
		SID         string `json:"sid"`
		PhoneNumber string `json:"phone_number"`
	}
	if err := t.request(ctx, http.MethodPost, "/IncomingPhoneNumbers.json", form, &response); err != nil {
		return Number{}, err
	}
	if response.SID == "" || response.PhoneNumber == "" {
		return Number{}, fmt.Errorf("%w: incomplete Twilio number response", ErrProviderRejected)
	}
	return Number{ProviderReference: response.SID, PhoneNumber: response.PhoneNumber}, nil
}

func (t *Twilio) ReleaseNumber(ctx context.Context, providerReference string) error {
	providerReference = strings.TrimSpace(providerReference)
	if providerReference == "" {
		return fmt.Errorf("%w: provider reference is required", ErrInvalidRequest)
	}
	path := "/IncomingPhoneNumbers/" + url.PathEscape(providerReference) + ".json"
	return t.request(ctx, http.MethodDelete, path, nil, nil)
}

func (t *Twilio) ConfigureNumberWebhooks(ctx context.Context, providerReference string, config NumberWebhookConfig) error {
	providerReference = strings.TrimSpace(providerReference)
	if providerReference == "" {
		return fmt.Errorf("%w: provider reference is required", ErrInvalidRequest)
	}
	path := "/IncomingPhoneNumbers/" + url.PathEscape(providerReference) + ".json"
	form := url.Values{
		"SmsUrl":         {config.SMSURL},
		"StatusCallback": {config.StatusCallbackURL},
	}
	return t.request(ctx, http.MethodPost, path, form, nil)
}

func (t *Twilio) SendMessage(ctx context.Context, request SendMessageRequest) (MessageReceipt, error) {
	if strings.TrimSpace(request.From) == "" || strings.TrimSpace(request.To) == "" || strings.TrimSpace(request.Body) == "" {
		return MessageReceipt{}, fmt.Errorf("%w: from, to, and body are required", ErrInvalidRequest)
	}
	form := url.Values{"From": {request.From}, "To": {request.To}, "Body": {request.Body}}
	if request.StatusCallbackURL != "" {
		form.Set("StatusCallback", request.StatusCallbackURL)
	}
	var response struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	if err := t.request(ctx, http.MethodPost, "/Messages.json", form, &response); err != nil {
		return MessageReceipt{}, err
	}
	if response.SID == "" {
		return MessageReceipt{}, fmt.Errorf("%w: incomplete Twilio message response", ErrProviderRejected)
	}
	return MessageReceipt{ProviderReference: response.SID, Status: response.Status}, nil
}

func (t *Twilio) request(ctx context.Context, method, path string, form url.Values, result any) error {
	requestURL := t.baseURL + "/" + twilioAPIVersion + "/Accounts/" + url.PathEscape(t.accountSID) + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			requestURL += "?" + form.Encode()
		}
	} else if len(form) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return fmt.Errorf("%w: could not create provider request", ErrInvalidRequest)
	}
	req.SetBasicAuth(t.accountSID, t.authToken)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: Twilio request failed: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: could not read provider response", ErrProviderUnavailable)
	}
	if len(responseBody) > maxResponseBytes {
		return fmt.Errorf("%w: oversized provider response", ErrProviderRejected)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: HTTP %d", ErrProviderNotFound, resp.StatusCode)
		case resp.StatusCode == http.StatusTooManyRequests:
			return fmt.Errorf("%w: HTTP %d", ErrProviderRateLimited, resp.StatusCode)
		case resp.StatusCode >= http.StatusInternalServerError:
			return fmt.Errorf("%w: HTTP %d", ErrProviderUnavailable, resp.StatusCode)
		default:
			return fmt.Errorf("%w: HTTP %d", ErrProviderRejected, resp.StatusCode)
		}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(responseBody, result); err != nil {
		return fmt.Errorf("%w: malformed provider response", ErrProviderRejected)
	}
	return nil
}
