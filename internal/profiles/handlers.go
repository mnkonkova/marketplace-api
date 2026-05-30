package profiles

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Человекочитаемые сообщения. Стабильные коды (`error`) остаются прежними,
// сообщения добавляются в `message` — фронт ветвится по коду, в UI кладёт текст.
const (
	msgInternal     = "Внутренняя ошибка сервера. Попробуйте позже."
	msgNoUser       = "Требуется авторизация."
	msgBadJSON      = "Некорректный JSON в теле запроса."
	msgBadID        = "Неверный формат идентификатора."
	msgNoProfile    = "Профиль не найден."
	msgNotFound     = "Объект не найден."
	msgStale        = "Объект был изменён другим запросом. Перезагрузите данные."
	msgPublishInc   = "Профиль не готов к публикации: проверьте обязательные поля."
	msgEmailUnverif = "Подтвердите email — на него отправлено письмо."
	msgStorageOff   = "Хранилище медиа недоступно."
)

// invalidInputMessage достаёт человеческое сообщение из обёрнутой ErrInvalidInput.
// Сервисный слой обёртывает ошибки как `fmt.Errorf("%w: <detail>", ErrInvalidInput)`,
// поэтому err.Error() выглядит как "invalid input: <detail>". Возвращаем <detail>
// для UI; если префикса нет (на всякий случай), вернётся текст ошибки целиком.
func invalidInputMessage(err error) string {
	const prefix = "invalid input: "
	s := err.Error()
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix)
	}
	return s
}

// Public godoc
// @Summary      Публичный профиль специалиста
// @Tags         profile
// @Produce      json
// @Param        id   path      string  true  "user id"
// @Success      200  {object}  PublicProfile
// @Failure      400  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /specialists/{id} [get]
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrFields(w, http.StatusBadRequest, "bad_id", msgBadID,
			httpx.FieldError{Field: "id", Message: "Должен быть UUID"})
		return
	}
	p, err := h.svc.GetPublic(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Специалист не найден.")
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

// Get godoc
// @Summary      Свой профиль (вместе с контактами)
// @Tags         profile
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  Profile
// @Failure      401  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /me/profile [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	p, err := h.svc.Get(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteErrMsg(w, http.StatusNotFound, "no_profile", msgNoProfile)
		return
	}
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// PatchFull godoc
// @Summary      Обновить свой профиль (атомарно)
// @Description  Одной транзакцией под одной optimistic-lock версией:
// @Description  поля профиля + (опционально) categories + (опционально) skills.
// @Description  Любая секция, оставленная nil/неуказанной, не трогается.
// @Tags         profile
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PatchFullInput  true  "профиль + categories + skills + updated_at"
// @Success      200   {object}  Profile
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/profile [patch]
func (h *Handler) PatchFull(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	var in PatchFullInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	p, err := h.svc.PatchFull(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "no_profile", msgNoProfile)
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at", msgStale)
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

// Publish godoc
// @Summary      Опубликовать профиль
// @Description  Требует подтверждённого email. Без подтверждения — 403 `email_unverified`, фронт должен показать баннер с предложением подтвердить (и кнопку «Отправить ещё раз» → POST /auth/resend-verification).
// @Tags         profile
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  Profile
// @Failure      401  {object}  errorResponse
// @Failure      403  {object}  errorResponse   "email_unverified"
// @Failure      404  {object}  errorResponse
// @Failure      422  {object}  errorResponse
// @Router       /me/profile/publish [post]
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) { h.setPublished(w, r, true) }

// Unpublish godoc
// @Summary      Снять профиль с публикации
// @Tags         profile
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  Profile
// @Failure      401  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /me/profile/unpublish [post]
func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) { h.setPublished(w, r, false) }

