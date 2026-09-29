package wallet

import (
	"errors"
	"net/http"
	"strconv"

	"migo/internal/auth"
	"migo/internal/httpx"
)

type Handler struct {
	svc     *Service
	funding *FundingService
}

func NewHandler(svc *Service) *Handler                   { return &Handler{svc: svc} }
func (h *Handler) EnableFunding(funding *FundingService) { h.funding = funding }

func (h *Handler) Balance(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = "NGN"
	}
	balance, err := h.svc.Balance(r.Context(), userID, currency)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"wallet": balance})
}

func (h *Handler) Transactions(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = "NGN"
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
	items, err := h.svc.Transactions(r.Context(), userID, currency, limit)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"transactions": items})
}

func (h *Handler) Adjust(w http.ResponseWriter, r *http.Request) {
	actorID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	var in Adjustment
	if err := httpx.DecodeJSON(w, r, &in, 8<<10); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	in.ActorUserID = actorID
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	transaction, err := h.svc.Adjust(r.Context(), in)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"transaction": transaction})
}

func (h *Handler) InitializeFunding(w http.ResponseWriter, r *http.Request) {
	if h.funding == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "payment_provider_unavailable", "Payment processing is not currently configured.", nil)
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	var in struct {
		AmountMinorUnits int64  `json:"amount_minor_units"`
		Currency         string `json:"currency"`
	}
	if err := httpx.DecodeJSON(w, r, &in, 8<<10); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	result, err := h.funding.Initialize(r.Context(), userID, in.Currency, r.Header.Get("Idempotency-Key"), in.AmountMinorUnits)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"funding": result.Attempt, "authorization_url": result.AuthorizationURL})
}

func (h *Handler) VerifyFunding(w http.ResponseWriter, r *http.Request) {
	if h.funding == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "payment_provider_unavailable", "Payment processing is not currently configured.", nil)
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		unauthorized(w)
		return
	}
	attempt, err := h.funding.Verify(r.Context(), userID, r.PathValue("id"))
	if err != nil && !errors.Is(err, ErrAmountMismatch) {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"funding": attempt})
}

func unauthorized(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
}
func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Request is missing or invalid.", nil)
	case errors.Is(err, ErrInsufficientFunds):
		httpx.WriteError(w, http.StatusConflict, "insufficient_funds", "The wallet has insufficient funds.", nil)
	case errors.Is(err, ErrIdempotencyConflict):
		httpx.WriteError(w, http.StatusConflict, "idempotency_conflict", "This operation has already been submitted.", nil)
	case errors.Is(err, ErrFundingNotFound):
		httpx.WriteError(w, http.StatusNotFound, "funding_not_found", "The funding attempt was not found.", nil)
	case errors.Is(err, ErrAmountMismatch):
		httpx.WriteError(w, http.StatusConflict, "amount_mismatch", "The verified payment amount or currency does not match this funding attempt.", nil)
	case errors.Is(err, ErrProviderUnavailable):
		httpx.WriteError(w, http.StatusServiceUnavailable, "payment_provider_unavailable", "Payment processing is not currently configured.", nil)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}
