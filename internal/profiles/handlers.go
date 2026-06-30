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
	msgInternal       = "Внутренняя ошибка сервера. Попробуйте позже."
	msgNoUser         = "Требуется авторизация."
	msgBadJSON        = "Некорректный JSON в теле запроса."
	msgBadID          = "Неверный формат идентификатора."
	msgNoProfile      = "Профиль не найден."
	msgNotFound       = "Объект не найден."
	msgStale          = "Объект был изменён другим запросом. Перезагрузите данные."
	msgPublishInc     = "Профиль не готов к публикации: проверьте обязательные поля."
	msgEmailUnverif   = "Подтвердите email — на него отправлено письмо."
	msgStorageOff     = "Хранилище медиа недоступно."
)

// Public godoc
// @Summary      Публичный профиль специалиста
// @Tags         profile
// @Produce      json
// @Param        id   path      string  true  "user id"
// @Success      200  {object}  PublicProfile
// @Failure      400  {object}  errorResponse
// @Failure      404  {object}  errorResponse
// @Router       /specialists/{handle} [get]
//
// handle — либо UUID (user_id, для back-compat / direct links), либо
// username (новый красивый URL). Парсим как UUID — если не удалось,
// идём в ResolveUserIDByUsername.
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	handle := chi.URLParam(r, "id")
	var id uuid.UUID
	if u, err := uuid.Parse(handle); err == nil {
		id = u
	} else {
		resolved, rerr := h.svc.ResolveUserIDByUsername(r.Context(), handle)
		if errors.Is(rerr, ErrNotFound) {
			httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Специалист не найден.")
			return
		}
		if rerr != nil {
			httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
			return
		}
		id = resolved
	}
	// Owner-preview: если caller авторизован И смотрит свой профиль —
	// возвращаем независимо от publish/moderation статуса, отмечаем
	// флагом IsPreview. Это даёт спецу «посмотреть как клиент» до publish.
	// Сравнение через OptionalMiddleware: uid пуст если токена не было.
	callerID, authed := auth.UserIDFrom(r.Context())
	if authed && callerID == id {
		p, err := h.svc.GetPublicForOwner(r.Context(), id)
		switch {
		case errors.Is(err, ErrNotFound):
			httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Специалист не найден.")
		case err != nil:
			httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
		default:
			p.IsPreview = !(p.IsPublished && p.ModerationStatus == "approved")
			httpx.WriteJSON(w, http.StatusOK, p)
		}
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
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrUsernameTaken):
		httpx.WriteErrMsg(w, http.StatusConflict, "username_taken",
			"Этот username уже занят, попробуйте другой.")
	case errors.Is(err, ErrInvalidUsername):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_username",
			"username: разрешены латиница, цифры, _ и - (3–30 символов).")
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
		// Сервис возвращает ошибку вида "publish incomplete: <русский детал>"
		// (см. RevalidatePublishInTx и SetPublished). Отрезаем префикс и
		// шлём конкретику фронту, чтобы юзер видел КАКОЕ поле не заполнено.
		detail := strings.TrimPrefix(err.Error(), "publish incomplete: ")
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "publish_incomplete",
			"Профиль не готов к публикации: "+detail+".")
	case errors.Is(err, ErrEmailUnverified):
		httpx.WriteErrMsg(w, http.StatusForbidden, "email_unverified", msgEmailUnverif)
	case errors.Is(err, ErrUserInactive):
		httpx.WriteErrMsg(w, http.StatusForbidden, "inactive",
			"Аккаунт деактивирован, обратитесь в поддержку.")
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
		httpx.SetReqReason(r.Context(), "no_user")
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	var in PortfolioCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.SetReqReason(r.Context(), "bad_json")
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	item, err := h.svc.AddPortfolioVideo(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		// Полный текст ошибки валидации в reason — Loki сразу покажет
		// какое из правил сорвалось (video_url not in bucket / title empty /
		// category mismatch и т.п.).
		httpx.SetReqReason(r.Context(), httpx.InvalidInputMessage(err))
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.SetReqReason(r.Context(), "internal:"+err.Error())
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusCreated, item)
	}
}

