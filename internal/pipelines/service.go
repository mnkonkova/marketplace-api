package pipelines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidInput = errors.New("invalid input")

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// ---- Pipeline ----

func (s *Service) CreatePipeline(ctx context.Context, in CreatePipelineInput) (Pipeline, error) {
	if err := validateName(in.Name); err != nil {
		return Pipeline{}, err
	}
	if in.RevisionsIncluded < 0 || in.RevisionsIncluded > 50 {
		return Pipeline{}, fmt.Errorf("%w: revisions_included must be 0..50", ErrInvalidInput)
	}
	return s.repo.CreatePipeline(ctx, in)
}

func (s *Service) ListPipelines(ctx context.Context) ([]Pipeline, error) {
	return s.repo.ListPipelines(ctx)
}

func (s *Service) GetPipelineFull(ctx context.Context, id uuid.UUID) (PipelineFull, error) {
	return s.repo.GetPipelineFull(ctx, id)
}

func (s *Service) PatchPipeline(ctx context.Context, id uuid.UUID, in PatchPipelineInput) (Pipeline, error) {
	if in.Name != nil {
		if err := validateName(*in.Name); err != nil {
			return Pipeline{}, err
		}
	}
	if in.RevisionsIncluded != nil && (*in.RevisionsIncluded < 0 || *in.RevisionsIncluded > 50) {
		return Pipeline{}, fmt.Errorf("%w: revisions_included must be 0..50", ErrInvalidInput)
	}
	return s.repo.PatchPipeline(ctx, id, in)
}

func (s *Service) DeletePipeline(ctx context.Context, id uuid.UUID) error {
	return s.repo.DeletePipeline(ctx, id)
}

// ---- Stage ----

func (s *Service) CreateStage(ctx context.Context, pipelineID uuid.UUID, in CreateStageInput) (Stage, error) {
	if err := validateName(in.Name); err != nil {
		return Stage{}, err
	}
	if in.SortOrder < 0 {
		return Stage{}, fmt.Errorf("%w: sort_order must be >= 0", ErrInvalidInput)
	}
	if _, err := s.repo.GetPipeline(ctx, pipelineID); err != nil {
		return Stage{}, err
	}
	return s.repo.CreateStage(ctx, pipelineID, in)
}

func (s *Service) PatchStage(ctx context.Context, id uuid.UUID, in PatchStageInput) (Stage, error) {
	if in.Name != nil {
		if err := validateName(*in.Name); err != nil {
			return Stage{}, err
		}
	}
	if in.SortOrder != nil && *in.SortOrder < 0 {
		return Stage{}, fmt.Errorf("%w: sort_order must be >= 0", ErrInvalidInput)
	}
	return s.repo.PatchStage(ctx, id, in)
}

func (s *Service) DeleteStage(ctx context.Context, id uuid.UUID) error {
	return s.repo.DeleteStage(ctx, id)
}

// ---- Step ----

func (s *Service) CreateStep(ctx context.Context, stageID uuid.UUID, in CreateStepInput) (Step, error) {
	if err := validateStep(in.Name, in.Owner, in.DurationDays, in.Weight, in.SortOrder); err != nil {
		return Step{}, err
	}
	if _, err := s.repo.GetStage(ctx, stageID); err != nil {
		return Step{}, err
	}
	return s.repo.CreateStep(ctx, stageID, in)
}

func (s *Service) PatchStep(ctx context.Context, id uuid.UUID, in PatchStepInput) (Step, error) {
	if in.Name != nil {
		if err := validateName(*in.Name); err != nil {
			return Step{}, err
		}
	}
	if in.Owner != nil {
		if err := validateOwner(*in.Owner); err != nil {
			return Step{}, err
		}
	}
	if in.DurationDays != nil && *in.DurationDays <= 0 {
		return Step{}, fmt.Errorf("%w: duration_days must be > 0", ErrInvalidInput)
	}
	if in.Weight != nil && *in.Weight <= 0 {
		return Step{}, fmt.Errorf("%w: weight must be > 0", ErrInvalidInput)
	}
	if in.SortOrder != nil && *in.SortOrder < 0 {
		return Step{}, fmt.Errorf("%w: sort_order must be >= 0", ErrInvalidInput)
	}
	return s.repo.PatchStep(ctx, id, in)
}

func (s *Service) DeleteStep(ctx context.Context, id uuid.UUID) error {
	return s.repo.DeleteStep(ctx, id)
}

// Reorder — пробрасываем как есть; репо сам атомарен.
func (s *Service) Reorder(ctx context.Context, pipelineID uuid.UUID, in ReorderInput) error {
	if _, err := s.repo.GetPipeline(ctx, pipelineID); err != nil {
		return err
	}
	for _, st := range in.Stages {
		if st.ID == uuid.Nil {
			return fmt.Errorf("%w: stage id is required", ErrInvalidInput)
		}
		if st.SortOrder < 0 {
			return fmt.Errorf("%w: stage sort_order must be >= 0", ErrInvalidInput)
		}
		for _, sp := range st.Steps {
			if sp.ID == uuid.Nil {
				return fmt.Errorf("%w: step id is required", ErrInvalidInput)
			}
			if sp.SortOrder < 0 {
				return fmt.Errorf("%w: step sort_order must be >= 0", ErrInvalidInput)
			}
		}
	}
	return s.repo.Reorder(ctx, pipelineID, in)
}

// ---- helpers (экспортированы для юнит-тестов) ----

func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	case utf8.RuneCountInString(name) > 200:
		return fmt.Errorf("%w: name must be <= 200 chars", ErrInvalidInput)
	}
	return nil
}

func ValidateOwner(owner string) error {
	switch owner {
	case OwnerClient, OwnerTeam, OwnerSystem:
		return nil
	default:
		return fmt.Errorf("%w: owner must be client|team|system", ErrInvalidInput)
	}
}

func ValidateStep(name, owner string, duration, weight, sortOrder int) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := ValidateOwner(owner); err != nil {
		return err
	}
	if duration <= 0 {
		return fmt.Errorf("%w: duration_days must be > 0", ErrInvalidInput)
	}
	if weight <= 0 {
		return fmt.Errorf("%w: weight must be > 0", ErrInvalidInput)
	}
	if sortOrder < 0 {
		return fmt.Errorf("%w: sort_order must be >= 0", ErrInvalidInput)
	}
	return nil
}

func validateName(name string) error                                 { return ValidateName(name) }
func validateOwner(owner string) error                               { return ValidateOwner(owner) }
func validateStep(n, o string, d, w, so int) error                   { return ValidateStep(n, o, d, w, so) }
