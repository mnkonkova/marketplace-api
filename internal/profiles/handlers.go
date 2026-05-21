package profiles

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

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
		httpx.WriteErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	p, err := h.svc.GetPublic(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	p, err := h.svc.Get(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		httpx.WriteErr(w, http.StatusNotFound, "no_profile")
		return
	}
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// Patch godoc
// @Summary      Частично обновить свой профиль
// @Tags         profile
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      PatchInput  true  "поля для апдейта"
// @Success      200   {object}  Profile
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/profile [patch]
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in PatchInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	p, err := h.svc.Patch(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "no_profile")
	case errors.Is(err, ErrConflict):
		httpx.WriteErr(w, http.StatusConflict, "stale_updated_at")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

// SetCategories godoc
// @Summary      Заменить список категорий специалиста
// @Tags         profile
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      SetCategoriesInput  true  "categories"
// @Success      200   {object}  Profile
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/profile/categories [put]
func (h *Handler) SetCategories(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in SetCategoriesInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	p, err := h.svc.SetCategories(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "no_profile")
	case errors.Is(err, ErrConflict):
		httpx.WriteErr(w, http.StatusConflict, "stale_updated_at")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

// SetSkills godoc
// @Summary      Заменить список навыков
// @Tags         profile
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      SetSkillsInput  true  "skills"
// @Success      200   {object}  Profile
// @Failure      400   {object}  errorResponse
// @Failure      401   {object}  errorResponse
// @Failure      404   {object}  errorResponse
// @Failure      409   {object}  errorResponse
// @Router       /me/profile/skills [put]
func (h *Handler) SetSkills(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in SetSkillsInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	p, err := h.svc.SetSkills(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "no_profile")
	case errors.Is(err, ErrConflict):
		httpx.WriteErr(w, http.StatusConflict, "stale_updated_at")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, p)
	}
}

// Publish godoc
// @Summary      Опубликовать профиль
// @Tags         profile
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  Profile
// @Failure      401  {object}  errorResponse
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	p, err := h.svc.SetPublished(r.Context(), uid, v)
	var rejected *ProfileRejectedError
	switch {
	case errors.As(err, &rejected):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "profile_rejected",
			"check": rejected.Result,
		})
	case errors.Is(err, ErrPublishIncomplete):
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "publish_incomplete")
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "no_profile")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	items, err := h.svc.ListPortfolio(r.Context(), uid)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in PortfolioCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	item, err := h.svc.AddPortfolioVideo(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "storage_disabled")
		return
	}
	var in PortfolioUploadURLInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	out, err := h.svc.CreatePortfolioUploadURL(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	if !h.svc.MediaAvailable() {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "storage_disabled")
		return
	}
	var in ImageUploadURLInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	out, err := h.svc.CreateImageUploadURL(r.Context(), uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	var in PortfolioSetCategoriesInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	item, err := h.svc.SetPortfolioCategories(r.Context(), uid, itemID, in.Codes, in.UpdatedAt)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrConflict):
		httpx.WriteErr(w, http.StatusConflict, "stale_updated_at")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_id")
		return
	}
	if err := h.svc.DeletePortfolioItem(r.Context(), uid, itemID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			httpx.WriteErr(w, http.StatusNotFound, "not_found")
		default:
			httpx.WriteErr(w, http.StatusInternalServerError, "internal")
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
