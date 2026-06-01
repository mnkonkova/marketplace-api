package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// AdminListFunnelTemplates godoc
// @Summary      Список активных воронок (для дропдауна выбора при создании проекта)
// @Description  Используется Directus Flow «Create manual project» для выпадающего списка. Содержит код+версию (фронту/Flow нужны они в POST /admin/projects).
// @Tags         admin-funnel-templates
// @Security     BearerAuth
// @Produce      json
// @Success      200  {object}  FunnelTemplatesResponse
// @Router       /admin/funnel-templates [get]
func (h *Handler) AdminListFunnelTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListFunnelTemplates(r.Context())
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить воронки")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// FunnelTemplatesResponse — обёртка для swagger типизации.
type FunnelTemplatesResponse struct {
	Items []FunnelTemplateSummary `json:"items"`
}

// AdminList godoc
// @Summary      Список проектов (admin/moderator)
// @Tags         admin-projects
// @Security     BearerAuth
// @Produce      json
// @Param        status      query  string  false  "draft|active|on_hold|done|cancelled|dispute"
// @Param        source      query  string  false  "marketplace|manual|referral|returning_client"
// @Param        client      query  string  false  "client_user_id (UUID)"
// @Param        specialist  query  string  false  "specialist_user_id (UUID)"
// @Param        assigned    query  string  false  "assigned_to_user_id (UUID)"
// @Param        overdue     query  bool    false  "только с просроченными шагами"
// @Param        limit       query  int     false  "default 100, max 500"
// @Param        offset      query  int     false  "default 0"
// @Success      200  {object}  AdminProjectsResponse
// @Router       /admin/projects [get]
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	f := AdminListFilter{
		Status:  ProjectStatus(r.URL.Query().Get("status")),
		Source:  ProjectSource(r.URL.Query().Get("source")),
		Overdue: r.URL.Query().Get("overdue") == "true",
	}
	if v := r.URL.Query().Get("client"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			f.Client = &id
		}
	}
	if v := r.URL.Query().Get("specialist"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			f.Specialist = &id
		}
	}
	if v := r.URL.Query().Get("assigned"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			f.AssignedTo = &id
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}
	items, err := h.svc.ListAdmin(r.Context(), f)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить проекты")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// AdminGet godoc
// @Summary      Карточка проекта (admin)
// @Tags         admin-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Project ID"
// @Success      200  {object}  ProjectAdminView
// @Failure      404  {object}  errorResponse
// @Router       /admin/projects/{id} [get]
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	p, err := h.svc.GetAdmin(r.Context(), id)
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

