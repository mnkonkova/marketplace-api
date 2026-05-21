package handlers

import (
	"context"
	"net/http"
	"time"

	"marketpclce/internal/httpx"
)

type HealthDB interface {
	Ping(ctx context.Context) error
}

type Health struct{ db HealthDB }

func NewHealth(db HealthDB) *Health { return &Health{db: db} }

func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.db.Ping(ctx); err != nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_unavailable"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
