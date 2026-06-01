package pipelines

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("pipeline entity not found")
	ErrHasActiveProjects = errors.New("pipeline has active projects")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// ---- Pipeline ----

func (r *Repo) CreatePipeline(ctx context.Context, in CreatePipelineInput) (Pipeline, error) {
	var p Pipeline
	err := r.db.QueryRow(ctx,
		`INSERT INTO pipelines (name, description, revisions_included)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, description, version, is_active, revisions_included, created_at, updated_at`,
		strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), in.RevisionsIncluded).
		Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.IsActive, &p.RevisionsIncluded, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Pipeline{}, fmt.Errorf("insert pipeline: %w", err)
	}
	return p, nil
}

// ListPipelines — для админ-обзора. Возвращает все, в т.ч. неактивные.
func (r *Repo) ListPipelines(ctx context.Context) ([]Pipeline, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, name, description, version, is_active, revisions_included, created_at, updated_at
		 FROM pipelines ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list pipelines: %w", err)
	}
	defer rows.Close()
	out := make([]Pipeline, 0)
	for rows.Next() {
		var p Pipeline
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.IsActive, &p.RevisionsIncluded, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan pipeline: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) GetPipeline(ctx context.Context, id uuid.UUID) (Pipeline, error) {
	var p Pipeline
	err := r.db.QueryRow(ctx,
		`SELECT id, name, description, version, is_active, revisions_included, created_at, updated_at
		 FROM pipelines WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.IsActive, &p.RevisionsIncluded, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pipeline{}, ErrNotFound
	}
	if err != nil {
		return Pipeline{}, fmt.Errorf("get pipeline: %w", err)
	}
	return p, nil
}

// GetPipelineFull — загрузить пайплайн с вложенными стадиями и шагами.
// Три запроса (pipeline / stages-by-pipeline / steps-by-stages), собираем в Go.
// Сортируем строго по sort_order — это контракт для редактора.
func (r *Repo) GetPipelineFull(ctx context.Context, id uuid.UUID) (PipelineFull, error) {
	p, err := r.GetPipeline(ctx, id)
	if err != nil {
		return PipelineFull{}, err
	}

	stageRows, err := r.db.Query(ctx,
		`SELECT id, pipeline_id, name, sort_order, created_at
		 FROM pipeline_stages WHERE pipeline_id = $1 ORDER BY sort_order, created_at`,
		id)
	if err != nil {
		return PipelineFull{}, fmt.Errorf("list stages: %w", err)
	}
	stages := make([]StageFull, 0)
	stageIdx := map[uuid.UUID]int{}
	for stageRows.Next() {
		var s Stage
		if err := stageRows.Scan(&s.ID, &s.PipelineID, &s.Name, &s.SortOrder, &s.CreatedAt); err != nil {
			stageRows.Close()
			return PipelineFull{}, fmt.Errorf("scan stage: %w", err)
		}
		stageIdx[s.ID] = len(stages)
		stages = append(stages, StageFull{Stage: s, Steps: []Step{}})
	}
	stageRows.Close()
	if err := stageRows.Err(); err != nil {
		return PipelineFull{}, fmt.Errorf("iter stages: %w", err)
	}

	if len(stages) > 0 {
		stageIDs := make([]uuid.UUID, 0, len(stages))
		for _, s := range stages {
			stageIDs = append(stageIDs, s.ID)
		}
		stepRows, err := r.db.Query(ctx,
			`SELECT id, stage_id, name, owner, duration_days,
			        visible_to_client, visible_to_specialist, weight, sort_order, is_review, created_at
			 FROM pipeline_steps WHERE stage_id = ANY($1) ORDER BY sort_order, created_at`,
			stageIDs)
		if err != nil {
			return PipelineFull{}, fmt.Errorf("list steps: %w", err)
		}
		defer stepRows.Close()
		for stepRows.Next() {
			var st Step
			if err := stepRows.Scan(&st.ID, &st.StageID, &st.Name, &st.Owner, &st.DurationDays,
				&st.VisibleToClient, &st.VisibleToSpecialist, &st.Weight, &st.SortOrder, &st.IsReview, &st.CreatedAt); err != nil {
				return PipelineFull{}, fmt.Errorf("scan step: %w", err)
			}
			idx, ok := stageIdx[st.StageID]
			if !ok {
				continue
			}
			stages[idx].Steps = append(stages[idx].Steps, st)
		}
		if err := stepRows.Err(); err != nil {
			return PipelineFull{}, fmt.Errorf("iter steps: %w", err)
		}
	}

	return PipelineFull{Pipeline: p, Stages: stages}, nil
}

func (r *Repo) PatchPipeline(ctx context.Context, id uuid.UUID, in PatchPipelineInput) (Pipeline, error) {
	sets := make([]string, 0, 4)
	args := []any{id}
	if in.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)+1))
		args = append(args, strings.TrimSpace(*in.Name))
	}
	if in.Description != nil {
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)+1))
		args = append(args, strings.TrimSpace(*in.Description))
	}
	if in.RevisionsIncluded != nil {
		sets = append(sets, fmt.Sprintf("revisions_included = $%d", len(args)+1))
		args = append(args, *in.RevisionsIncluded)
	}
	if in.IsActive != nil {
		sets = append(sets, fmt.Sprintf("is_active = $%d", len(args)+1))
		args = append(args, *in.IsActive)
	}
	if len(sets) == 0 {
		return r.GetPipeline(ctx, id)
	}
	q := fmt.Sprintf(
		`UPDATE pipelines SET %s, updated_at = now() WHERE id = $1
		 RETURNING id, name, description, version, is_active, revisions_included, created_at, updated_at`,
		strings.Join(sets, ", "))
	var p Pipeline
	err := r.db.QueryRow(ctx, q, args...).
		Scan(&p.ID, &p.Name, &p.Description, &p.Version, &p.IsActive, &p.RevisionsIncluded, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pipeline{}, ErrNotFound
	}
	if err != nil {
		return Pipeline{}, fmt.Errorf("patch pipeline: %w", err)
	}
	return p, nil
}

// DeletePipeline — мягко: is_active=false. Жёстко не удаляем из-за FK
// projects.pipeline_id (проекты могут уже существовать). Если есть активные
// проекты — возвращаем ErrHasActiveProjects, чтобы UI показал понятный отказ.
func (r *Repo) DeletePipeline(ctx context.Context, id uuid.UUID) error {
	var hasActive bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM projects WHERE pipeline_id = $1 AND status IN ('draft','active','on_hold','dispute'))`,
		id).Scan(&hasActive); err != nil {
		return fmt.Errorf("check active projects: %w", err)
	}
	if hasActive {
		return ErrHasActiveProjects
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE pipelines SET is_active = FALSE, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete pipeline: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Stage ----

func (r *Repo) CreateStage(ctx context.Context, pipelineID uuid.UUID, in CreateStageInput) (Stage, error) {
	var s Stage
	err := r.db.QueryRow(ctx,
		`INSERT INTO pipeline_stages (pipeline_id, name, sort_order) VALUES ($1, $2, $3)
		 RETURNING id, pipeline_id, name, sort_order, created_at`,
		pipelineID, strings.TrimSpace(in.Name), in.SortOrder).
		Scan(&s.ID, &s.PipelineID, &s.Name, &s.SortOrder, &s.CreatedAt)
	if err != nil {
		return Stage{}, fmt.Errorf("insert stage: %w", err)
	}
	return s, nil
}

func (r *Repo) GetStage(ctx context.Context, id uuid.UUID) (Stage, error) {
	var s Stage
	err := r.db.QueryRow(ctx,
		`SELECT id, pipeline_id, name, sort_order, created_at FROM pipeline_stages WHERE id = $1`,
		id).Scan(&s.ID, &s.PipelineID, &s.Name, &s.SortOrder, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stage{}, ErrNotFound
	}
	if err != nil {
		return Stage{}, fmt.Errorf("get stage: %w", err)
	}
	return s, nil
}

func (r *Repo) PatchStage(ctx context.Context, id uuid.UUID, in PatchStageInput) (Stage, error) {
	sets := make([]string, 0, 2)
	args := []any{id}
	if in.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)+1))
		args = append(args, strings.TrimSpace(*in.Name))
	}
	if in.SortOrder != nil {
		sets = append(sets, fmt.Sprintf("sort_order = $%d", len(args)+1))
		args = append(args, *in.SortOrder)
	}
	if len(sets) == 0 {
		return r.GetStage(ctx, id)
	}
	q := fmt.Sprintf(
		`UPDATE pipeline_stages SET %s WHERE id = $1
		 RETURNING id, pipeline_id, name, sort_order, created_at`,
		strings.Join(sets, ", "))
	var s Stage
	err := r.db.QueryRow(ctx, q, args...).Scan(&s.ID, &s.PipelineID, &s.Name, &s.SortOrder, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stage{}, ErrNotFound
	}
	if err != nil {
		return Stage{}, fmt.Errorf("patch stage: %w", err)
	}
	return s, nil
}