// AdminFunnel godoc
// @Summary      Воронка проекта (admin) — все шаги без фильтра
// @Tags         admin-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Project ID"
// @Success      200  {object}  FunnelView
// @Router       /admin/projects/{id}/funnel [get]
func (h *Handler) AdminFunnel(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	f, err := h.svc.GetAdminFunnel(r.Context(), id)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить воронку")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

// AdminEvents godoc
// @Summary      Аудит-лог проекта
// @Tags         admin-projects
// @Security     BearerAuth
// @Produce      json
// @Param        id     path   string  true   "Project ID"
// @Param        limit  query  int     false  "default 100, max 500"
// @Success      200  {object}  AdminEventsResponse
// @Router       /admin/projects/{id}/events [get]
func (h *Handler) AdminEvents(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.svc.ListEvents(r.Context(), id, limit)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось загрузить события")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// AdminCreateManual godoc
// @Summary      Создать manual-проект (admin only)
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body  body      CreateManualInput  true  "поля для создания"
// @Success      201   {object}  ProjectAdminView
// @Failure      400   {object}  errorResponse
// @Router       /admin/projects [post]
func (h *Handler) AdminCreateManual(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context()) // actor для аудита, может быть nil для service-token
	var in CreateManualInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	if in.TemplateVersion == 0 {
		in.TemplateVersion = 1
	}
	if in.TemplateCode == "" {
		in.TemplateCode = "video_production"
	}
	p, err := h.svc.StartProjectManual(r.Context(), in, uid)
	if mappedErr := mapCreateErr(w, err); mappedErr {
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

// AdminCreateFromRecipient godoc
// @Summary      Создать marketplace-проект из принятого recipient'а
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        body  body      CreateFromRecipientInput  true  "lead_id + specialist + title"
// @Success      201   {object}  ProjectAdminView
// @Failure      409   {object}  errorResponse "already_started | recipient_not_ready"
// @Router       /admin/projects/from_recipient [post]
func (h *Handler) AdminCreateFromRecipient(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	var in CreateFromRecipientInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	if in.TemplateVersion == 0 {
		in.TemplateVersion = 1
	}
	if in.TemplateCode == "" {
		in.TemplateCode = "video_production"
	}
	p, err := h.svc.StartFromRecipient(r.Context(), in, uid)
	if mappedErr := mapCreateErr(w, err); mappedErr {
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

// AdminBulkFromLead godoc
// @Summary      Bulk-создание проектов из всех accepted recipients одного лида
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        lead_id  path      string             true  "Lead ID"
// @Param        body     body      BulkFromLeadInput  true  "title + assigned"
// @Success      201      {object}  BulkStartResultResponse
// @Router       /admin/projects/from_lead/{lead_id}/bulk [post]
func (h *Handler) AdminBulkFromLead(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFrom(r.Context())
	leadID, err := uuid.Parse(chi.URLParam(r, "lead_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_lead_id", "Некорректный lead_id")
		return
	}
	var in BulkFromLeadInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	if in.TemplateCode == "" {
		in.TemplateCode = "video_production"
	}
	if in.TemplateVersion == 0 {
		in.TemplateVersion = 1
	}
	result, err := h.svc.StartFromAllAcceptedRecipients(
		r.Context(), leadID, in.TemplateCode, in.TemplateVersion, in.Title, in.AssignedToUserID, uid,
	)
	if mappedErr := mapCreateErr(w, err); mappedErr {
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, BulkStartResultResponse{
		Created: result.Created,
		Skipped: result.Skipped,
	})
}

// AdminPatch godoc
// @Summary      Частичный апдейт проекта (whitelist + optimistic-lock)
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id    path      string           true  "Project ID"
// @Param        body  body      PatchAdminInput  true  "title/budget/notes/assigned/status + updated_at"
// @Success      200   {object}  ProjectAdminView
// @Failure      409   {object}  errorResponse "stale_updated_at"
// @Failure      422   {object}  errorResponse "invalid_transition"
// @Router       /admin/projects/{id} [patch]
func (h *Handler) AdminPatch(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Некорректный ID")
		return
	}
	var in PatchAdminInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
		return
	}
	p, err := h.svc.UpdateAdmin(r.Context(), id, in)
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteErrMsg(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at",
			"Запись была изменена кем-то другим. Перезагрузите её и повторите.")
		return
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case err != nil:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось обновить проект")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminStartStep, AdminCompleteStep, AdminSkipStep — переходы шагов.

// AdminStartStep godoc
// @Summary      Перевести шаг в in_progress (ETA пересчитывается)
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id          path  string  true  "Project ID"
// @Param        step_id     path  string  true  "Step ID"
// @Param        body        body  StepActionInput  false  "updated_at для optimistic-lock"
// @Success      200  {object}  StepView
// @Failure      409  {object}  errorResponse "stale_updated_at"
// @Failure      422  {object}  errorResponse "invalid_transition"
// @Router       /admin/projects/{id}/steps/{step_id}/start [post]
func (h *Handler) AdminStartStep(w http.ResponseWriter, r *http.Request) {
	h.adminTransition(w, r, func(svc *Service, projectID, stepID, actor uuid.UUID, expected *time.Time, _ string) (StepView, error) {
		return svc.StartStep(r.Context(), projectID, stepID, actor, expected)
	})
}

// AdminCompleteStep godoc
// @Summary      Перевести шаг в done (закрывает стадию/проект)
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id      path  string            true  "Project ID"
// @Param        step_id path  string            true  "Step ID"
// @Param        body    body  StepActionInput   false "updated_at + comment"
// @Success      200  {object}  StepView
// @Router       /admin/projects/{id}/steps/{step_id}/complete [post]
func (h *Handler) AdminCompleteStep(w http.ResponseWriter, r *http.Request) {
	h.adminTransition(w, r, func(svc *Service, projectID, stepID, actor uuid.UUID, expected *time.Time, _ string) (StepView, error) {
		return svc.CompleteStep(r.Context(), projectID, stepID, actor, expected)
	})
}

// AdminSkipStep godoc
// @Summary      Skip опционального шага (рекомендуется comment)
// @Tags         admin-projects
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        id      path  string            true  "Project ID"
// @Param        step_id path  string            true  "Step ID"
// @Param        body    body  StepActionInput   true  "updated_at + comment"
// @Success      200  {object}  StepView
// @Router       /admin/projects/{id}/steps/{step_id}/skip [post]
func (h *Handler) AdminSkipStep(w http.ResponseWriter, r *http.Request) {
	h.adminTransition(w, r, func(svc *Service, projectID, stepID, actor uuid.UUID, expected *time.Time, comment string) (StepView, error) {
		return svc.SkipStep(r.Context(), projectID, stepID, actor, comment, expected)
	})
}

// AdminAcceptRecipient godoc
// @Summary      Менеджер апрувит recipient'а за свой продакшен
// @Description  Используется когда специалист не успел сам — менеджер выставляет recipient.status='accepted' от его лица.
// @Tags         admin-projects
// @Security     BearerAuth
// @Param        lead_id        path  string  true  "Lead ID"
// @Param        specialist_id  path  string  true  "Specialist user ID"
// @Success      204  "no content"
// @Router       /admin/lead_recipients/{lead_id}/{specialist_id}/accept [post]
func (h *Handler) AdminAcceptRecipient(w http.ResponseWriter, r *http.Request) {
	leadID, err := uuid.Parse(chi.URLParam(r, "lead_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_lead_id", "Некорректный lead_id")
		return
	}
	specID, err := uuid.Parse(chi.URLParam(r, "specialist_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_specialist_id", "Некорректный specialist_id")
		return
	}
	if err := h.svc.AcceptRecipient(r.Context(), leadID, specID); err != nil {
		// leads.Service отдаёт ErrInvalidInput / ErrConflict / ErrNotFound —
		// мапим обобщённо: 500 при неизвестном.
		switch {
		case errors.Is(err, ErrInvalidInput):
			httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
		default:
			httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось принять recipient'a")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Helpers ───────────────────────────────────────────────────────

// adminTransition — общий обработчик POST /steps/.../{action}.
type stepActionFn func(svc *Service, projectID, stepID, actor uuid.UUID, expected *time.Time, comment string) (StepView, error)

func (h *Handler) adminTransition(w http.ResponseWriter, r *http.Request, fn stepActionFn) {
	uid, _ := auth.UserIDFrom(r.Context())
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
	in := StepActionInput{}
	// Body опциональный для start/complete; для skip обычно есть comment.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Невалидный JSON")
			return
		}
	}
	var expected *time.Time
	if in.UpdatedAt != nil && !in.UpdatedAt.IsZero() {
		expected = in.UpdatedAt
	}
	view, err := fn(h.svc, projectID, stepID, uid, expected, strings.TrimSpace(in.Comment))
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at",
			"Шаг был изменён кем-то другим. Перезагрузите его и повторите.")
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

// mapCreateErr — единый маппинг ошибок для create-методов. Возвращает
// true если ответ уже отправлен (хендлер должен return'нуть).
func mapCreateErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrRecipientNotReady):
		httpx.WriteErrMsg(w, http.StatusConflict, "recipient_not_ready",
			"Получатель ещё не accepted. Сначала апрувните в leads.")
	case errors.Is(err, ErrAlreadyStarted):
		httpx.WriteErrMsg(w, http.StatusConflict, "already_started",
			"Проект для этого recipient'а уже создан.")
	case errors.Is(err, ErrTemplateNotFound):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "template_not_found",
			"Шаблон услуги не найден или архивный.")
	default:
		httpx.WriteErrMsg(w, http.StatusInternalServerError, "internal", "Не удалось создать проект")
	}
	return true
}

// ─── DTO для swagger / body decoder ────────────────────────────────

type AdminProjectsResponse struct {
	Items []ProjectAdminView `json:"items"`
}

type AdminEventsResponse struct {
	Items []StepEvent `json:"items"`
}

type BulkFromLeadInput struct {
	Title            string     `json:"title"`
	TemplateCode     string     `json:"template_code,omitempty"`
	TemplateVersion  int        `json:"template_version,omitempty"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id,omitempty"`
}

type BulkStartResultResponse struct {
	Created []ProjectAdminView `json:"created"`
	Skipped []uuid.UUID        `json:"skipped_specialist_ids"`
}

// StepActionInput — тело POST .../steps/.../{start,complete,skip}.
// updated_at обязателен для concurrent-edit защиты в DoD scenario E
// (но не required — без него optimistic-lock не применяется).
type StepActionInput struct {
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	Comment   string     `json:"comment,omitempty"`
}
