package productions

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Public godoc
// @Summary      Список продакшенов (для выбора в профиле спеца)
// @Tags         productions
// @Produce      json
// @Success      200  {object}  listResp
// @Router       /productions [get]
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListActive(r.Context())
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listResp{Items: items})
}

// AdminList godoc
// @Summary      Список всех продакшенов (включая деактивированные)
// @Tags         admin-productions
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  listResp
// @Router       /admin/productions [get]
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listResp{Items: items})
}

type createReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AdminCreate godoc
// @Summary      Создать продакшен
// @Tags         admin-productions
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      createReq  true  "name + description"
// @Success      201   {object}  Production
// @Failure      400   {object}  errorResponse
// @Failure      409   {object}  errorResponse "name_taken: уже есть активный с таким именем"
// @Router       /admin/productions [post]
func (h *Handler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	var in createReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.Create(r.Context(), CreateInput{Name: in.Name, Description: in.Description})
	switch {
	case errors.Is(err, ErrInvalidInput):
		// data-sec D12: только детали обёртки, без префикса "invalid input:".
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrAlreadyExists):
		httpx.WriteErrMsg(w, http.StatusConflict, "name_taken",
			"Активный продакшен с таким именем уже есть.")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusCreated, p)
	}
}

type patchReq struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	IsActive    *bool   `json:"is_active,omitempty"`
}

// AdminPatch godoc
// @Summary      Обновить продакшен
// @Tags         admin-productions
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string    true  "production id"
// @Param        body  body      patchReq  true  "patch fields"
// @Success      200   {object}  Production
// @Failure      400   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /admin/productions/{id} [patch]
func (h *Handler) AdminPatch(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id продакшена.")
		return
	}
	var in patchReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.Patch(r.Context(), id, PatchInput{
		Name:        in.Name,
		Description: in.Description,
		IsActive:    in.IsActive,
	})
	switch {
	case errors.Is(err, ErrInvalidInput):
		// data-sec D12: только детали обёртки, без префикса "invalid input:".
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Продакшен не найден.")
	case errors.Is(err, ErrAlreadyExists):
		httpx.WriteErrMsg(w, http.StatusConflict, "name_taken",
			"Активный продакшен с таким именем уже есть.")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

// AdminDelete godoc
// @Summary      Деактивировать продакшен
// @Description  Мягкое удаление: is_active=false. У живых спецов выбор сохраняется.
// @Tags         admin-productions
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "production id"
// @Success      204
// @Failure      404  {object}  errorResponse
// @Router       /admin/productions/{id} [delete]
func (h *Handler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id продакшена.")
		return
	}
	err = h.svc.Delete(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Продакшен не найден.")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// типы для swaggo
type listResp struct {
	Items []Production `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
