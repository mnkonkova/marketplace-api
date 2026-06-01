package projects

import (
	"net/http"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// SpecialistList godoc
// @Summary  Назначенные мне проекты (специалист)
// @Tags     specialist-projects
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} managerListResp
// @Router   /me/specialist/projects [get]
func (h *Handler) SpecialistList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	items, err := h.svc.ListBySpecialist(r.Context(), uid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managerListResp{Items: items})
}

// SpecialistGetFunnel godoc
// @Summary  Воронка моего проекта (специалист, read-only)
// @Tags     specialist-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} ProjectFullView
// @Router   /me/specialist/projects/{id}/funnel [get]
func (h *Handler) SpecialistGetFunnel(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	pid, err := uuid.Parse(chiURLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	// Проверка: специалист — назначенный исполнитель?
	p, err := h.svc.repo.GetByID(r.Context(), pid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	if p.SpecialistUserID == nil || *p.SpecialistUserID != uid {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден или не назначен вам.")
		return
	}
	view, err := h.svc.GetFull(r.Context(), pid, uuid.Nil)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}