func (r *Repo) DeleteStage(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM pipeline_stages WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete stage: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Step ----

func (r *Repo) CreateStep(ctx context.Context, stageID uuid.UUID, in CreateStepInput) (Step, error) {
	var st Step
	err := r.db.QueryRow(ctx,
		`INSERT INTO pipeline_steps
		   (stage_id, name, owner, duration_days, visible_to_client, visible_to_specialist,
		    weight, sort_order, is_review)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 RETURNING id, stage_id, name, owner, duration_days, visible_to_client, visible_to_specialist,
		           weight, sort_order, is_review, created_at`,
		stageID, strings.TrimSpace(in.Name), in.Owner, in.DurationDays,
		in.VisibleToClient, in.VisibleToSpecialist, in.Weight, in.SortOrder, in.IsReview).
		Scan(&st.ID, &st.StageID, &st.Name, &st.Owner, &st.DurationDays,
			&st.VisibleToClient, &st.VisibleToSpecialist, &st.Weight, &st.SortOrder, &st.IsReview, &st.CreatedAt)
	if err != nil {
		return Step{}, fmt.Errorf("insert step: %w", err)
	}
	return st, nil
}

func (r *Repo) GetStep(ctx context.Context, id uuid.UUID) (Step, error) {
	var st Step
	err := r.db.QueryRow(ctx,
		`SELECT id, stage_id, name, owner, duration_days, visible_to_client, visible_to_specialist,
		        weight, sort_order, is_review, created_at
		 FROM pipeline_steps WHERE id = $1`, id).
		Scan(&st.ID, &st.StageID, &st.Name, &st.Owner, &st.DurationDays,
			&st.VisibleToClient, &st.VisibleToSpecialist, &st.Weight, &st.SortOrder, &st.IsReview, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, fmt.Errorf("get step: %w", err)
	}
	return st, nil
}

func (r *Repo) PatchStep(ctx context.Context, id uuid.UUID, in PatchStepInput) (Step, error) {
	sets := make([]string, 0, 8)
	args := []any{id}
	add := func(col string, val any) {
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)+1))
		args = append(args, val)
	}
	if in.Name != nil {
		add("name", strings.TrimSpace(*in.Name))
	}
	if in.Owner != nil {
		add("owner", *in.Owner)
	}
	if in.DurationDays != nil {
		add("duration_days", *in.DurationDays)
	}
	if in.VisibleToClient != nil {
		add("visible_to_client", *in.VisibleToClient)
	}
	if in.VisibleToSpecialist != nil {
		add("visible_to_specialist", *in.VisibleToSpecialist)
	}
	if in.Weight != nil {
		add("weight", *in.Weight)
	}
	if in.SortOrder != nil {
		add("sort_order", *in.SortOrder)
	}
	if in.IsReview != nil {
		add("is_review", *in.IsReview)
	}
	if len(sets) == 0 {
		return r.GetStep(ctx, id)
	}
	q := fmt.Sprintf(
		`UPDATE pipeline_steps SET %s WHERE id = $1
		 RETURNING id, stage_id, name, owner, duration_days, visible_to_client, visible_to_specialist,
		           weight, sort_order, is_review, created_at`,
		strings.Join(sets, ", "))
	var st Step
	err := r.db.QueryRow(ctx, q, args...).Scan(&st.ID, &st.StageID, &st.Name, &st.Owner, &st.DurationDays,
		&st.VisibleToClient, &st.VisibleToSpecialist, &st.Weight, &st.SortOrder, &st.IsReview, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, fmt.Errorf("patch step: %w", err)
	}
	return st, nil
}

