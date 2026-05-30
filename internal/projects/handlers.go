package projects

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ListClient godoc
// @Summary      Список проектов клиента
// @Tags         me-projects
// @Security     BearerAuth
// @Produce      json
// @Success      200  {object}  ClientProjectsResponse
// @Failure      401  {object}  errorResponse
// @Router       /me/projects [get]
func (h *Handler) ListClient(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := h.svc.ListClientProjects(r.Context(), uid)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить проекты")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetClient godoc
// @Summary      Карточка проекта клиента
// @Tags         me-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Project ID"
// @Success      200  {object}  ProjectClientView
// @Failure      404  {object}  errorResponse
// @Router       /me/projects/{id} [get]
func (h *Handler) GetClient(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	p, err := h.svc.GetClientProject(r.Context(), id, uid)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить проект")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// GetClientFunnel godoc
// @Summary      Воронка проекта клиента (стадии и видимые шаги)
// @Tags         me-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Project ID"
// @Success      200  {object}  FunnelView
// @Failure      404  {object}  errorResponse
// @Router       /me/projects/{id}/funnel [get]
func (h *Handler) GetClientFunnel(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	f, err := h.svc.GetClientFunnel(r.Context(), id, uid)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить воронку")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

// ListClientByLead godoc
// @Summary      Все проекты клиента по одному брифу (лиду)
// @Description  Используется фронтом для группировки проектов под одной шапкой «Бриф от X». Для marketplace-source проектов; для своих лида нет → пустой массив.
// @Tags         me-projects
// @Security     BearerAuth
// @Produce      json
// @Param        lead_id  path      string  true  "Lead ID"
// @Success      200  {object}  ClientProjectsResponse
// @Router       /me/projects/by_lead/{lead_id} [get]
func (h *Handler) ListClientByLead(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	leadID, err := uuid.Parse(chi.URLParam(r, "lead_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_lead_id", "Некорректный lead_id")
		return
	}
	items, err := h.svc.ListClientProjectsByLead(r.Context(), uid, leadID)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить проекты")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ClientProjectsResponse — swaggo wrapper для типизации /me/projects.
type ClientProjectsResponse struct {
	Items []ProjectClientView `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
