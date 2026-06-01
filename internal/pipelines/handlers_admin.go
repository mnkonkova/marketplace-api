package pipelines

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func parseID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id в URL.")
		return uuid.Nil, false
	}
	return id, true
}

func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Объект не найден.")
	case errors.Is(err, ErrHasActiveProjects):
		httpx.WriteErrMsg(w, http.StatusConflict, "has_active_projects",
			"У пайплайна есть активные проекты — деактивация запрещена.")
	default:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// ---- Pipeline ----

// AdminListPipelines godoc
// @Summary List pipelines (admin)
// @Tags    admin-pipelines
// @Produce json
// @Security BearerAuth
// @Success 200 {object} pipelineListResp
// @Router  /admin/pipelines [get]
func (h *Handler) AdminListPipelines(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListPipelines(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pipelineListResp{Items: items})
}

// AdminGetPipeline godoc
// @Summary Get pipeline with stages and steps (admin)
// @Tags    admin-pipelines
// @Produce json
// @Security BearerAuth
// @Param   id path string true "pipeline id"
// @Success 200 {object} PipelineFull
// @Failure 404 {object} errorResponse
// @Router  /admin/pipelines/{id} [get]
func (h *Handler) AdminGetPipeline(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	full, err := h.svc.GetPipelineFull(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, full)
}

type createPipelineReq struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	RevisionsIncluded int    `json:"revisions_included"`
}

