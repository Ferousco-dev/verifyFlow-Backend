package rental

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"migo/internal/auth"
	"migo/internal/httpx"
)

const (
	maxBodyBytes   = 4 << 10
	requestTimeout = 10 * time.Second
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type numberResponse struct {
	ID           string `json:"id"`
	PhoneNumber  string `json:"phone_number"`
	NumberType   string `json:"number_type"`
	SMSEnabled   bool   `json:"sms_enabled"`
	MMSEnabled   bool   `json:"mms_enabled"`
	VoiceEnabled bool   `json:"voice_enabled"`
}

func toNumberResponse(n NumberSummary) numberResponse {
	return numberResponse{
		ID: n.ID, PhoneNumber: n.PhoneNumber, NumberType: n.NumberType,
		SMSEnabled: n.SMSEnabled, MMSEnabled: n.MMSEnabled, VoiceEnabled: n.VoiceEnabled,
	}
}

func (h *Handler) SearchNumbers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := NumberFilter{
		NumberType:   q.Get("type"),
		RequireSMS:   q.Get("sms") == "true",
		RequireMMS:   q.Get("mms") == "true",
		RequireVoice: q.Get("voice") == "true",
		Cursor:       q.Get("cursor"),
	}
	if limit := q.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer.", nil)
			return
		}
		filter.Limit = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	page, err := h.svc.SearchNumbers(ctx, filter)
	if err != nil {
		h.writeError(w, err)
		return
	}
	numbers := make([]numberResponse, 0, len(page.Numbers))
	for _, n := range page.Numbers {
		numbers = append(numbers, toNumberResponse(n))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"numbers": numbers, "next_cursor": page.NextCursor})
}

type planResponse struct {
	ID              string `json:"id"`
	Code            string `json:"code"`
	Name            string `json:"name"`
	DurationSeconds int32  `json:"duration_seconds"`
	PriceMinorUnits int64  `json:"price_minor_units"`
	Currency        string `json:"currency"`
}

func toPlanResponse(p Plan) planResponse {
	return planResponse{
		ID: p.ID, Code: p.Code, Name: p.Name,
		DurationSeconds: p.DurationSeconds, PriceMinorUnits: p.PriceMinorUnits, Currency: p.Currency,
	}
}

func (h *Handler) ListPlans(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	plans, err := h.svc.ListPlans(ctx)
	if err != nil {
		h.writeError(w, err)
		return
	}
	resp := make([]planResponse, 0, len(plans))
	for _, p := range plans {
		resp = append(resp, toPlanResponse(p))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"plans": resp})
}

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

func toOrderResponse(o Order) orderResponse {
	return orderResponse{
		ID: o.ID, RentalID: o.RentalID, ProviderNumberID: o.ProviderNumberID, RentalPlanID: o.RentalPlanID,
		Status: o.Status, PlanCode: o.PlanCode, PlanName: o.PlanName, DurationSeconds: o.DurationSeconds,
		PriceMinorUnits: o.PriceMinorUnits, Currency: o.Currency,
		ReservationExpiresAt: o.ReservationExpiresAt, CreatedAt: o.CreatedAt,
	}
}

func (h *Handler) Reserve(w http.ResponseWriter, r *http.Request) {
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

	reservation, err := h.svc.Reserve(ctx, userID, ReserveRequest{
		ProviderNumberID: req.ProviderNumberID,
		RentalPlanID:     req.RentalPlanID,
		IdempotencyKey:   idempotencyKey,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"order": toOrderResponse(reservation.Order)})
}

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
		return
	}
	orderID := r.PathValue("id")

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	order, err := h.svc.GetOrder(ctx, userID, orderID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": toOrderResponse(order)})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Request is missing or has invalid fields.", nil)
	case errors.Is(err, ErrNumberNotFound):
		httpx.WriteError(w, http.StatusNotFound, "number_not_found", "The requested number was not found.", nil)
	case errors.Is(err, ErrNumberUnavailable):
		httpx.WriteError(w, http.StatusConflict, "number_unavailable", "The requested number is no longer available.", nil)
	case errors.Is(err, ErrPlanUnavailable):
		httpx.WriteError(w, http.StatusConflict, "plan_unavailable", "The requested rental plan is unavailable.", nil)
	case errors.Is(err, ErrIdempotencyConflict):
		httpx.WriteError(w, http.StatusConflict, "idempotency_conflict", "This idempotency key was already used with a different request.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "The requested order was not found.", nil)
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
	}
}
