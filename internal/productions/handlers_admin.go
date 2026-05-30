package productions

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// AdminList godoc
// @Summary      Список всех продакшенов (включая архивные)
// @Description  Только для роли admin/moderator или service-токена (Directus). Включает is_active=false.
// @Tags         admin-productions
// @Security     BearerAuth
// @Produce      json
// @Success      200  {object}  AdminProductionsResponse
// @Failure      401  {object}  errorResponse
// @Failure      403  {object}  errorResponse
// @Router       /admin/productions [get]
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListAll(r.Context())
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить продакшены")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// AdminGet godoc
// @Summary      Получить продакшен по ID
// @Tags         admin-productions
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Production ID (UUID)"
// @Success      200  {object}  Production
// @Failure      400  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /admin/productions/{id} [get]
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	p, err := h.svc.GetByID(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить продакшен")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminCreate godoc
// @Summary      Создать продакшен
// @Tags         admin-productions
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body  body      CreateInput  true  "Поля создания"
// @Success      201   {object}  Production
// @Failure      400   {object}  errorResponse
// @Failure      409   {object}  errorResponse "Продакшен с таким именем уже существует среди активных"
// @Router       /admin/productions [post]
func (h *Handler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	p, err := h.svc.Create(r.Context(), in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input",
			"Имя 2–120 символов, описание ≤1000")
		return
	case errors.Is(err, ErrDuplicateName):
		httpx.WriteErrMsg(w, http.StatusConflict, "duplicate_name",
			"Продакшен с таким именем уже есть среди активных")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось создать продакшен")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

// AdminUpdate godoc
// @Summary      Изменить продакшен (optimistic-lock через updated_at)
// @Description  Клиент должен прислать updated_at, полученный из GET/PATCH. Несовпадение → 409.
// @Tags         admin-productions
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id    path      string       true  "Production ID (UUID)"
// @Param        body  body      UpdateInput  true  "Частичный апдейт + updated_at"
// @Success      200   {object}  Production
// @Failure      400   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse "stale_updated_at | duplicate_name"
// @Router       /admin/productions/{id} [patch]
func (h *Handler) AdminUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	var in UpdateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	p, err := h.svc.Update(r.Context(), id, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input",
			"Имя 2–120 символов, описание ≤1000")
		return
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at",
			"Запись была изменена кем-то другим. Перезагрузите её и повторите.")
		return
	case errors.Is(err, ErrDuplicateName):
		httpx.WriteErrMsg(w, http.StatusConflict, "duplicate_name",
			"Продакшен с таким именем уже есть среди активных")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось обновить продакшен")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminDeactivate godoc
// @Summary      Деактивировать продакшен (soft-delete)
// @Description  Запись остаётся в БД (is_active=false). Привязанные специалисты не блокируются. В публичном /productions запись больше не появляется.
// @Tags         admin-productions
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id            path   string    true   "Production ID (UUID)"
// @Param        updated_at    query  string    false  "Optimistic-lock версия (RFC3339). Если задана и не совпадает — 409."
// @Success      200   {object}  Production
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse "stale_updated_at"
// @Router       /admin/productions/{id} [delete]
func (h *Handler) AdminDeactivate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	var expected *time.Time
	if v := r.URL.Query().Get("updated_at"); v != "" {
		t, perr := time.Parse(time.RFC3339Nano, v)
		if perr != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_updated_at",
				"updated_at должен быть RFC3339")
			return
		}
		expected = &t
	}
	p, err := h.svc.Deactivate(r.Context(), id, expected)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at",
			"Запись была изменена кем-то другим. Перезагрузите её и повторите.")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось деактивировать продакшен")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}
