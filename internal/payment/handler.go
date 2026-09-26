package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"migo/internal/auth"
	"migo/internal/httpx"
)

const (
	maxWebhookBodyBytes = 256 << 10
	requestTimeout      = 15 * time.Second
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type initializeResponse struct {
	AuthorizationURL string `json:"authorization_url"`
	Reference        string `json:"reference"`
}

// Initialize starts a Paystack checkout for the order named by the path
// value "id". Mounted behind RequireAuth + RequireVerifiedEmail, same as
// the rest of the rental routes.
func (h *Handler) Initialize(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}
	orderID := r.PathValue("id")

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	result, err := h.svc.Initialize(ctx, userID, orderID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, initializeResponse{AuthorizationURL: result.AuthorizationURL, Reference: result.Reference})
}

type attemptResponse struct {
	Status           string    `json:"status"`
	AmountMinorUnits int64     `json:"amount_minor_units"`
	Currency         string    `json:"currency"`
	Reference        string    `json:"reference"`
	CreatedAt        time.Time `json:"created_at"`
}

func toAttemptResponse(a Attempt) attemptResponse {
	return attemptResponse{
		Status: a.ProviderStatus, AmountMinorUnits: a.AmountMinorUnits, Currency: a.Currency,
		Reference: a.ProviderReference, CreatedAt: a.CreatedAt,
	}
}

// Verify re-checks the order's latest payment attempt directly against
// Paystack (useful for polling after the checkout redirect, and callable
// even if the webhook is delayed or misconfigured).
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}
	orderID := r.PathValue("id")

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	attempt, err := h.svc.Verify(ctx, userID, orderID)
	if err != nil && !errors.Is(err, ErrAmountMismatch) {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"payment": toAttemptResponse(attempt)})
}

// Webhook receives Paystack event deliveries. It is unauthenticated (no
// Bearer token) — trust comes entirely from the signature check inside
// Service.HandleWebhookEvent.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_body", "Could not read request body.", nil)
		return
	}
	if len(body) > maxWebhookBodyBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body is too large.", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if err := h.svc.HandleWebhookEvent(ctx, body, r.Header.Get("X-Paystack-Signature")); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Request is missing or has invalid fields.", nil)
	case errors.Is(err, ErrInvalidWebhookSignature):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_signature", "Webhook signature is invalid.", nil)
	case errors.Is(err, ErrOrderNotFound):
		httpx.WriteError(w, http.StatusNotFound, "order_not_found", "The requested order was not found.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "No payment attempt was found for this order.", nil)
	case errors.Is(err, ErrOrderNotPending):
		httpx.WriteError(w, http.StatusConflict, "order_not_pending", "This order is no longer pending.", nil)
	case errors.Is(err, ErrAlreadyPaid):
		httpx.WriteError(w, http.StatusConflict, "already_paid", "This order already has a successful payment.", nil)
	case errors.Is(err, ErrAmountMismatch):
		httpx.WriteError(w, http.StatusConflict, "amount_mismatch", "The verified payment does not match this order's amount or currency.", nil)
	case errors.Is(err, ErrProviderConfigMissing):
		httpx.WriteError(w, http.StatusServiceUnavailable, "payment_provider_unavailable", "Payment processing is not currently configured.", nil)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}