// AdminCreatePipeline godoc
// @Summary Create pipeline (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   body body     createPipelineReq true "pipeline"
// @Success 201  {object} Pipeline
// @Failure 400  {object} errorResponse
// @Router  /admin/pipelines [post]
func (h *Handler) AdminCreatePipeline(w http.ResponseWriter, r *http.Request) {
	var in createPipelineReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.CreatePipeline(r.Context(), CreatePipelineInput{
		Name: in.Name, Description: in.Description, RevisionsIncluded: in.RevisionsIncluded,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

type patchPipelineReq struct {
	Name              *string `json:"name,omitempty"`
	Description       *string `json:"description,omitempty"`
	RevisionsIncluded *int    `json:"revisions_included,omitempty"`
	IsActive          *bool   `json:"is_active,omitempty"`
}

// AdminPatchPipeline godoc
// @Summary Patch pipeline (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   id   path     string           true "pipeline id"
// @Param   body body     patchPipelineReq true "patch fields"
// @Success 200  {object} Pipeline
// @Router  /admin/pipelines/{id} [patch]
func (h *Handler) AdminPatchPipeline(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in patchPipelineReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.PatchPipeline(r.Context(), id, PatchPipelineInput{
		Name: in.Name, Description: in.Description,
		RevisionsIncluded: in.RevisionsIncluded, IsActive: in.IsActive,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// AdminMakeDefault godoc
// @Summary Сделать воронку дефолтной (для брифов)
// @Tags    admin-pipelines
// @Produce json
// @Security BearerAuth
// @Param   id path string true "pipeline id"
// @Success 204
// @Router  /admin/pipelines/{id}/make_default [post]
func (h *Handler) AdminMakeDefault(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.MakeDefault(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminDeletePipeline godoc
// @Summary Soft- или hard-delete воронки (admin)
// @Description hard=true → физическое удаление из БД (cascade на stages/steps).
// @Description Запрещено если есть любые проекты с этим pipeline_id.
// @Description Без флага — soft-delete (is_active=false), запрет только при
// @Description активных проектах (draft/active/on_hold/dispute).
// @Tags    admin-pipelines
// @Produce json
// @Security BearerAuth
// @Param   id   path  string true "pipeline id"
// @Param   hard query bool   false "true для hard-delete"
// @Success 204
// @Failure 409 {object} errorResponse "has_active_projects | has_projects"
// @Router  /admin/pipelines/{id} [delete]
func (h *Handler) AdminDeletePipeline(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	hard := r.URL.Query().Get("hard") == "true"
	var err error
	if hard {
		err = h.svc.HardDeletePipeline(r.Context(), id)
	} else {
		err = h.svc.DeletePipeline(r.Context(), id)
	}
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Stage ----

type createStageReq struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

// AdminCreateStage godoc
// @Summary Create stage under pipeline (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   id   path     string         true "pipeline id"
// @Param   body body     createStageReq true "stage"
// @Success 201  {object} Stage
// @Router  /admin/pipelines/{id}/stages [post]
func (h *Handler) AdminCreateStage(w http.ResponseWriter, r *http.Request) {
	pipelineID, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in createStageReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	s, err := h.svc.CreateStage(r.Context(), pipelineID,
		CreateStageInput{Name: in.Name, SortOrder: in.SortOrder})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, s)
}

type patchStageReq struct {
	Name      *string `json:"name,omitempty"`
	SortOrder *int    `json:"sort_order,omitempty"`
}

// AdminPatchStage godoc
// @Summary Patch stage (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   id   path     string        true "stage id"
// @Param   body body     patchStageReq true "patch"
// @Success 200  {object} Stage
// @Router  /admin/pipelines/stages/{id} [patch]
func (h *Handler) AdminPatchStage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in patchStageReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	s, err := h.svc.PatchStage(r.Context(), id, PatchStageInput{Name: in.Name, SortOrder: in.SortOrder})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s)
}

// AdminDeleteStage godoc
// @Summary Delete stage (admin)
// @Tags    admin-pipelines
// @Produce json
// @Security BearerAuth
// @Param   id path string true "stage id"
// @Success 204
// @Router  /admin/pipelines/stages/{id} [delete]
func (h *Handler) AdminDeleteStage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.DeleteStage(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Step ----

type createStepReq struct {
	Name                string `json:"name"`
	Owner               string `json:"owner"`
	DurationDays        int    `json:"duration_days"`
	VisibleToClient     bool   `json:"visible_to_client"`
	VisibleToSpecialist bool   `json:"visible_to_specialist"`
	Weight              int    `json:"weight"`
	SortOrder           int    `json:"sort_order"`
	IsReview            bool   `json:"is_review"`
}

// AdminCreateStep godoc
// @Summary Create step under stage (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   id   path     string        true "stage id"
// @Param   body body     createStepReq true "step"
// @Success 201  {object} Step
// @Router  /admin/pipelines/stages/{id}/steps [post]
func (h *Handler) AdminCreateStep(w http.ResponseWriter, r *http.Request) {
	stageID, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in createStepReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	st, err := h.svc.CreateStep(r.Context(), stageID, CreateStepInput{
		Name:                in.Name,
		Owner:               in.Owner,
		DurationDays:        in.DurationDays,
		VisibleToClient:     in.VisibleToClient,
		VisibleToSpecialist: in.VisibleToSpecialist,
		Weight:              in.Weight,
		SortOrder:           in.SortOrder,
		IsReview:            in.IsReview,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, st)
}

type patchStepReq struct {
	Name                *string `json:"name,omitempty"`
	Owner               *string `json:"owner,omitempty"`
	DurationDays        *int    `json:"duration_days,omitempty"`
	VisibleToClient     *bool   `json:"visible_to_client,omitempty"`
	VisibleToSpecialist *bool   `json:"visible_to_specialist,omitempty"`
	Weight              *int    `json:"weight,omitempty"`
	SortOrder           *int    `json:"sort_order,omitempty"`
	IsReview            *bool   `json:"is_review,omitempty"`
}

// AdminPatchStep godoc
// @Summary Patch step (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   id   path     string       true "step id"
// @Param   body body     patchStepReq true "patch"
// @Success 200  {object} Step
// @Router  /admin/pipelines/steps/{id} [patch]
func (h *Handler) AdminPatchStep(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in patchStepReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	st, err := h.svc.PatchStep(r.Context(), id, PatchStepInput{
		Name: in.Name, Owner: in.Owner, DurationDays: in.DurationDays,
		VisibleToClient: in.VisibleToClient, VisibleToSpecialist: in.VisibleToSpecialist,
		Weight: in.Weight, SortOrder: in.SortOrder, IsReview: in.IsReview,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

// AdminDeleteStep godoc
// @Summary Delete step (admin)
// @Tags    admin-pipelines
// @Produce json
// @Security BearerAuth
// @Param   id path string true "step id"
// @Success 204
// @Router  /admin/pipelines/steps/{id} [delete]
func (h *Handler) AdminDeleteStep(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.DeleteStep(r.Context(), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminReorder godoc
// @Summary Bulk reorder stages and steps within pipeline (admin)
// @Tags    admin-pipelines
// @Accept  json
// @Produce json
// @Security BearerAuth
// @Param   id   path     string       true "pipeline id"
// @Param   body body     ReorderInput true "reorder payload"
// @Success 204
// @Router  /admin/pipelines/{id}/reorder [put]
func (h *Handler) AdminReorder(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	var in ReorderInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	if err := h.svc.Reorder(r.Context(), id, in); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// типы для swaggo
type pipelineListResp struct {
	Items []Pipeline `json:"items"`
}

type errorResponse struct {
	Error string `json:"error"`
}
