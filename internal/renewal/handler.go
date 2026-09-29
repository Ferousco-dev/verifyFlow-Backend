package renewal

import (
	"context"
	"net/http"
	"time"

	"migo/internal/httpx"
)

const requestTimeout = 30 * time.Second

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Run handles POST /api/v1/admin/renewals/run (admin only): triggers one
// renewal pass immediately, for ops visibility and manual recovery — the
// scheduled runner in cmd/api calls the same Service.RunDue on its own
// interval.
func (h *Handler) Run(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	result, err := h.svc.RunDue(ctx)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"renewed": result.Renewed,
		"expired": result.Expired,
		"failed":  result.Failed,
	})
}
