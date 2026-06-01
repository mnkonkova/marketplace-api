package projects

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// ---- Client: чтение комментариев своего проекта ----

// ClientListComments godoc
// @Summary  Комментарии моего проекта (клиент)
// @Tags     client-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} commentsResp
// @Router   /me/projects/{id}/comments [get]
func (h *Handler) ClientListComments(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	// Проверка владения.
	if _, err := h.svc.GetClientProject(r.Context(), pid, uid); err != nil {
		writeServiceErr(w, err)
		return
	}
	items, err := h.svc.ListComments(r.Context(), pid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

// ---- Manager: чтение/запись комментариев и лента ----

// ManagerListComments godoc
// @Summary  Комментарии проекта (менеджер)
// @Tags     manager-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} commentsResp
// @Router   /manager/projects/{id}/comments [get]
func (h *Handler) ManagerListComments(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	items, err := h.svc.ListComments(r.Context(), pid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

type commentReq struct {
	Body string `json:"body"`
}

// ManagerCreateComment godoc
// @Summary  Добавить комментарий (менеджер)
// @Tags     manager-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    body body commentReq true "body"
// @Success  201  {object} Comment
// @Router   /manager/projects/{id}/comments [post]
func (h *Handler) ManagerCreateComment(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	c, err := h.svc.CreateComment(r.Context(), pid, uid, in.Body)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// ManagerListEvents godoc
// @Summary  Лента активности проекта (менеджер)
// @Tags     manager-projects
// @Produce  json
// @Security BearerAuth
// @Param    id     path  string true  "project id"
// @Param    limit  query int    false "default 50, max 200"
// @Param    offset query int    false "default 0"
// @Success  200 {object} eventsResp
// @Router   /manager/projects/{id}/events [get]
func (h *Handler) ManagerListEvents(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, err := h.svc.ListEvents(r.Context(), pid, limit, offset)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, eventsResp{Items: items})
}

// ---- Admin: те же два эндпоинта, но без assigned-фильтра ----

// AdminListProjectEvents godoc
// @Summary  Лента активности любого проекта (админ)
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} eventsResp
// @Router   /admin/projects/{id}/events [get]
func (h *Handler) AdminListProjectEvents(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, err := h.svc.ListEvents(r.Context(), pid, limit, offset)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, eventsResp{Items: items})
}

// AdminListProjectComments godoc
// @Summary  Комментарии любого проекта (админ)
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} commentsResp
// @Router   /admin/projects/{id}/comments [get]
func (h *Handler) AdminListProjectComments(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	items, err := h.svc.ListComments(r.Context(), pid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, commentsResp{Items: items})
}

// AdminCreateProjectComment godoc
// @Summary  Комментировать любой проект (админ)
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    body body commentReq true "body"
// @Success  201  {object} Comment
// @Router   /admin/projects/{id}/comments [post]
func (h *Handler) AdminCreateProjectComment(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	c, err := h.svc.CreateComment(r.Context(), pid, actorID, in.Body)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// типы для swaggo
type commentsResp struct {
	Items []Comment `json:"items"`
}

type eventsResp struct {
	Items []Event `json:"items"`
}
