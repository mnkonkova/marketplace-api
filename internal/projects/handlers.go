package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

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

// ─── Клиентские POST: approve / request_revision / submit_review ───

// ClientApprove godoc
// @Summary      Клиент: принять шаг (client_review → done)
// @Tags         me-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id       path  string            true   "Project ID"
// @Param        step_id  path  string            true   "Step ID"
// @Param        body     body  StepActionInput   false  "updated_at"
// @Success      200  {object}  StepView
// @Failure      409  {object}  errorResponse "stale_updated_at"
// @Failure      422  {object}  errorResponse "invalid_transition"
// @Router       /me/projects/{id}/steps/{step_id}/approve [post]
func (h *Handler) ClientApprove(w http.ResponseWriter, r *http.Request) {
	uid, projectID, stepID, expected, _, ok := h.parseClientStepReq(w, r)
	if !ok {
		return
	}
	view, err := h.svc.ApproveStep(r.Context(), projectID, stepID, uid, expected)
	h.writeStepResult(w, view, err)
}

// ClientRequestRevision godoc
// @Summary      Клиент: запрос правок (client_review → rejected; цикл правок)
// @Tags         me-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id       path  string            true   "Project ID"
// @Param        step_id  path  string            true   "Step ID"
// @Param        body     body  StepActionInput   false  "updated_at + comment"
// @Success      200  {object}  StepView
// @Router       /me/projects/{id}/steps/{step_id}/request_revision [post]
func (h *Handler) ClientRequestRevision(w http.ResponseWriter, r *http.Request) {
	uid, projectID, stepID, expected, comment, ok := h.parseClientStepReq(w, r)
	if !ok {
		return
	}
	view, err := h.svc.RequestRevision(r.Context(), projectID, stepID, uid, comment, expected)
	h.writeStepResult(w, view, err)
}

// SubmitReviewInput — тело POST .../steps/.../submit_review.
type SubmitReviewInput struct {
	Rating    int        `json:"rating"`
	Text      string     `json:"text"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// SubmitReviewResponse — обогащённый ответ: помимо step ещё review_id.
type SubmitReviewResponse struct {
	Step     StepView  `json:"step"`
	ReviewID uuid.UUID `json:"review_id"`
}

// ClientSubmitReview godoc
// @Summary      Клиент: оставить отзыв (review → done)
// @Tags         me-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id       path  string             true   "Project ID"
// @Param        step_id  path  string             true   "Step ID"
// @Param        body     body  SubmitReviewInput  true   "rating 1..5 + text"
// @Success      200  {object}  SubmitReviewResponse
// @Router       /me/projects/{id}/steps/{step_id}/submit_review [post]
func (h *Handler) ClientSubmitReview(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный project ID")
		return
	}
	stepID, err := uuid.Parse(chi.URLParam(r, "step_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_step_id", "Некорректный step ID")
		return
	}
	var in SubmitReviewInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	view, reviewID, err := h.svc.SubmitReview(r.Context(), projectID, stepID, uid, in.Rating, in.Text, in.UpdatedAt)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at", "Шаг был изменён.")
		return
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось отправить отзыв")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, SubmitReviewResponse{Step: view, ReviewID: reviewID})
}

// parseClientStepReq — общий декодер для approve/request_revision.
// Возвращает uid, projectID, stepID, expectedUpdatedAt (nullable),
// comment, ok=false при ошибке (ответ уже отправлен).
func (h *Handler) parseClientStepReq(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, *time.Time, string, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return uuid.Nil, uuid.Nil, uuid.Nil, nil, "", false
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный project ID")
		return uuid.Nil, uuid.Nil, uuid.Nil, nil, "", false
	}
	stepID, err := uuid.Parse(chi.URLParam(r, "step_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_step_id", "Некорректный step ID")
		return uuid.Nil, uuid.Nil, uuid.Nil, nil, "", false
	}
	var in StepActionInput
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
			return uuid.Nil, uuid.Nil, uuid.Nil, nil, "", false
		}
	}
	var expected *time.Time
	if in.UpdatedAt != nil && !in.UpdatedAt.IsZero() {
		expected = in.UpdatedAt
	}
	return uid, projectID, stepID, expected, in.Comment, true
}

func (h *Handler) writeStepResult(w http.ResponseWriter, view StepView, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at", "Шаг был изменён.")
		return
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
		return
	case errors.Is(err, ErrForbidden):
		httpx.WriteErr(w, http.StatusForbidden, "forbidden")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось обновить шаг")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// ClientProjectsResponse — swaggo wrapper для типизации /me/projects.
type ClientProjectsResponse struct {
	Items []ProjectClientView `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
