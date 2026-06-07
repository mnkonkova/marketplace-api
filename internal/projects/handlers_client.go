package projects

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func clientFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return uuid.Nil, false
	}
	return uid, true
}

func parseProjectStep(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return uuid.Nil, uuid.Nil, false
	}
	sid, err := uuid.Parse(chi.URLParam(r, "step_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_step_id", "Неверный id шага.")
		return uuid.Nil, uuid.Nil, false
	}
	return pid, sid, true
}

// writeServiceErr — общий маппер доменных ошибок в HTTP. Все client-эндпоинты
// одинаково реагируют на ErrNotFound (404), ErrInvalidTransition (409) и т.д.
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		// data-sec D12: только детали обёртки, без префикса "invalid input:".
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrStepNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект или шаг не найден.")
	case errors.Is(err, ErrNotClientStep):
		httpx.WriteErrMsg(w, http.StatusForbidden, "not_client_step",
			"Это действие доступно только на клиентских шагах.")
	case errors.Is(err, ErrNotReviewStep):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "not_review_step",
			"Отзыв можно оставить только на шаге отзыва.")
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteErrMsg(w, http.StatusConflict, "invalid_transition",
			"Шаг уже в другом статусе — перезагрузите страницу.")
	case errors.Is(err, ErrRevisionsExhausted):
		httpx.WriteErrMsg(w, http.StatusConflict, "revisions_exhausted",
			"Лимит правок исчерпан — проект ушёл к менеджеру.")
	default:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// ClientList godoc
// @Summary      Мои проекты (для клиента)
// @Tags         client-projects
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  clientListResp
// @Router       /me/projects [get]
func (h *Handler) ClientList(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListClientProjects(r.Context(), uid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, clientListResp{Items: items})
}

// ClientGetFunnel godoc
// @Summary      Воронка моего проекта (стадии + видимые шаги)
// @Tags         client-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      string  true  "project id"
// @Success      200  {object}  ProjectClientView
// @Failure      404  {object}  errorResponse
// @Router       /me/projects/{id}/funnel [get]
func (h *Handler) ClientGetFunnel(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	view, err := h.svc.GetClientProject(r.Context(), pid, uid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// ClientSubmitReview godoc
// @Summary      Закрыть review-шаг (после оставления отзыва)
// @Tags         client-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id       path      string  true  "project id"
// @Param        step_id  path      string  true  "step id (is_review=true)"
// @Success      200      {object}  Step
// @Router       /me/projects/{id}/steps/{step_id}/submit_review [post]
func (h *Handler) ClientSubmitReview(w http.ResponseWriter, r *http.Request) {
	uid, ok := clientFrom(w, r)
	if !ok {
		return
	}
	pid, sid, ok := parseProjectStep(w, r)
	if !ok {
		return
	}
	// access-check теперь внутри SubmitReview (см. service.go).
	step, err := h.svc.SubmitReview(r.Context(), pid, sid, uid)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, step)
}

// типы для swaggo
type clientListResp struct {
	Items []ProjectClientView `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
