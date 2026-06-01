package integration_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/pipelines"
)

// newPipelinesRepo / *Create-хелперы вынесены чтобы не дублироваться
// между тестами projects_*. Все DTO с минимально валидными полями.

func newPipelinesRepo(t *testing.T, pool *pgxpool.Pool) *pipelines.Repo {
	t.Helper()
	return pipelines.NewRepo(pool)
}

func pipelinesCreate(name string) pipelines.CreatePipelineInput {
	return pipelines.CreatePipelineInput{
		Name:              name,
		Description:       "integration test",
		RevisionsIncluded: 2,
	}
}

func pipelinesStage(name string, order int) pipelines.CreateStageInput {
	return pipelines.CreateStageInput{Name: name, SortOrder: order}
}

func pipelinesStep(name, owner string, order int) pipelines.CreateStepInput {
	return pipelines.CreateStepInput{
		Name:                name,
		Owner:               owner,
		DurationDays:        1,
		VisibleToClient:     true,
		VisibleToSpecialist: true,
		Weight:              1,
		SortOrder:           order,
		IsReview:            false,
	}
}
