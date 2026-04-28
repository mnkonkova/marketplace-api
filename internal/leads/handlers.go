package leads

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type createReq struct {
	ClientName     string   `json:"client_name"`
	ClientContact  string   `json:"client_contact"`
	Brief          string   `json:"brief"`
	BudgetMin      *int     `json:"budget_min"`
	BudgetMax      *int     `json:"budget_max"`
	Deadline       string   `json:"deadline"`
	TargetCategory string   `json:"target_category"`
	SpecialistIDs  []string `json:"specialist_ids"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in createReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}

	ids := make([]uuid.UUID, 0, len(in.SpecialistIDs))
	for _, raw := range in.SpecialistIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_specialist_id")
			return
		}
		ids = append(ids, id)
	}

	var deadline *time.Time
	if s := strings.TrimSpace(in.Deadline); s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_deadline")
			return
		}
		deadline = &d
	}

	var clientUserID *uuid.UUID
	if uid, ok := auth.UserIDFrom(r.Context()); ok {
		clientUserID = &uid
	}

	id, err := h.svc.Create(r.Context(), CreateInput{
		ClientUserID:       clientUserID,
		ClientName:         in.ClientName,
		ClientContact:      in.ClientContact,
		Brief:              in.Brief,
		BudgetMin:          in.BudgetMin,
		BudgetMax:          in.BudgetMax,
		Deadline:           deadline,
		TargetCategoryCode: in.TargetCategory,
		SpecialistIDs:      ids,
	})
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNoSpecialists):
		writeErr(w, http.StatusBadRequest, "no_valid_specialists")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
	}
}

func (h *Handler) ListIncoming(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	v := r.URL.Query()
	status := v.Get("status")
	limit := atoi(v.Get("limit"), 20)
	offset := atoi(v.Get("offset"), 0)

	items, err := h.svc.ListIncoming(r.Context(), uid, status, limit, offset)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type recipientReq struct {
	Status string `json:"status"`
}

func (h *Handler) UpdateRecipient(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	leadID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_lead_id")
		return
	}
	var in recipientReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	err = h.svc.UpdateRecipientStatus(r.Context(), leadID, uid, in.Status)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrRecipientMissing):
		writeErr(w, http.StatusNotFound, "recipient_not_found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": in.Status})
	}
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
