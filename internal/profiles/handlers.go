package profiles

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	p, err := h.svc.GetPublic(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	p, err := h.svc.Get(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no_profile")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in PatchInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	p, err := h.svc.Patch(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "no_profile")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) SetCategories(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in SetCategoriesInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	p, err := h.svc.SetCategories(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) SetSkills(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in SetSkillsInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	p, err := h.svc.SetSkills(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) { h.setPublished(w, r, true) }

func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) { h.setPublished(w, r, false) }

func (h *Handler) setPublished(w http.ResponseWriter, r *http.Request, v bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	p, err := h.svc.SetPublished(r.Context(), uid, v)
	var rejected *ProfileRejectedError
	switch {
	case errors.As(err, &rejected):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "profile_rejected",
			"check": rejected.Result,
		})
	case errors.Is(err, ErrPublishIncomplete):
		writeErr(w, http.StatusUnprocessableEntity, "publish_incomplete")
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "no_profile")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

/* ───── portfolio (video) ──────────────────────────────────────── */

func (h *Handler) PortfolioList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	items, err := h.svc.ListPortfolio(r.Context(), uid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) PortfolioCreate(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in PortfolioCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	item, err := h.svc.AddPortfolioVideo(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusCreated, item)
	}
}

func (h *Handler) PortfolioUploadURL(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	if !h.svc.MediaAvailable() {
		writeErr(w, http.StatusServiceUnavailable, "storage_disabled")
		return
	}
	var in PortfolioUploadURLInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	out, err := h.svc.CreatePortfolioUploadURL(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

func (h *Handler) PortfolioSetCategories(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	var in PortfolioSetCategoriesInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	item, err := h.svc.SetPortfolioCategories(r.Context(), uid, itemID, in.Codes)
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, item)
	}
}

func (h *Handler) PortfolioDelete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	if err := h.svc.DeletePortfolioItem(r.Context(), uid, itemID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			writeErr(w, http.StatusNotFound, "not_found")
		default:
			writeErr(w, http.StatusInternalServerError, "internal")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
