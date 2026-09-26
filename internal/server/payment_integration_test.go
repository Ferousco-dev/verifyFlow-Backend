package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"migo/internal/providerconfig"
)

const paystackTestSecret = "sk_test_secret_value"

// newPaystackTestServer fakes just enough of Paystack's transaction API for
// the init -> verify -> webhook flow: /transaction/initialize always
// succeeds, and /transaction/verify/{reference} returns whatever status the
// test has configured for that reference (defaulting to "success" at the
// order's own price/currency, so a plain happy-path test needs no setup).
type paystackTestServer struct {
	*httptest.Server
	verifyResponses map[string]string // reference -> raw JSON "data" status override
}

func newPaystackTestServer(t *testing.T) *paystackTestServer {
	t.Helper()
	fake := &paystackTestServer{verifyResponses: map[string]string{}}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/transaction/initialize":
			var body struct {
				Amount    int64  `json:"amount"`
				Currency  string `json:"currency"`
				Reference string `json:"reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fmt.Fprintf(w, `{"status":true,"data":{"authorization_url":"https://checkout.paystack.test/%s","access_code":"code","reference":"%s"}}`,
				body.Reference, body.Reference)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/transaction/verify/"):
			reference := strings.TrimPrefix(r.URL.Path, "/transaction/verify/")
			if custom, ok := fake.verifyResponses[reference]; ok {
				_, _ = io.WriteString(w, custom)
				return
			}
			fmt.Fprintf(w, `{"status":true,"data":{"status":"success","amount":1299,"currency":"USD","reference":"%s"}}`, reference)
		default:
			t.Errorf("unexpected Paystack request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.Close)
	return fake
}

func createPaystackProviderConfig(t *testing.T, svc *providerconfig.Service) {
	t.Helper()
	credentials, err := json.Marshal(map[string]string{"secret_key": paystackTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), providerconfig.KindPayment, "paystack", "primary", credentials); err != nil {
		t.Fatal(err)
	}
}

func signWebhook(body []byte) string {
	mac := hmac.New(sha512.New, []byte(paystackTestSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// newTwilioTestServer fakes just enough of Twilio's IncomingPhoneNumbers
// endpoint for provisioning: every request succeeds and echoes back the
// requested phone number with a made-up SID.
func newTwilioTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/IncomingPhoneNumbers.json") {
			t.Errorf("unexpected Twilio request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		fmt.Fprintf(w, `{"sid":"PNtest0000000000000000000000000","phone_number":%q}`, r.FormValue("PhoneNumber"))
	}))
	t.Cleanup(server.Close)
	return server
}

func createTwilioProviderConfig(t *testing.T, svc *providerconfig.Service) (configID string) {
	t.Helper()
	credentials, err := json.Marshal(map[string]string{"account_sid": "AC_test", "auth_token": "test-auth-token"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.Create(context.Background(), providerconfig.KindTelephony, "twilio", "primary", credentials)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ID
}

func TestPaymentInitializeVerifyAndWebhookFlow(t *testing.T) {
	fake := newPaystackTestServer(t)
	twilio := newTwilioTestServer(t)
	h, pool, _, providerConfigSvc := newFullStack(t, fake.URL, twilio.URL)
	createPaystackProviderConfig(t, providerConfigSvc)
	twilioConfigID := createTwilioProviderConfig(t, providerConfigSvc)

	_, accessToken := registerVerifiedUser(t, h, pool, "payer@example.com")
	planID, numberID := seedRentalPlanAndNumberWithConfig(t, pool, twilioConfigID)
	authHeaders := map[string]string{"Authorization": "Bearer " + accessToken}

	w := call(h, req{method: "POST", path: "/api/v1/rentals",
		body:    fmt.Sprintf(`{"provider_number_id":%q,"rental_plan_id":%q}`, numberID, planID),
		headers: map[string]string{"Authorization": "Bearer " + accessToken, "Idempotency-Key": "checkout-1"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", w.Code, w.Body)
	}
	var reserveResp struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reserveResp); err != nil {
		t.Fatal(err)
	}
	orderID := reserveResp.Order.ID

	w = call(h, req{method: "POST", path: "/api/v1/orders/" + orderID + "/payment", headers: authHeaders})
	if w.Code != http.StatusCreated {
		t.Fatalf("initialize: %d %s", w.Code, w.Body)
	}
	var initResp struct {
		AuthorizationURL string `json:"authorization_url"`
		Reference        string `json:"reference"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initResp); err != nil {
		t.Fatal(err)
	}
	if initResp.AuthorizationURL == "" || initResp.Reference == "" {
		t.Fatalf("initialize response = %+v", initResp)
	}

	// A second initialize attempt is allowed (multiple attempts are normal:
	// retries, abandoned checkouts) as long as nothing has succeeded yet.
	w = call(h, req{method: "POST", path: "/api/v1/orders/" + orderID + "/payment", headers: authHeaders})
	if w.Code != http.StatusCreated {
		t.Fatalf("second initialize: %d %s", w.Code, w.Body)
	}

	w = call(h, req{method: "POST", path: "/api/v1/orders/" + orderID + "/payment/verify", headers: authHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}

	// A confirmed payment triggers provisioning: the number goes ACTIVE and
	// the rental is marked activated.
	var numberStatus string
	var activatedAt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT pn.status, r.activated_at::text FROM provider_numbers pn
		 JOIN rentals r ON r.provider_number_id = pn.id
		 WHERE r.order_id = $1::uuid`, orderID,
	).Scan(&numberStatus, &activatedAt); err != nil {
		t.Fatal(err)
	}
	if numberStatus != "ACTIVE" || activatedAt == nil {
		t.Fatalf("fulfillment did not activate the rental: number status=%s activated_at=%v", numberStatus, activatedAt)
	}

	// Now that a payment has succeeded, a fresh initialize is rejected.
	w = call(h, req{method: "POST", path: "/api/v1/orders/" + orderID + "/payment", headers: authHeaders})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already_paid") {
		t.Fatalf("initialize after success: %d %s", w.Code, w.Body)
	}

	// Webhook redelivery of the same event is accepted and is a no-op.
	body := []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":123,"reference":%q}}`, initResp.Reference))
	sig := signWebhook(body)
	for i := 0; i < 2; i++ {
		w = call(h, req{method: "POST", path: "/api/v1/webhooks/paystack", body: string(body),
			headers: map[string]string{"X-Paystack-Signature": sig}})
		if w.Code != http.StatusOK {
			t.Fatalf("webhook delivery %d: %d %s", i, w.Code, w.Body)
		}
	}
}