func (h *Handler) setPublished(w http.ResponseWriter, r *http.Request, v bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	p, err := h.svc.SetPublished(r.Context(), uid, v)
	var rejected *ProfileRejectedError
	switch {
	case errors.As(err, &rejected):
		// Особый случай: возвращаем расширенный ответ с check-деталями,
		// помимо message — поэтому пишем JSON руками, а не через WriteErrMsg.
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   "profile_rejected",
			"message": "Профиль не прошёл автоматическую проверку.",
			"check":   rejected.Result,
		})
	case errors.Is(err, ErrPublishIncomplete):
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "publish_incomplete", msgPublishInc)
	case errors.Is(err, ErrEmailUnverified):
		httpx.WriteErrMsg(w, http.StatusForbidden, "email_unverified", msgEmailUnverif)
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "no_profile", msgNoProfile)
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

/* ───── portfolio (video) ──────────────────────────────────────── */

// PortfolioList godoc
// @Summary      Свои элементы портфолио
// @Tags         portfolio
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  portfolioListResponse
// @Failure      401  {object}  errorResponse
// @Router       /me/portfolio [get]
func (h *Handler) PortfolioList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	items, err := h.svc.ListPortfolio(r.Context(), uid)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// PortfolioCreate godoc
// @Summary      Добавить элемент портфолио (видео по URL)
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioCreateInput  true  "video payload"
// @Success      201   {object}  PortfolioItem
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Router       /me/portfolio [post]
func (h *Handler) PortfolioCreate(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	var in PortfolioCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	item, err := h.svc.AddPortfolioVideo(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusCreated, item)
	}
}

// PortfolioUploadURL godoc
// @Summary      Presigned PUT URL для аплоада видео в S3
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioUploadURLInput  true  "filename/size"
// @Success      200   {object}  PortfolioUploadURL
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/portfolio/upload-url [post]
func (h *Handler) PortfolioUploadURL(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "storage_disabled", msgStorageOff)
		return
	}
	var in PortfolioUploadURLInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	out, err := h.svc.CreatePortfolioUploadURL(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// ImageUploadURL godoc
// @Summary      Presigned PUT URL для аплоада картинки (аватар / превью)
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      ImageUploadURLInput  true  "filename/size"
// @Success      200   {object}  PortfolioUploadURL
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/uploads/image [post]
//
// ImageUploadURL — presigned PUT для аватара или превью видео. Используется
// одной ручкой; куда положить полученный public_url — решает фронт (PATCH
// /me/profile.avatar_url для аватара или POST /me/portfolio.thumbnail_url
// для превью к ролику).
func (h *Handler) ImageUploadURL(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "storage_disabled", msgStorageOff)
		return
	}
	var in ImageUploadURLInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	out, err := h.svc.CreateImageUploadURL(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// PortfolioSetCategories godoc
// @Summary      Заменить категории у элемента портфолио
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                       true  "portfolio item id"
// @Param        body  body      PortfolioSetCategoriesInput  true  "category codes"
// @Success      200   {object}  PortfolioItem
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/portfolio/{id}/categories [put]
func (h *Handler) PortfolioSetCategories(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrFields(w, http.StatusBadRequest, "bad_id", msgBadID,
			httpx.FieldError{Field: "id", Message: "Должен быть UUID"})
		return
	}
	var in PortfolioSetCategoriesInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	item, err := h.svc.SetPortfolioCategories(r.Context(), uid, itemID, in.Codes, in.UpdatedAt)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", invalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Элемент портфолио не найден.")
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at", msgStale)
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, item)
	}
}

// PortfolioDelete godoc
// @Summary      Удалить элемент портфолио
// @Tags         portfolio
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      string  true  "portfolio item id"
// @Success      204  "no content"
// @Failure      401  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /me/portfolio/{id} [delete]
func (h *Handler) PortfolioDelete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrFields(w, http.StatusBadRequest, "bad_id", msgBadID,
			httpx.FieldError{Field: "id", Message: "Должен быть UUID"})
		return
	}
	if err := h.svc.DeletePortfolioItem(r.Context(), uid, itemID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Элемент портфолио не найден.")
		default:
			httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// типы для swaggo
type errorResponse struct {
	Error string `json:"error"`
}

type portfolioListResponse struct {
	Items []PortfolioItem `json:"items"`
}
