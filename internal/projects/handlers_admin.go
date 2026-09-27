package projects

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// admin-эндпоинты лежат в том же handler-receiver: канбан админа сильно
// похож на менеджерский (одинаковые view-структуры), отдельный struct
// принёс бы только дублирование.

type assignReq struct {
	// ManagerUserID может быть пустой строкой или null → unassign.
	// Не uuid.UUID напрямую, чтобы клиент мог явно прислать "" для снятия.
	ManagerUserID *string `json:"manager_user_id"`
}

// AdminAssignManager godoc
// @Summary  Назначить менеджера на проект (или снять)
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string    true "project id"
// @Param    body body assignReq true "manager_user_id (uuid или null)"
// @Success  204
// @Router   /admin/projects/{id}/assign [post]
type cancelReq struct {
	Reason string `json:"reason"`
}

// AdminCancelProject godoc
// @Summary  Отменить проект (soft-delete, обратимо через /restore)
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true  "project id"
// @Param    body body cancelReq  false "причина"
// @Success  204
// @Failure  404  {object}  errorResponse
// @Router   /admin/projects/{id} [delete]
func (h *Handler) AdminCancelProject(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var in cancelReq
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := h.svc.CancelProject(r.Context(), pid, actorID, in.Reason); err != nil {
		writeManagerErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) AdminAssignManager(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var in assignReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	var mgrID *uuid.UUID
	if in.ManagerUserID != nil {
		v := strings.TrimSpace(*in.ManagerUserID)
		if v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_manager_id",
					"manager_user_id должен быть UUID или null.")
				return
			}
			mgrID = &id
		}
	}
	if err := h.svc.AssignManager(r.Context(), pid, mgrID, actorID); err != nil {
		writeManagerErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminListProjects godoc
// @Summary  Все проекты (админ — таблица и канбан)
// @Description Поиск, фильтры, сортировка и пагинация считаются на сервере.
// @Description q — ILIKE по названию проекта и по клиенту (имя в профиле,
// @Description имя на проекте у клиента без аккаунта, почта), мин 2 символа.
// @Description manager — uuid ответственного либо "none" (без ответственного).
// @Description status — точный статус проекта либо unfinished: «в работе»,
// @Description то есть draft, active, on_hold и dispute разом. Именно это
// @Description значение даёт тот же набор, что nav_counts.projects_active в
// @Description сводке; пустой status отдаёт всё, кроме отменённых, то есть
// @Description вместе с завершёнными.
// @Description include_test=true показывает проекты, помеченные как тестовые
// @Description (по умолчанию скрыты). kind — вид проекта. sort: updated_asc |
// @Description updated_desc | created_asc | created_desc, default updated_desc.
// @Description У каждой карточки прогресс ещё и числом: progress_done /
// @Description progress_total, единица в progress_unit (publications у
// @Description креаторов, steps у продакшна).
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    q            query string false "часть названия проекта или имени клиента, мин 2 симв"
// @Param    status       query string false "точный статус проекта | unfinished (в работе: draft, active, on_hold, dispute); пусто = всё кроме cancelled, включая завершённые"
// @Param    manager      query string false "uuid менеджера | none"
// @Param    include_test query bool   false "показать тестовые проекты, default false"
// @Param    kind         query string false "creators_turnkey | production_turnkey | general"
// @Param    sort         query string false "updated_asc | updated_desc | created_asc | created_desc"
// @Param    limit        query int    false "1-1000, default 20"
// @Param    offset       query int    false "default 0"
// @Success  200 {object} AdminListResult
// @Failure  400 {object} errorResponse "bad_manager_id | invalid_input — незнакомое значение status, kind или sort; сообщение перечисляет допустимые"
// @Router   /admin/projects [get]
func (h *Handler) AdminListProjects(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, _ := strconv.Atoi(qs.Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	offset, _ := strconv.Atoi(qs.Get("offset"))
	params := AdminListParams{
		Q:      strings.TrimSpace(qs.Get("q")),
		Status: strings.TrimSpace(qs.Get("status")),
		Sort:   strings.TrimSpace(qs.Get("sort")),
		// Тестовые прячем, пока явно не попросили: вопрос «включать ли
		// мусор» должен решаться выбором, а не молчанием.
		IncludeTest: qs.Get("include_test") == "true",
		Kind:        strings.TrimSpace(qs.Get("kind")),
		Limit:       limit,
		Offset:      offset,
	}
	// "none" — единственный способ спросить «проекты без ответственного»:
	// пустой manager значит «любой», а UUID'а у «никто» не бывает.
	if m := strings.TrimSpace(qs.Get("manager")); m != "" {
		if m == "none" {
			params.Unassigned = true
		} else {
			id, err := uuid.Parse(m)
			if err != nil {
				httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_manager_id",
					"manager должен быть UUID или none.")
				return
			}
			params.ManagerID = &id
		}
	}
	res, err := h.svc.ListAll(r.Context(), params)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// AdminGetProject godoc
// @Summary  Полный вид любого проекта (админ)
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} ProjectFullView
// @Router   /admin/projects/{id} [get]
func (h *Handler) AdminGetProject(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	view, err := h.svc.GetFull(r.Context(), pid, uuid.Nil) // Nil = без assigned-фильтра
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type adminAssignSpecReq struct {
	SpecialistUserID string `json:"specialist_user_id"`
}

// AdminAssignSpecialist godoc
// @Summary  Назначить специалиста на проект (по UUID) — админ
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id  path  string             true "project id"
// @Param    body body adminAssignSpecReq true "specialist_user_id"
// @Success  204
// @Failure  400  {object} errorResponse
// @Router   /admin/projects/{id}/assign_specialist [post]
func (h *Handler) AdminAssignSpecialist(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return
	}
	var in adminAssignSpecReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	specID, err := uuid.Parse(strings.TrimSpace(in.SpecialistUserID))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_specialist_id",
			"specialist_user_id обязателен и должен быть UUID.")
		return
	}
	if err := h.svc.AssignSpecialist(r.Context(), pid, actor, specID); err != nil {
		writeManagerErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type adminCreateProjectReq struct {
	// ClientUserID — для зарегистрированного клиента. Если пусто,
	// нужно client_name + client_contact (no-account клиент).
	ClientUserID     string `json:"client_user_id,omitempty"`
	ClientName       string `json:"client_name,omitempty"`
	ClientContact    string `json:"client_contact,omitempty"`
	SpecialistUserID string `json:"specialist_user_id,omitempty"`
	AssignedToUserID string `json:"assigned_to_user_id,omitempty"`
	// PipelineID — необязателен: воронку продакшну подставляем по
	// умолчанию, а креаторам и общему проекту она не нужна вовсе.
	PipelineID string `json:"pipeline_id,omitempty"`
	// Kind — вид проекта. Пусто = production_turnkey.
	Kind string `json:"kind,omitempty"`
	// IsTest — пометить проект как тестовый: в админском списке такие
	// скрыты по умолчанию. Ставится здесь, а не выводится из данных —
	// «тест» в названии пишут не всегда.
	IsTest bool   `json:"is_test,omitempty"`
	Title  string `json:"title"`
	Budget *int   `json:"budget,omitempty"`
	Notes  string `json:"notes,omitempty"`
	Source string `json:"source,omitempty"`
}

// AdminCreateProject godoc
// @Summary  Создать проект вручную (админ)
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body adminCreateProjectReq true "project"
// @Success  201 {object} Project
// @Router   /admin/projects [post]
func (h *Handler) AdminCreateProject(w http.ResponseWriter, r *http.Request) {
	var in adminCreateProjectReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	kind := ProjectKind(in.Kind)
	if kind == "" {
		kind = KindProductionTurnkey
	} else if !IsKnownKind(kind) {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_kind",
			"Вид проекта бывает "+KindsHint()+".")
		return
	}
	var pipelineID uuid.UUID
	if s := strings.TrimSpace(in.PipelineID); s != "" {
		parsed, err := uuid.Parse(s)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_pipeline_id",
				"pipeline_id должен быть UUID.")
			return
		}
		pipelineID = parsed
	}
	startInput := StartProjectInput{
		PipelineID:    pipelineID,
		Kind:          kind,
		IsTest:        in.IsTest,
		Title:         in.Title,
		Budget:        in.Budget,
		Notes:         in.Notes,
		Source:        ProjectSource(strings.TrimSpace(in.Source)),
		ClientName:    strings.TrimSpace(in.ClientName),
		ClientContact: strings.TrimSpace(in.ClientContact),
	}
	// client_user_id опционально: если передан, парсим. Иначе сервис
	// проверит что есть client_name+client_contact.
	if s := strings.TrimSpace(in.ClientUserID); s != "" {
		clientID, err := uuid.Parse(s)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_client_id",
				"client_user_id должен быть UUID.")
			return
		}
		startInput.ClientUserID = &clientID
	}
	if in.SpecialistUserID != "" {
		sid, err := uuid.Parse(strings.TrimSpace(in.SpecialistUserID))
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_specialist_id",
				"specialist_user_id невалиден.")
			return
		}
		startInput.SpecialistUserID = &sid
	}
	if in.AssignedToUserID != "" {
		aid, err := uuid.Parse(strings.TrimSpace(in.AssignedToUserID))
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_assigned_id",
				"assigned_to_user_id невалиден.")
			return
		}
		startInput.AssignedToUserID = &aid
	}
	projectID, err := h.svc.StartProject(r.Context(), startInput)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	p, err := h.svc.repo.GetByID(r.Context(), projectID)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

