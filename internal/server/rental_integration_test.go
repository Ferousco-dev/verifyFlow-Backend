package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/auth"
	"migo/internal/fulfillment"
	"migo/internal/mailer"
	"migo/internal/payment"
	"migo/internal/providerconfig"
	"migo/internal/providercrypto"
	"migo/internal/rental"
	"migo/internal/testutil/dbtest"
	"migo/internal/user"
)

func newRentalStack(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	handler, pool, _, _ := newFullStack(t, "", "")
	return handler, pool
}

// newFullStack builds the full router. paystackBaseURL and twilioBaseURL,
// when non-empty, point the payment and fulfillment resolvers at test
// doubles instead of the real production APIs.
func newFullStack(t *testing.T, paystackBaseURL, twilioBaseURL string) (http.Handler, *pgxpool.Pool, *providerconfig.Handler, *providerconfig.Service) {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	tokens := auth.NewTokenManager([]byte(strings.Repeat("s", 32)), "migo", 15*time.Minute)
	svc, err := auth.NewService(user.NewRepository(pool), auth.NewSessionRepository(pool),
		auth.NewHasher(auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 4), tokens, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc.EnableEmailVerification(auth.NewVerifyRepository(pool), &sink{}, "https://app.example.com/verify-email", "https://app.example.com/dashboard", 24*time.Hour, log)
	h := auth.NewHandler(svc, log)

	rentalSvc, err := rental.NewService(rental.NewRepository(pool), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rentalHandler := rental.NewHandler(rentalSvc)

	keyRing, err := providercrypto.NewKeyRing(map[string][]byte{"v1": []byte(strings.Repeat("k", 32))}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	providerConfigSvc, err := providerconfig.NewService(providerconfig.NewRepository(pool), keyRing)
	if err != nil {
		t.Fatal(err)
	}
	providerConfigHandler := providerconfig.NewHandler(providerConfigSvc)

	resolver := payment.NewProviderConfigResolver(providerConfigSvc)
	if paystackBaseURL != "" {
		resolver.SetBaseURL(paystackBaseURL, http.DefaultClient)
	}
	telephonyResolver := fulfillment.NewProviderConfigResolver(providerConfigSvc)
	if twilioBaseURL != "" {
		telephonyResolver.SetBaseURL(twilioBaseURL, http.DefaultClient)
	}
	fulfillmentSvc, err := fulfillment.NewService(rentalSvc, telephonyResolver)
	if err != nil {
		t.Fatal(err)
	}
	paymentSvc, err := payment.NewService(
		payment.NewRepository(pool), payment.NewRentalOrders(rentalSvc), payment.NewAuthUserEmails(svc), resolver, fulfillmentSvc, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	paymentHandler := payment.NewHandler(paymentSvc)

	handler := New(Deps{
		Log: log, DB: pool, Auth: h, Tokens: tokens, AuthService: svc,
		Rental: rentalHandler, Payment: paymentHandler, ProviderConfig: providerConfigHandler,
		Limiters: DefaultLimiters(), AllowedOrigins: []string{allowedOrigin},
	})
	return handler, pool, providerConfigHandler, providerConfigSvc
}

var _ mailer.Sender = &sink{}

func registerVerifiedUser(t *testing.T, h http.Handler, pool *pgxpool.Pool, email string) (userID, accessToken string) {
	t.Helper()
	w := call(h, req{method: "POST", path: "/api/v1/auth/register",
		body: fmt.Sprintf(`{"full_name":"Rental User","email":%q,"password":"a-long-password"}`, email)})
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var resp struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET email_verified = true WHERE id = $1::uuid`, resp.User.ID,
	); err != nil {
		t.Fatal(err)
	}
	return resp.User.ID, resp.AccessToken
}

func seedRentalPlanAndNumber(t *testing.T, pool *pgxpool.Pool) (planID, numberID string) {
	t.Helper()
	var configID string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO provider_configs (provider_kind, provider_key, config_name, credentials_ciphertext, credential_key_version)
		 VALUES ('telephony', 'twilio', 'primary', decode('010203', 'hex'), 'test-v1') RETURNING id::text`,
	).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	return seedRentalPlanAndNumberWithConfig(t, pool, configID)
}

// seedRentalPlanAndNumberWithConfig seeds a plan and an available number
// backed by an existing provider_configs row (e.g. one created through
// providerconfig.Service, so its credentials are actually decryptable).
func seedRentalPlanAndNumberWithConfig(t *testing.T, pool *pgxpool.Pool, configID string) (planID, numberID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency)
		 VALUES ('one-hour', 'One hour', 3600, 1299, 'USD') RETURNING id::text`,
	).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, sms_enabled)
		 VALUES ($1::uuid, 'provider-ref-1', '+14155550001', 'Local', true) RETURNING id::text`,
		configID,
	).Scan(&numberID); err != nil {
		t.Fatal(err)
	}
	return planID, numberID
}

func TestRentalRoutesRequireAuthAndVerifiedEmail(t *testing.T) {
	h, _ := newRentalStack(t)

	if w := call(h, req{method: "GET", path: "/api/v1/numbers"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search: %d %s", w.Code, w.Body)
	}

	w := call(h, req{method: "POST", path: "/api/v1/auth/register",
		body: `{"full_name":"Unverified User","email":"unverified@example.com","password":"a-long-password"}`})
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	w = call(h, req{method: "GET", path: "/api/v1/numbers",
		headers: map[string]string{"Authorization": "Bearer " + resp.AccessToken}})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "email_not_verified") {
		t.Fatalf("unverified user search: %d %s", w.Code, w.Body)
	}
}

func TestRentalSearchListPlansAndCheckoutFlow(t *testing.T) {
	h, pool := newRentalStack(t)
	_, accessToken := registerVerifiedUser(t, h, pool, "checkout@example.com")
	planID, numberID := seedRentalPlanAndNumber(t, pool)
	authHeaders := map[string]string{"Authorization": "Bearer " + accessToken}

	w := call(h, req{method: "GET", path: "/api/v1/numbers?type=Local&sms=true", headers: authHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), numberID) {
		t.Fatalf("search numbers: %d %s", w.Code, w.Body)
	}

	w = call(h, req{method: "GET", path: "/api/v1/rental-plans", headers: authHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), planID) {
		t.Fatalf("list plans: %d %s", w.Code, w.Body)
	}

	reserveHeaders := map[string]string{"Authorization": "Bearer " + accessToken, "Idempotency-Key": "checkout-key-1"}
	w = call(h, req{method: "POST", path: "/api/v1/rentals",
		body:    fmt.Sprintf(`{"provider_number_id":%q,"rental_plan_id":%q}`, numberID, planID),
		headers: reserveHeaders})
	if w.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", w.Code, w.Body)
	}
	var reserveResp struct {
		Order struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reserveResp); err != nil {
		t.Fatal(err)
	}
	if reserveResp.Order.Status != "PENDING" {
		t.Fatalf("order status = %s", reserveResp.Order.Status)
	}

	// Same idempotency key returns the same order rather than reserving twice.
	w = call(h, req{method: "POST", path: "/api/v1/rentals",
		body:    fmt.Sprintf(`{"provider_number_id":%q,"rental_plan_id":%q}`, numberID, planID),
		headers: reserveHeaders})
	if w.Code != http.StatusCreated {
		t.Fatalf("idempotent reserve: %d %s", w.Code, w.Body)
	}
	var replay struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Order.ID != reserveResp.Order.ID {
		t.Fatalf("idempotent replay created a different order: %s vs %s", replay.Order.ID, reserveResp.Order.ID)
	}

	w = call(h, req{method: "GET", path: "/api/v1/orders/" + reserveResp.Order.ID, headers: authHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), reserveResp.Order.ID) {
		t.Fatalf("get order: %d %s", w.Code, w.Body)
	}

	// A different user cannot see someone else's order.
	_, otherToken := registerVerifiedUser(t, h, pool, "other@example.com")
	w = call(h, req{method: "GET", path: "/api/v1/orders/" + reserveResp.Order.ID,
		headers: map[string]string{"Authorization": "Bearer " + otherToken}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user order lookup: %d %s", w.Code, w.Body)
	}
}

func TestAdminCanChangeAnotherUsersRoleButNotDemoteLastAdmin(t *testing.T) {
	h, pool := newRentalStack(t)
	adminID, adminToken := registerVerifiedUser(t, h, pool, "admin@example.com")
	regularID, _ := registerVerifiedUser(t, h, pool, "regular@example.com")

	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1::uuid`, adminID); err != nil {
		t.Fatal(err)
	}
	adminHeaders := map[string]string{"Authorization": "Bearer " + adminToken}

	w := call(h, req{method: "PATCH", path: "/api/v1/admin/users/" + regularID + "/role",
		body: `{"role":"admin"}`, headers: adminHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"role":"admin"`) {
		t.Fatalf("promote regular user: %d %s", w.Code, w.Body)
	}

	// A non-admin cannot call the route at all.
	_, regularToken := registerVerifiedUser(t, h, pool, "third@example.com")
	w = call(h, req{method: "PATCH", path: "/api/v1/admin/users/" + adminID + "/role",
		body: `{"role":"user"}`, headers: map[string]string{"Authorization": "Bearer " + regularToken}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin caller: %d %s", w.Code, w.Body)
	}

	// Demoting the original admin is now safe (regularID is also admin), but
	// demoting the ONLY remaining admin must be refused.
	w = call(h, req{method: "PATCH", path: "/api/v1/admin/users/" + regularID + "/role",
		body: `{"role":"user"}`, headers: adminHeaders})
	if w.Code != http.StatusOK {
		t.Fatalf("demote one of two admins: %d %s", w.Code, w.Body)
	}
	w = call(h, req{method: "PATCH", path: "/api/v1/admin/users/" + adminID + "/role",
		body: `{"role":"user"}`, headers: adminHeaders})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "last_admin") {
		t.Fatalf("demoting the last admin: %d %s", w.Code, w.Body)
	}
}

func TestRentalReserveMissingIdempotencyKey(t *testing.T) {
	h, pool := newRentalStack(t)
	_, accessToken := registerVerifiedUser(t, h, pool, "no-key@example.com")
	planID, numberID := seedRentalPlanAndNumber(t, pool)

	w := call(h, req{method: "POST", path: "/api/v1/rentals",
		body:    fmt.Sprintf(`{"provider_number_id":%q,"rental_plan_id":%q}`, numberID, planID),
		headers: map[string]string{"Authorization": "Bearer " + accessToken}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing_idempotency_key") {
		t.Fatalf("missing idempotency key: %d %s", w.Code, w.Body)
	}
}
