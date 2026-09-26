package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAdminProviderConfigCreateListAndToggle(t *testing.T) {
	h, pool, _, _ := newFullStack(t, "", "")
	adminID, adminToken := registerVerifiedUser(t, h, pool, "provider-admin@example.com")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1::uuid`, adminID); err != nil {
		t.Fatal(err)
	}
	adminHeaders := map[string]string{"Authorization": "Bearer " + adminToken}

	w := call(h, req{method: "POST", path: "/api/v1/admin/provider-configs",
		body:    `{"kind":"telephony","key":"twilio","name":"primary","credentials":{"account_sid":"AC123","auth_token":"secret-value"}}`,
		headers: adminHeaders})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret-value") {
		t.Fatalf("response leaked plaintext credentials: %s", w.Body)
	}
	var created struct {
		ProviderConfig struct {
			ID        string `json:"id"`
			IsEnabled bool   `json:"is_enabled"`
		} `json:"provider_config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.ProviderConfig.IsEnabled {
		t.Fatalf("expected new config enabled by default: %+v", created)
	}

	// Duplicate kind/key/name is rejected.
	w = call(h, req{method: "POST", path: "/api/v1/admin/provider-configs",
		body:    `{"kind":"telephony","key":"twilio","name":"primary","credentials":{"account_sid":"AC999","auth_token":"other"}}`,
		headers: adminHeaders})
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate create: %d %s", w.Code, w.Body)
	}

	w = call(h, req{method: "GET", path: "/api/v1/admin/provider-configs?kind=telephony", headers: adminHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.ProviderConfig.ID) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "secret-value") || strings.Contains(w.Body.String(), "credentials_ciphertext") {
		t.Fatalf("list leaked credential material: %s", w.Body)
	}

	w = call(h, req{method: "PATCH", path: "/api/v1/admin/provider-configs/" + created.ProviderConfig.ID,
		body: `{"is_enabled":false}`, headers: adminHeaders})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"is_enabled":false`) {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}

	// Non-admin cannot reach any provider-config route.
	_, regularToken := registerVerifiedUser(t, h, pool, "provider-regular@example.com")
	regularHeaders := map[string]string{"Authorization": "Bearer " + regularToken}
	if w := call(h, req{method: "GET", path: "/api/v1/admin/provider-configs", headers: regularHeaders}); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin list: %d %s", w.Code, w.Body)
	}
}

func TestAdminProviderConfigCreateRejectsInvalidFields(t *testing.T) {
	h, pool, _, _ := newFullStack(t, "", "")
	adminID, adminToken := registerVerifiedUser(t, h, pool, "provider-admin-2@example.com")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1::uuid`, adminID); err != nil {
		t.Fatal(err)
	}
	adminHeaders := map[string]string{"Authorization": "Bearer " + adminToken}

	w := call(h, req{method: "POST", path: "/api/v1/admin/provider-configs",
		body:    `{"kind":"not-a-kind","key":"twilio","name":"primary","credentials":{"x":"y"}}`,
		headers: adminHeaders})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid kind: %d %s", w.Code, w.Body)
	}
}