// PortfolioPhotoSetCreate godoc
// @Summary      Добавить photo-set в портфолио (1..10 фото = одна карусель)
// @Description  Создаёт portfolio_item kind='image' + N portfolio_images.
//               Каждый image_url должен указывать на наш S3-bucket.
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioPhotoSetCreateInput  true  "title + images"
// @Success      201   {object}  PortfolioItem
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Router       /me/portfolio/photoset [post]
func (h *Handler) PortfolioPhotoSetCreate(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.SetReqReason(r.Context(), "no_user")
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	var in PortfolioPhotoSetCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.SetReqReason(r.Context(), "bad_json")
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	item, err := h.svc.AddPortfolioPhotoSet(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.SetReqReason(r.Context(), httpx.InvalidInputMessage(err))
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.SetReqReason(r.Context(), "internal:"+err.Error())
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
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// PortfolioMultipartStart godoc
// @Summary      Старт S3 multipart upload видео (для файлов > 5 МБ)
// @Description  Возвращает upload_id, ключ и part_size. Фронт нарезает файл на чанки по part_size и для каждой части ходит за presigned PUT в /me/portfolio/multipart/part-url. После всех PUT — /me/portfolio/multipart/complete.
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioMultipartStartInput  true  "filename/content_type/size"
// @Success      200   {object}  PortfolioMultipartStartOutput
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/portfolio/multipart/start [post]
func (h *Handler) PortfolioMultipartStart(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "storage_disabled", msgStorageOff)
		return
	}
	var in PortfolioMultipartStartInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	out, err := h.svc.StartPortfolioMultipart(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// PortfolioMultipartPartURL godoc
// @Summary      Presigned PUT URL для одной части multipart upload'а
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioMultipartPartURLInput  true  "key/upload_id/part_number"
// @Success      200   {object}  PortfolioMultipartPartURLOutput
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/portfolio/multipart/part-url [post]
func (h *Handler) PortfolioMultipartPartURL(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "storage_disabled", msgStorageOff)
		return
	}
	var in PortfolioMultipartPartURLInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	out, err := h.svc.PortfolioMultipartPartURL(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// PortfolioMultipartComplete godoc
// @Summary      Завершить multipart upload (собрать чанки)
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioMultipartCompleteInput  true  "key/upload_id/parts"
// @Success      204
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/portfolio/multipart/complete [post]
func (h *Handler) PortfolioMultipartComplete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "storage_disabled", msgStorageOff)
		return
	}
	var in PortfolioMultipartCompleteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	err := h.svc.CompletePortfolioMultipart(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// PortfolioMultipartAbort godoc
// @Summary      Отменить multipart upload (удалить уже залитые части)
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PortfolioMultipartAbortInput  true  "key/upload_id"
// @Success      204
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      503   {object}  errorResponse
// @Router       /me/portfolio/multipart/abort [post]
func (h *Handler) PortfolioMultipartAbort(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErrMsg(w, http.StatusServiceUnavailable, "storage_disabled", msgStorageOff)
		return
	}
	var in PortfolioMultipartAbortInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	err := h.svc.AbortPortfolioMultipart(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		w.WriteHeader(http.StatusNoContent)
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
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
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
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
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

// PortfolioUpdate godoc
// @Summary      Обновить title/description элемента портфолио
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string               true  "portfolio item id"
// @Param        body  body      PortfolioPatchInput  true  "patch fields"
// @Success      200   {object}  PortfolioItem
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/portfolio/{id} [patch]
func (h *Handler) PortfolioUpdate(w http.ResponseWriter, r *http.Request) {
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
	var in PortfolioPatchInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	item, err := h.svc.UpdatePortfolio(r.Context(), uid, itemID, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Элемент портфолио не найден.")
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at", msgStale)
	case err != nil:
		httpx.SetReqReason(r.Context(), "internal:"+err.Error())
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, item)
	}
}

// PortfolioImagesAppend godoc
// @Summary      Добавить N фото в существующий photo-set
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                       true  "portfolio item id"
// @Param        body  body      PortfolioImagesAppendInput   true  "images to append"
// @Success      200   {array}   PortfolioImage
// @Failure      400   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Router       /me/portfolio/{id}/images [post]
func (h *Handler) PortfolioImagesAppend(w http.ResponseWriter, r *http.Request) {
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
	var in PortfolioImagesAppendInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	res, err := h.svc.AppendPhotosToSet(r.Context(), uid, itemID, in.Images)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Фото-кейс не найден.")
	case err != nil:
		httpx.SetReqReason(r.Context(), "internal:"+err.Error())
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"images":     res.Images,
			"updated_at": res.UpdatedAt,
		})
	}
}

// PortfolioImagesReorder godoc
// @Summary      Поменять порядок фото в photo-set'е (drag-and-drop)
// @Tags         portfolio
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                         true  "portfolio item id"
// @Param        body  body      PortfolioImagesReorderInput    true  "image_ids в желаемом порядке"
// @Success      200   {array}   PortfolioImage
// @Failure      400   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Router       /me/portfolio/{id}/images/order [put]
func (h *Handler) PortfolioImagesReorder(w http.ResponseWriter, r *http.Request) {
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
	var in PortfolioImagesReorderInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", msgBadJSON)
		return
	}
	ids := make([]uuid.UUID, 0, len(in.ImageIDs))
	for _, raw := range in.ImageIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "image_ids: каждый должен быть UUID")
			return
		}
		ids = append(ids, id)
	}
	res, err := h.svc.ReorderSetPhotos(r.Context(), uid, itemID, ids)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Фото-кейс не найден.")
	case err != nil:
		httpx.SetReqReason(r.Context(), "internal:"+err.Error())
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"images":     res.Images,
			"updated_at": res.UpdatedAt,
		})
	}
}

// PortfolioImageDelete godoc
// @Summary      Удалить одно фото из photo-set'а
// @Description  Если в сете остаётся 0 фото — каскадом удаляется сам элемент портфолио.
// @Tags         portfolio
// @Produce      json
// @Security     BearerAuth
// @Param        img_id  path      string  true  "portfolio image id"
// @Success      204     "no content"
// @Failure      401     {object}  errorResponse
// @Failure      404     {object}  errorResponse
// @Router       /me/portfolio/images/{img_id} [delete]
func (h *Handler) PortfolioImageDelete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", msgNoUser)
		return
	}
	imageID, err := uuid.Parse(chi.URLParam(r, "img_id"))
	if err != nil {
		httpx.WriteErrFields(w, http.StatusBadRequest, "bad_id", msgBadID,
			httpx.FieldError{Field: "img_id", Message: "Должен быть UUID"})
		return
	}
	if err := h.svc.DeletePortfolioImage(r.Context(), uid, imageID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Фото не найдено.")
		default:
			httpx.SetReqReason(r.Context(), "internal:"+err.Error())
			httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", msgInternal)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
