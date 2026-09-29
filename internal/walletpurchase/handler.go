package walletpurchase

import (
	"context"
	"errors"
	"net/http"
	"time"

	"migo/internal/auth"
	"migo/internal/fulfillment"
	"migo/internal/httpx"
	"migo/internal/rental"
	"migo/internal/wallet"
)

const (
	maxBodyBytes   = 4 << 10
	requestTimeout = 15 * time.Second
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type orderResponse struct {
	ID                   string    `json:"id"`
	RentalID             string    `json:"rental_id"`
	ProviderNumberID     string    `json:"provider_number_id"`
	RentalPlanID         string    `json:"rental_plan_id"`
	Status               string    `json:"status"`
	PlanCode             string    `json:"plan_code"`
	PlanName             string    `json:"plan_name"`
	DurationSeconds      int32     `json:"duration_seconds"`
	PriceMinorUnits      int64     `json:"price_minor_units"`
	Currency             string    `json:"currency"`
	ReservationExpiresAt time.Time `json:"reservation_expires_at"`
	CreatedAt            time.Time `json:"created_at"`
}

func toOrderResponse(o rental.Order) orderResponse {
	return orderResponse{
		ID: o.ID, RentalID: o.RentalID, ProviderNumberID: o.ProviderNumberID, RentalPlanID: o.RentalPlanID,
		Status: o.Status, PlanCode: o.PlanCode, PlanName: o.PlanName, DurationSeconds: o.DurationSeconds,
		PriceMinorUnits: o.PriceMinorUnits, Currency: o.Currency,
		ReservationExpiresAt: o.ReservationExpiresAt, CreatedAt: o.CreatedAt,
	}
}

// Purchase handles POST /api/v1/numbers/purchase: reserve a number, debit
// the customer's wallet, and provision it in one call.
func (h *Handler) Purchase(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required.", nil)
		return
	}

	var req struct {
		ProviderNumberID string `json:"provider_number_id"`
		RentalPlanID     string `json:"rental_plan_id"`
	}
	if err := httpx.DecodeJSON(w, r, &req, maxBodyBytes); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	order, err := h.svc.Purchase(ctx, userID, PurchaseRequest{
		ProviderNumberID: req.ProviderNumberID,
		RentalPlanID:     req.RentalPlanID,
		IdempotencyKey:   idempotencyKey,
	})
	if err != nil {
		if errors.Is(err, fulfillment.ErrPendingFulfillment) {
			// Charged, reserved, but held for operator attention: not a
			// client error, so return 202 with the order rather than 4xx/5xx.
			httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"order": toOrderResponse(order), "status": "pending_fulfillment"})
			return
		}
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"order": toOrderResponse(order)})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, rental.ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Request is missing or has invalid fields.", nil)
	case errors.Is(err, wallet.ErrInsufficientFunds):
		httpx.WriteError(w, http.StatusPaymentRequired, "insufficient_funds", "Your wallet balance is too low for this purchase.", nil)
	case errors.Is(err, rental.ErrNumberNotFound):
		httpx.WriteError(w, http.StatusNotFound, "number_not_found", "The requested number was not found.", nil)
	case errors.Is(err, rental.ErrNumberUnavailable):
		httpx.WriteError(w, http.StatusConflict, "number_unavailable", "The requested number is no longer available.", nil)
	case errors.Is(err, rental.ErrPlanUnavailable):
		httpx.WriteError(w, http.StatusConflict, "plan_unavailable", "The requested rental plan is unavailable.", nil)
	case errors.Is(err, rental.ErrIdempotencyConflict), errors.Is(err, wallet.ErrIdempotencyConflict):
		httpx.WriteError(w, http.StatusConflict, "idempotency_conflict", "This idempotency key was already used with a different request.", nil)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}