func (r *Repo) DeleteStep(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM pipeline_steps WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Reorder — атомарно перепрошить sort_order стадий и шагов. Шаг может
// сменить stage_id (drag между колонками в редакторе → между стадиями).
// Только id внутри одного пайплайна — валидируется на уровне сервиса.
func (r *Repo) Reorder(ctx context.Context, pipelineID uuid.UUID, in ReorderInput) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, s := range in.Stages {
		tag, err := tx.Exec(ctx,
			`UPDATE pipeline_stages SET sort_order = $2
			 WHERE id = $1 AND pipeline_id = $3`,
			s.ID, s.SortOrder, pipelineID)
		if err != nil {
			return fmt.Errorf("update stage order: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		for _, st := range s.Steps {
			tag, err := tx.Exec(ctx,
				`UPDATE pipeline_steps SET sort_order = $2, stage_id = $3 WHERE id = $1`,
				st.ID, st.SortOrder, s.ID)
			if err != nil {
				return fmt.Errorf("update step order: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
	}
	return tx.Commit(ctx)
}

// StageBelongsToPipeline — для валидации, что приходящий /stages/{id} в
// admin-роутах действительно принадлежит редактируемому пайплайну.
// Используется в Reorder и в эндпоинтах создания шагов.
func (r *Repo) StagePipelineID(ctx context.Context, stageID uuid.UUID) (uuid.UUID, error) {
	var pid uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT pipeline_id FROM pipeline_stages WHERE id = $1`,
		stageID).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("stage pipeline id: %w", err)
	}
	return pid, nil
}
