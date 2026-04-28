package clarify

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"marketpclce/internal/llm"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type request struct {
	Category string    `json:"category"`
	History  []Message `json:"history"`
}

func (h *Handler) Clarify(w http.ResponseWriter, r *http.Request) {
	var in request
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	if len(in.History) == 0 {
		writeErr(w, http.StatusBadRequest, "empty_history")
		return
	}

	res, err := h.svc.Run(r.Context(), Input{Category: in.Category, History: in.History})
	switch {
	case errors.Is(err, ErrLLMDisabled):
		writeErr(w, http.StatusServiceUnavailable, "llm_disabled")
		return
	case err != nil:
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			slog.Error("clarify llm api", "status", apiErr.Status, "body", apiErr.Body)
		} else {
			slog.Error("clarify", "err", err)
		}
		writeErr(w, http.StatusBadGateway, "clarify_failed")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