// AdminMoveStage godoc
// @Summary  Админ-канбан: перенести проект на любую стадию
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path  string       true "project id"
// @Param    body body  moveStageReq true "target_stage_id"
// @Success  200  {object} Project
// @Failure  409  {object} errorResponse "stage_blocked"
// @Router   /admin/projects/{id}/move_stage [post]
func (h *Handler) AdminMoveStage(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
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
	p, err := h.svc.MoveProjectToStage(r.Context(), pid, target, actorID, in.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminMoveStep godoc
// @Summary  Админ-канбан: перенести проект на конкретный шаг
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path  string      true "project id"
// @Param    body body  moveStepReq true "target_step_id"
// @Success  200  {object} Project
// @Router   /admin/projects/{id}/move_step [post]
func (h *Handler) AdminMoveStep(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
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
	p, err := h.svc.MoveProjectToStep(r.Context(), pid, target, actorID, in.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminChangeFunnel godoc
// @Summary  Админ: сменить воронку проекта (сбросить прогресс)
// @Description Удаляет project_stages/project_steps текущего проекта и инстанциирует их из новой воронки. revisions_used обнуляется, started_at пересчитывается. Только для status=active.
// @Tags     admin-projects
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path  string             true "project id"
// @Param    body body  changeFunnelReq    true "new pipeline_id"
// @Success  200  {object} Project
// @Failure  400  {object} errorResponse    "bad_id | bad_pipeline_id | invalid_transition"
// @Failure  404  {object} errorResponse    "no_project | no_pipeline"
// @Router   /admin/projects/{id}/change_funnel [post]
func (h *Handler) AdminChangeFunnel(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var in changeFunnelReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	newPipelineID, err := uuid.Parse(in.PipelineID)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_pipeline_id", "pipeline_id должен быть UUID.")
		return
	}
	p, err := h.svc.ChangeFunnel(r.Context(), pid, newPipelineID, actorID)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminAdvanceStage godoc
// @Summary  Админ-канбан: продвинуть стадию любого проекта
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} Project
// @Failure  409 {object} errorResponse "stage_blocked | last_stage"
// @Router   /admin/projects/{id}/advance_stage [post]
func (h *Handler) AdminAdvanceStage(w http.ResponseWriter, r *http.Request) {
	actorID, _ := auth.UserIDFrom(r.Context())
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	var advIn advanceStageReq
	// Body опциональное, но битый JSON — 400 (см. decodeOptionalJSON).
	if err := decodeOptionalJSON(r, &advIn); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.AdvanceStage(r.Context(), pid, actorID, advIn.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}
