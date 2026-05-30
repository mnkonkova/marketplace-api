package projects

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// ListSpecialist godoc
// @Summary      Назначенные специалисту проекты
// @Description  Read-only выдача для блока «Назначенные проекты» во вкладке «Продакшен» в кабинете специалиста (Вариант А — никаких мутаций).
// @Tags         me-specialist-projects
// @Security     BearerAuth
// @Produce      json
// @Success      200  {object}  SpecialistProjectsResponse
// @Failure      401  {object}  errorResponse
// @Router       /me/specialist/projects [get]
func (h *Handler) ListSpecialist(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := h.svc.ListSpecialistProjects(r.Context(), uid)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить проекты")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetSpecialist godoc
// @Summary      Карточка назначенного проекта
// @Tags         me-specialist-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Project ID"
// @Success      200  {object}  ProjectSpecialistView
// @Failure      404  {object}  errorResponse
// @Router       /me/specialist/projects/{id} [get]
func (h *Handler) GetSpecialist(w http.ResponseWriter, r *http.Request) {
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
	p, err := h.svc.GetSpecialistProject(r.Context(), id, uid)
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

// GetSpecialistFunnelHandler godoc
// @Summary      Воронка назначенного проекта (только видимые специалисту шаги)
// @Description  Из шаблона исключаются payment/escrow_hold/social_setup/revision_round/nps/review — то, что не относится к работе специалиста.
// @Tags         me-specialist-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Project ID"
// @Success      200  {object}  FunnelView
// @Failure      404  {object}  errorResponse
// @Router       /me/specialist/projects/{id}/funnel [get]
func (h *Handler) GetSpecialistFunnelHandler(w http.ResponseWriter, r *http.Request) {
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
	f, err := h.svc.GetSpecialistFunnel(r.Context(), id, uid)
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

// SpecialistProjectsResponse — обёртка для swagger.
type SpecialistProjectsResponse struct {
	Items []ProjectSpecialistView `json:"items"`
}
