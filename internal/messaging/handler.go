package messaging

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"migo/internal/auth"
	"migo/internal/httpx"
	"migo/internal/telephony"
)

const maxRequestBody = 256 << 10

type Handler struct {
	svc      *Service
	resolver Resolver
}

func NewHandler(svc *Service, resolver Resolver) *Handler {
	return &Handler{svc: svc, resolver: resolver}
}

func (h *Handler) Inbound(w http.ResponseWriter, r *http.Request) {
	configID := r.PathValue("providerConfigID")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_webhook", "Webhook payload is invalid.", nil)
		return
	}
	provider, err := h.resolver.Resolve(r.Context(), configID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	parser, ok := provider.(telephony.InboundMessageParser)
	if !ok {
		httpx.WriteError(w, http.StatusNotImplemented, "webhook_unsupported", "This provider does not support inbound messages.", nil)
		return
	}
	signature := r.Header.Get("X-Twilio-Signature")
	if ts := r.Header.Get("Telnyx-Timestamp"); ts != "" {
		signature = ts + "." + r.Header.Get("Telnyx-Signature-Ed25519")
	}
	publicURL := externalURL(r)
	created, err := h.svc.ProcessInbound(r.Context(), configID, parser, telephony.InboundWebhook{PublicURL: publicURL, Signature: signature, Body: body})
	if err != nil {
		h.writeError(w, err)
		return
	}
	if !created {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func externalURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v == "https" || v == "http" {
		scheme = v
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}
	var in SendRequest
	if err := httpx.DecodeJSON(w, r, &in, 8<<10); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m, err := h.svc.Send(ctx, userID, in)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"message": m})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			h.writeError(w, ErrInvalidRequest)
			return
		}
		limit = n
	}
	page, err := h.svc.ListMessages(r.Context(), userID, r.URL.Query().Get("number_id"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"messages": page.Messages, "next_cursor": page.NextCursor})
}

func (h *Handler) MyNumbers(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}
	numbers, err := h.svc.ListNumbers(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"numbers": numbers})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Request is missing or invalid.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "The requested resource was not found.", nil)
	case errors.Is(err, ErrNumberInactive):
		httpx.WriteError(w, http.StatusConflict, "number_inactive", "The number is not active or SMS capable.", nil)
	case errors.Is(err, telephony.ErrInvalidWebhookSignature):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_webhook_signature", "Webhook signature is invalid.", nil)
	case errors.Is(err, telephony.ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_webhook", "Webhook payload is invalid.", nil)
	case errors.Is(err, telephony.ErrProviderRateLimited):
		httpx.WriteError(w, http.StatusTooManyRequests, "provider_rate_limited", "Messaging provider rate limit reached.", nil)
	case errors.Is(err, telephony.ErrProviderUnavailable):
		httpx.WriteError(w, http.StatusBadGateway, "provider_unavailable", "Messaging provider is temporarily unavailable.", nil)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}
