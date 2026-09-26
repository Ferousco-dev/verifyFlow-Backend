package providerconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"migo/internal/httpx"
)

const (
	maxBodyBytes   = 8 << 10
	requestTimeout = 10 * time.Second
)

// Handler is mounted admin-only (RequireAuth + RequireRole(admin)). It never
// returns credential plaintext or ciphertext in a response.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type configResponse struct {
	ID                   string    `json:"id"`
	Kind                 string    `json:"kind"`
	Key                  string    `json:"key"`
	Name                 string    `json:"name"`
	IsEnabled            bool      `json:"is_enabled"`
	CredentialKeyVersion string    `json:"credential_key_version"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func toConfigResponse(c Config) configResponse {
	return configResponse{
		ID: c.ID, Kind: c.Kind, Key: c.Key, Name: c.Name, IsEnabled: c.IsEnabled,
		CredentialKeyVersion: c.CredentialKeyVersion, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind        string          `json:"kind"`
		Key         string          `json:"key"`
		Name        string          `json:"name"`
		Credentials json.RawMessage `json:"credentials"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	created, err := h.svc.Create(ctx, req.Kind, req.Key, req.Name, req.Credentials)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"provider_config": toConfigResponse(created)})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	configs, err := h.svc.List(ctx, r.URL.Query().Get("kind"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	resp := make([]configResponse, 0, len(configs))
	for _, c := range configs {
		resp = append(resp, toConfigResponse(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"provider_configs": resp})
}

func (h *Handler) SetEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		IsEnabled bool `json:"is_enabled"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	updated, err := h.svc.SetEnabled(ctx, id, req.IsEnabled)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"provider_config": toConfigResponse(updated)})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Request is missing or has invalid fields.", nil)
	case errors.Is(err, ErrDuplicateConfig):
		httpx.WriteError(w, http.StatusConflict, "duplicate_config", "A provider config with this kind, key and name already exists.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "The requested provider config was not found.", nil)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}
