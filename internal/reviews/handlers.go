package reviews

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type createReq struct {
	LeadID       string `json:"lead_id,omitempty"`
	TargetUserID string `json:"target_user_id"`
	AuthorName   string `json:"author_name,omitempty"`
	Rating       int    `json:"rating"`
	Text         string `json:"text"`
}

// Create godoc
// @Summary      Создать отзыв
// @Description  Оставляет отзыв на специалиста. Если указан lead_id, то клиент должен быть автором лида, а target — принятым получателем.
// @Tags         reviews
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      createReq  true  "review payload"
// @Success      201   {object}  Review
// @Failure      400   {object}  errResponse
// @Failure      401   {object}  errResponse
// @Failure      403   {object}  errResponse
// @Router       /reviews [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	var in createReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	target, err := uuid.Parse(strings.TrimSpace(in.TargetUserID))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_target_user_id")
		return
	}
	var leadID *uuid.UUID
	if s := strings.TrimSpace(in.LeadID); s != "" {
		lid, err := uuid.Parse(s)
		if err != nil {
			httpx.WriteErr(w, http.StatusBadRequest, "bad_lead_id")
			return
		}
		leadID = &lid
	}

	rv, err := h.svc.Create(r.Context(), CreateInput{
		LeadID:       leadID,
		AuthorUserID: uid,
		AuthorName:   in.AuthorName,
		TargetUserID: target,
		Rating:       in.Rating,
		Text:         in.Text,
	})
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrLeadCheck):
		httpx.WriteErr(w, http.StatusForbidden, "lead_does_not_authorize")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusCreated, rv)
	}
}

// Update godoc
// @Summary      Изменить отзыв
// @Description  Изменяет рейтинг и/или текст отзыва. Только автор отзыва.
// @Tags         reviews
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string       true  "review id"
// @Param        body  body      UpdateInput  true  "patch payload"
// @Success      200   {object}  Review
// @Failure      400   {object}  errResponse
// @Failure      401   {object}  errResponse
// @Failure      403   {object}  errResponse
// @Failure      404   {object}  errResponse
// @Router       /reviews/{id} [patch]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_review_id")
		return
	}
	var in UpdateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_json")
		return
	}
	rv, err := h.svc.Update(r.Context(), id, uid, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "review_not_found")
	case errors.Is(err, ErrForbidden):
		httpx.WriteErr(w, http.StatusForbidden, "not_the_author")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		httpx.WriteJSON(w, http.StatusOK, rv)
	}
}

// Delete godoc
// @Summary      Удалить отзыв
// @Description  Удаляет отзыв. Только автор отзыва.
// @Tags         reviews
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      string  true  "review id"
// @Success      204  "no content"
// @Failure      401  {object}  errResponse
// @Failure      403  {object}  errResponse
// @Failure      404  {object}  errResponse
// @Router       /reviews/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_review_id")
		return
	}
	err = h.svc.Delete(r.Context(), id, uid)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "review_not_found")
	case errors.Is(err, ErrForbidden):
		httpx.WriteErr(w, http.StatusForbidden, "not_the_author")
	case err != nil:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListBySpecialist godoc
// @Summary      Отзывы на специалиста (пагинация)
// @Description  Листит отзывы, отсортированные по дате убывания.
// @Tags         reviews
// @Produce      json
// @Param        id      path   string  true   "specialist user id"
// @Param        limit   query  int     false  "default 20, max 100"
// @Param        offset  query  int     false  "default 0"
// @Success      200     {object}  listResponse
// @Failure      400     {object}  errResponse
// @Router       /specialists/{id}/reviews [get]
func (h *Handler) ListBySpecialist(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_specialist_id")
		return
	}
	v := r.URL.Query()
	limit := atoi(v.Get("limit"), 20)
	offset := atoi(v.Get("offset"), 0)

	items, err := h.svc.ListByTarget(r.Context(), id, limit, offset)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listResponse{Items: items})
}

// listResponse / errResponse — публичные, чтобы swaggo подхватил типы.
type listResponse struct {
	Items []Review `json:"items"`
}

type errResponse struct {
	Error string `json:"error"`
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

