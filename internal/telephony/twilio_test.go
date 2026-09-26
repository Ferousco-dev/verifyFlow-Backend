package telephony

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestTwilio(t *testing.T, handler http.Handler) *Twilio {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewTwilio("AC-test", "auth-test")
	if err != nil {
		t.Fatal(err)
	}
	provider.baseURL = server.URL
	provider.client = server.Client()
	return provider
}

func TestNewTwilioRequiresCredentials(t *testing.T) {
	for _, credentials := range [][2]string{{"", "token"}, {"account", ""}, {" ", "token"}} {
		if _, err := NewTwilio(credentials[0], credentials[1]); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("NewTwilio(%q, %q) error = %v", credentials[0], credentials[1], err)
		}
	}
}

func TestTwilioSearchNumbers(t *testing.T) {
	provider := newTestTwilio(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/2010-04-01/Accounts/AC-test/AvailablePhoneNumbers/US/Local.json" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
		}
		if username, password, ok := r.BasicAuth(); !ok || username != "AC-test" || password != "auth-test" {
			t.Errorf("basic auth = %q, %q, %t", username, password, ok)
		}
		want := url.Values{"AreaCode": {"415"}, "PageSize": {"50"}, "SmsEnabled": {"true"}, "VoiceEnabled": {"true"}}
		if r.URL.Query().Encode() != want.Encode() {
			t.Errorf("query = %v, want %v", r.URL.Query(), want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"available_phone_numbers":[{"phone_number":"+14155550100","friendly_name":"Non-MMS","capabilities":{"sms":true,"mms":false,"voice":true}},{"phone_number":"+14155550101","friendly_name":"MMS-capable","capabilities":{"sms":true,"mms":true,"voice":true}}]}`)
	}))

	page, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{
		CountryCode: "us", AreaCode: "415", Require: Capabilities{SMS: true, MMS: true, Voice: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Numbers) != 1 || page.Numbers[0].PhoneNumber != "+14155550101" || page.Numbers[0].Name != "MMS-capable" ||
		!page.Numbers[0].SMS || !page.Numbers[0].MMS || !page.Numbers[0].Voice {
		t.Fatalf("numbers = %+v", page.Numbers)
	}
}

func TestTwilioSearchNumbersCarriesOpaquePageCursor(t *testing.T) {
	provider := newTestTwilio(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("PageSize") != "1" || r.URL.Query().Get("PageToken") != "cursor+one" {
			t.Errorf("pagination query = %v", r.URL.Query())
		}
		_, _ = io.WriteString(w, `{"available_phone_numbers":[],"next_page_uri":"/2010-04-01/Accounts/AC-test/AvailablePhoneNumbers/US/Local.json?Page=2&PageToken=cursor%2Btwo"}`)
	}))
	page, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{
		CountryCode: "US", PageSize: 1, Cursor: "token:cursor+one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "token:cursor+two" {
		t.Fatalf("next cursor = %q", page.NextCursor)
	}
}

func TestTwilioSearchCarriesLegacyPageCursor(t *testing.T) {
	requests := 0
	provider := newTestTwilio(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.URL.Query().Get("Page") != "" {
				t.Errorf("first page unexpectedly has Page query: %v", r.URL.Query())
			}
			_, _ = io.WriteString(w, `{"available_phone_numbers":[],"next_page_uri":"/AvailablePhoneNumbers/US/Local.json?Page=2&PageSize=50"}`)
			return
		}
		if r.URL.Query().Get("Page") != "2" {
			t.Errorf("next page query = %v", r.URL.Query())
		}
		_, _ = io.WriteString(w, `{"available_phone_numbers":[]}`)
	}))
	firstPage, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{CountryCode: "US"})
	if err != nil {
		t.Fatal(err)
	}
	if firstPage.NextCursor != "page:2" {
		t.Fatalf("next cursor = %q", firstPage.NextCursor)
	}
	if _, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{CountryCode: "US", Cursor: firstPage.NextCursor}); err != nil {
		t.Fatal(err)
	}
}

func TestTwilioSearchRejectsInvalidPageSize(t *testing.T) {
	called := false
	provider := newTestTwilio(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	_, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{CountryCode: "US", PageSize: 1001})
	if !errors.Is(err, ErrInvalidRequest) || called {
		t.Fatalf("error = %v, request sent = %t", err, called)
	}
}

func TestTwilioProvisionReleaseAndSendMessage(t *testing.T) {
	provider := newTestTwilio(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if username, password, ok := r.BasicAuth(); !ok || username != "AC-test" || password != "auth-test" {
			t.Errorf("basic auth = %q, %q, %t", username, password, ok)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/2010-04-01/Accounts/AC-test/IncomingPhoneNumbers.json":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse number form: %v", err)
			}
			if r.Form.Get("PhoneNumber") != "+14155550100" || r.Form.Get("SmsUrl") != "https://api.example.com/inbound" ||
				r.Form.Get("StatusCallback") != "https://api.example.com/status" {
				t.Errorf("number form = %v", r.Form)
			}
			_, _ = io.WriteString(w, `{"sid":"PN-provider-ref","phone_number":"+14155550100"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/2010-04-01/Accounts/AC-test/IncomingPhoneNumbers/PN-provider-ref.json":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/2010-04-01/Accounts/AC-test/Messages.json":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse message form: %v", err)
			}
			if r.Form.Get("From") != "+14155550100" || r.Form.Get("To") != "+14155550101" || r.Form.Get("Body") != "hello" ||
				r.Form.Get("StatusCallback") != "https://api.example.com/status" {
				t.Errorf("message form = %v", r.Form)
			}
			_, _ = io.WriteString(w, `{"sid":"SM-provider-ref","status":"queued"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))

	number, err := provider.ProvisionNumber(context.Background(), ProvisionNumberRequest{
		PhoneNumber: "+14155550100", SMSWebhookURL: "https://api.example.com/inbound", StatusCallbackURL: "https://api.example.com/status",
	})
	if err != nil || number.ProviderReference != "PN-provider-ref" || number.PhoneNumber != "+14155550100" {
		t.Fatalf("provision = %+v, %v", number, err)
	}
	if err := provider.ReleaseNumber(context.Background(), number.ProviderReference); err != nil {
		t.Fatalf("release: %v", err)
	}
	receipt, err := provider.SendMessage(context.Background(), SendMessageRequest{
		From: "+14155550100", To: "+14155550101", Body: "hello", StatusCallbackURL: "https://api.example.com/status",
	})
	if err != nil || receipt.ProviderReference != "SM-provider-ref" || receipt.Status != "queued" {
		t.Fatalf("send = %+v, %v", receipt, err)
	}
}

func TestTwilioMapsErrorsWithoutLeakingResponseBody(t *testing.T) {
	const secret = "response-secret"
	provider := newTestTwilio(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"`+secret+`"}`)
	}))
	_, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{CountryCode: "US"})
	if !errors.Is(err, ErrProviderUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error = %v", err)
	}
}

func TestTwilioRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	called := false
	provider := newTestTwilio(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	if _, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{CountryCode: "USA"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid country code error = %v", err)
	}
	if _, err := provider.ProvisionNumber(context.Background(), ProvisionNumberRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty number error = %v", err)
	}
	if _, err := provider.SendMessage(context.Background(), SendMessageRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty message error = %v", err)
	}
	if called {
		t.Fatal("invalid request reached Twilio")
	}
}

func TestTwilioRejectsMalformedSuccessResponse(t *testing.T) {
	provider := newTestTwilio(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"available_phone_numbers":`)
	}))
	_, err := provider.SearchNumbers(context.Background(), SearchNumbersRequest{CountryCode: "US"})
	if !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("malformed response error = %v", err)
	}
}
