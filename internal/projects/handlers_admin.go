package projects

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// admin-эндпоинты лежат в том же handler-receiver: канбан админа сильно
// похож на менеджерский (одинаковые view-структуры), отдельный struct
// принёс бы только дублирование.

// AdminListProjects godoc
// @Summary  Все проекты (админ — таблица и канбан)
// @Tags     admin-projects
// @Produce  json
// @Security BearerAuth
// @Param    status query string false "filter by status"
// @Success  200 {object} managerListResp
// @Router   /admin/projects [get]
func (h *Handler) AdminListProjects(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	items, err := h.svc.ListAll(r.Context(), status)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, managerListResp{Items: items})
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

type adminCreateProjectReq struct {
	ClientUserID     string `json:"client_user_id"`
	SpecialistUserID string `json:"specialist_user_id,omitempty"`
	AssignedToUserID string `json:"assigned_to_user_id,omitempty"`
	PipelineID       string `json:"pipeline_id"`
	Title            string `json:"title"`
	Budget           *int   `json:"budget,omitempty"`
	Notes            string `json:"notes,omitempty"`
	Source           string `json:"source,omitempty"`
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
	clientID, err := uuid.Parse(strings.TrimSpace(in.ClientUserID))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_client_id",
			"client_user_id обязателен и должен быть UUID.")
		return
	}
	pipelineID, err := uuid.Parse(strings.TrimSpace(in.PipelineID))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_pipeline_id",
			"pipeline_id обязателен и должен быть UUID.")
		return
	}
	startInput := StartProjectInput{
		ClientUserID: clientID,
		PipelineID:   pipelineID,
		Title:        in.Title,
		Budget:       in.Budget,
		Notes:        in.Notes,
		Source:       ProjectSource(strings.TrimSpace(in.Source)),
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
	_ = json.NewDecoder(r.Body).Decode(&advIn)
	p, err := h.svc.AdvanceStage(r.Context(), pid, actorID, advIn.UpdatedAt)
	if err != nil {
		writeManagerErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}
