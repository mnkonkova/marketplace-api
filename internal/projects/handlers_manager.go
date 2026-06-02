package projects

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

func managerFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
		return uuid.Nil, false
	}
	return uid, true
}

// effectiveOwnerID — для менеджера возвращает его user_id (assigned_to фильтр
// применяется), для админа — uuid.Nil (без фильтра, доступ ко всем проектам).
// Используется в handler-ах /manager/projects/* чтобы админу не выдавать 404
// на проекты, где он не assigned_to.
func effectiveOwnerID(r *http.Request, uid uuid.UUID) uuid.UUID {
	if role, _ := auth.RoleFrom(r.Context()); role == auth.RoleAdmin {
		return uuid.Nil
	}
	return uid
}

func writeManagerErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrStepNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект или шаг не найден.")
	case errors.Is(err, ErrAlreadyClaimed):
		httpx.WriteErrMsg(w, http.StatusConflict, "already_claimed",
			"Проект уже взят другим менеджером.")
	case errors.Is(err, ErrStageBlocked):
		httpx.WriteErrMsg(w, http.StatusConflict, "stage_blocked",
			"Нельзя продвинуть стадию: ожидается действие клиента.")
	case errors.Is(err, ErrLastStage):
		httpx.WriteErrMsg(w, http.StatusConflict, "last_stage",
			"Проект уже на последней стадии.")
	case errors.Is(err, ErrConflict):
		httpx.WriteErrMsg(w, http.StatusConflict, "stale_updated_at",
			"Проект был обновлён другим запросом.")
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteErrMsg(w, http.StatusConflict, "invalid_transition",
			"Шаг уже в другом статусе.")
	case errors.Is(err, ErrNoProposedSpecialist):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "no_proposed_specialist",
			"Клиент не выбрал исполнителя — нечего подтверждать.")
	default:
		// Логируем всё, что не маппится в доменную ошибку: иначе 500 уходит
		// клиенту молча и его невозможно расследовать.
		slog.Error("projects: unhandled error", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// ManagerApproveSpecialist godoc
// @Summary  Подтвердить выбранного клиентом исполнителя
// @Tags     manager-projects
// @Produce  json
// @Security BearerAuth
// @Param    id  path  string  true  "project id"
// @Success  200  {object}  approveResp
// @Failure  400  {object}  errorResponse "no_proposed_specialist"
// @Router   /manager/projects/{id}/approve_specialist [post]
func (h *Handler) ManagerApproveSpecialist(w http.ResponseWriter, r *http.Request) {
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
	specID, err := h.svc.ApproveProposedSpecialist(r.Context(), pid, uid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, approveResp{SpecialistUserID: specID})
}

type approveResp struct {
	SpecialistUserID uuid.UUID `json:"specialist_user_id"`
}

type rejectSpecReq struct {
	Reason string `json:"reason"`
}

// ManagerRejectSpecialist godoc
// @Summary  Отклонить предложенного клиентом исполнителя
// @Tags     manager-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id  path  string  true  "project id"
// @Param    body body rejectSpecReq false "причина"
// @Success  204
// @Router   /manager/projects/{id}/reject_specialist [post]
func (h *Handler) ManagerRejectSpecialist(w http.ResponseWriter, r *http.Request) {
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
	var in rejectSpecReq
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := h.svc.RejectProposedSpecialist(r.Context(), pid, uid, in.Reason); err != nil {
		writeManagerErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ManagerInbox godoc
// @Summary      Входящие проекты без ответственного
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  managerListResp
// @Router       /manager/projects/inbox [get]
func (h *Handler) ManagerInbox(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListInbox(r.Context())
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managerListResp{Items: items})
}

// ManagerListAssigned godoc
// @Summary      Мои проекты (assigned_to=me) — для канбана
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  managerListResp
// @Router       /manager/projects [get]
func (h *Handler) ManagerListAssigned(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListAssignedTo(r.Context(), uid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managerListResp{Items: items})
}

// ManagerClaim godoc
// @Summary      Взять проект на себя
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "project id"
// @Success      204
// @Failure      404  {object}  errorResponse
// @Failure      409  {object}  errorResponse  "already_claimed"
// @Router       /manager/projects/{id}/claim [post]
func (h *Handler) ManagerClaim(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	if err := h.svc.Claim(r.Context(), pid, uid); err != nil {
		writeManagerErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ManagerGetFull godoc
// @Summary      Полный вид моего проекта (все стадии и шаги)
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "project id"
// @Success      200  {object}  ProjectFullView
// @Router       /manager/projects/{id} [get]
func (h *Handler) ManagerGetFull(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	view, err := h.svc.GetFull(r.Context(), pid, effectiveOwnerID(r, uid))
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type moveStageReq struct {
	TargetStageID string     `json:"target_stage_id"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

type moveStepReq struct {
	TargetStepID string     `json:"target_step_id"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

// ManagerMoveStep godoc
// @Summary  Перенести проект на конкретный шаг (для канбана по шагам)
// @Tags     manager-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path  string      true "project id"
// @Param    body body  moveStepReq true "target_step_id"
// @Success  200  {object} Project
// @Failure  409  {object} errorResponse "stage_blocked"
// @Router   /manager/projects/{id}/move_step [post]
func (h *Handler) ManagerMoveStep(w http.ResponseWriter, r *http.Request) {
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
	var in moveStepReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	target, err := uuid.Parse(in.TargetStepID)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_step_id", "target_step_id должен быть UUID.")
		return
	}
	p, err := h.svc.MoveProjectToStep(r.Context(), pid, target, uid, in.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

type advanceStageReq struct {
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// ManagerMoveStage godoc
// @Summary  Перенести проект на произвольную стадию (любую)
// @Tags     manager-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path  string         true "project id"
// @Param    body body  moveStageReq   true "target_stage_id"
// @Success  200  {object} Project
// @Failure  409  {object} errorResponse "stage_blocked | not_found"
// @Router   /manager/projects/{id}/move_stage [post]
func (h *Handler) ManagerMoveStage(w http.ResponseWriter, r *http.Request) {
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
	var in moveStageReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	target, err := uuid.Parse(in.TargetStageID)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_stage_id", "target_stage_id должен быть UUID.")
		return
	}
	p, err := h.svc.MoveProjectToStage(r.Context(), pid, target, uid, in.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ManagerAdvanceStage godoc
// @Summary      Продвинуть проект на следующую стадию
// @Description  Канбан-drag: завершает оставшиеся team-шаги текущей стадии,
// @Description  активирует первый шаг следующей. 409 если есть незавершённый
// @Description  client-шаг.
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "project id"
// @Success      200  {object}  Project
// @Failure      409  {object}  errorResponse  "stage_blocked | last_stage"
// @Router       /manager/projects/{id}/advance_stage [post]
func (h *Handler) ManagerAdvanceStage(w http.ResponseWriter, r *http.Request) {
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
	var in advanceStageReq
	// Тело опциональное — optimistic-lock через updated_at тоже опционально.
	_ = json.NewDecoder(r.Body).Decode(&in)
	p, err := h.svc.AdvanceStage(r.Context(), pid, uid, in.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ManagerStartStep godoc
// @Summary      Стартовать pending-шаг (менеджер)
// @Description  owner=team/system → in_progress; owner=client → waiting_client.
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id       path  string  true  "project id"
// @Param        step_id  path  string  true  "step id"
// @Success      200  {object}  Step
// @Router       /manager/projects/{id}/steps/{step_id}/start [post]
func (h *Handler) ManagerStartStep(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, sid, ok := parseProjectStep(w, r)
	if !ok {
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	step, err := h.svc.StartStep(r.Context(), pid, sid, uid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, step)
}

// ManagerCompleteStep godoc
// @Summary      Завершить team-шаг
// @Tags         manager-projects
// @Produce      json
// @Security     BearerAuth
// @Param        id       path  string  true  "project id"
// @Param        step_id  path  string  true  "step id"
// @Success      200  {object}  Step
// @Router       /manager/projects/{id}/steps/{step_id}/complete [post]
func (h *Handler) ManagerCompleteStep(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, sid, ok := parseProjectStep(w, r)
	if !ok {
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	step, err := h.svc.CompleteStep(r.Context(), pid, sid, uid)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, step)
}

type skipReq struct {
	Comment string `json:"comment"`
}

// ManagerSkipStep godoc
// @Summary      Пропустить шаг (с комментарием)
// @Tags         manager-projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id       path  string   true  "project id"
// @Param        step_id  path  string   true  "step id"
// @Param        body     body  skipReq  true  "comment (required)"
// @Success      200  {object}  Step
// @Router       /manager/projects/{id}/steps/{step_id}/skip [post]
func (h *Handler) ManagerSkipStep(w http.ResponseWriter, r *http.Request) {
	uid, ok := managerFrom(w, r)
	if !ok {
		return
	}
	pid, sid, ok := parseProjectStep(w, r)
	if !ok {
		return
	}
	if err := h.svc.AssertManagerHasAccess(r.Context(), pid, effectiveOwnerID(r, uid)); err != nil {
		writeManagerErr(w, err)
		return
	}
	var in skipReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	step, err := h.svc.SkipStep(r.Context(), pid, sid, uid, in.Comment)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, step)
}

// ManagerPatch godoc
// @Summary      Inline-редактирование title/budget/notes
// @Tags         manager-projects
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path  string             true   "project id"
// @Param        body  body  ManagerPatchInput  true   "patch"
// @Success      200   {object}  Project
// @Failure      409   {object}  errorResponse  "stale_updated_at"
// @Router       /manager/projects/{id} [patch]
func (h *Handler) ManagerPatch(w http.ResponseWriter, r *http.Request) {
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
	var in ManagerPatchInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.PatchProject(r.Context(), pid, in)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// типы для swaggo
type managerListResp struct {
	Items []ProjectManagerView `json:"items"`
}