func TestPaymentWebhookRejectsInvalidSignature(t *testing.T) {
	fake := newPaystackTestServer(t)
	h, _, _, providerConfigSvc := newFullStack(t, fake.URL, "")
	createPaystackProviderConfig(t, providerConfigSvc)

	body := `{"event":"charge.success","data":{"id":1,"reference":"whatever"}}`
	w := call(h, req{method: "POST", path: "/api/v1/webhooks/paystack", body: body,
		headers: map[string]string{"X-Paystack-Signature": "not-a-real-signature"}})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature: %d %s", w.Code, w.Body)
	}
}

func TestPaymentInitializeWithoutProviderConfigIsUnavailable(t *testing.T) {
	fake := newPaystackTestServer(t)
	h, pool, _, _ := newFullStack(t, fake.URL, "")
	// No Paystack provider config created: payment processing is "configured
	// off" from the app's point of view, same as in production without keys.

	_, accessToken := registerVerifiedUser(t, h, pool, "no-config@example.com")
	planID, numberID := seedRentalPlanAndNumber(t, pool)
	w := call(h, req{method: "POST", path: "/api/v1/rentals",
		body:    fmt.Sprintf(`{"provider_number_id":%q,"rental_plan_id":%q}`, numberID, planID),
		headers: map[string]string{"Authorization": "Bearer " + accessToken, "Idempotency-Key": "checkout-2"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", w.Code, w.Body)
	}
	var reserveResp struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reserveResp); err != nil {
		t.Fatal(err)
	}

	w = call(h, req{method: "POST", path: "/api/v1/orders/" + reserveResp.Order.ID + "/payment",
		headers: map[string]string{"Authorization": "Bearer " + accessToken}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("initialize without provider config: %d %s", w.Code, w.Body)
	}
}
