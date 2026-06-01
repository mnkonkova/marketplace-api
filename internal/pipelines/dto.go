package pipelines

import (
	"time"

	"github.com/google/uuid"
)

// Owner шага. Системный шаг — для будущих автоматов (не клиент и не команда).
const (
	OwnerClient = "client"
	OwnerTeam   = "team"
	OwnerSystem = "system"
)

// Pipeline — шаблон воронки. Редактирование не трогает активные проекты:
// при StartProject делается копия в project_stages/project_steps.
type Pipeline struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	Version           int       `json:"version"`
	IsActive          bool      `json:"is_active"`
	RevisionsIncluded int       `json:"revisions_included"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// PipelineFull — полное дерево для редактора. GET /admin/pipelines/{id}.
type PipelineFull struct {
	Pipeline
	Stages []StageFull `json:"stages"`
}

type Stage struct {
	ID         uuid.UUID `json:"id"`
	PipelineID uuid.UUID `json:"pipeline_id"`
	Name       string    `json:"name"`
	SortOrder  int       `json:"sort_order"`
	CreatedAt  time.Time `json:"created_at"`
}

type StageFull struct {
	Stage
	Steps []Step `json:"steps"`
}

type Step struct {
	ID                  uuid.UUID `json:"id"`
	StageID             uuid.UUID `json:"stage_id"`
	Name                string    `json:"name"`
	Owner               string    `json:"owner"`
	DurationDays        int       `json:"duration_days"`
	VisibleToClient     bool      `json:"visible_to_client"`
	VisibleToSpecialist bool      `json:"visible_to_specialist"`
	Weight              int       `json:"weight"`
	SortOrder           int       `json:"sort_order"`
	IsReview            bool      `json:"is_review"`
	CreatedAt           time.Time `json:"created_at"`
}

type CreatePipelineInput struct {
	Name              string
	Description       string
	RevisionsIncluded int
}

type PatchPipelineInput struct {
	Name              *string
	Description       *string
	RevisionsIncluded *int
	IsActive          *bool
}

type CreateStageInput struct {
	Name      string
	SortOrder int
}

type PatchStageInput struct {
	Name      *string
	SortOrder *int
}

type CreateStepInput struct {
	Name                string
	Owner               string
	DurationDays        int
	VisibleToClient     bool
	VisibleToSpecialist bool
	Weight              int
	SortOrder           int
	IsReview            bool
}

type PatchStepInput struct {
	Name                *string
	Owner               *string
	DurationDays        *int
	VisibleToClient     *bool
	VisibleToSpecialist *bool
	Weight              *int
	SortOrder           *int
	IsReview            *bool
}

// ReorderInput — массовое обновление sort_order стадий И шагов в пайплайне.
// PUT /admin/pipelines/{id}/reorder. Шаг идентифицируется stage_id+step_id,
// чтобы можно было ещё и менять привязку шага к стадии (drag между стадиями).
type ReorderInput struct {
	Stages []ReorderStage `json:"stages"`
}

type ReorderStage struct {
	ID        uuid.UUID     `json:"id"`
	SortOrder int           `json:"sort_order"`
	Steps     []ReorderStep `json:"steps"`
}

type ReorderStep struct {
	ID        uuid.UUID `json:"id"`
	SortOrder int       `json:"sort_order"`
}
