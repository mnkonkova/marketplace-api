package projects

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SnapshotTemplate — собранное из БД дерево «шаблон → стадии → шаги».
// Используется в Фазе 2 для копирования в project_stages/_steps
// при старте проекта. После старта проект «отвязан» от шаблона:
// дальнейшие правки в service_template_* живых проектов не меняют.
type SnapshotTemplate struct {
	TemplateID        uuid.UUID
	Code              string
	Version           int
	Title             string
	RevisionsIncluded int
	Stages            []SnapshotStage
}

type SnapshotStage struct {
	Code      string
	Title     string
	SortOrder int
	Steps     []SnapshotStep
}

type SnapshotStep struct {
	Code                string
	Title               string
	Owner               StepOwner
	DurationDays        int
	VisibleToClient     bool
	VisibleToSpecialist bool
	Weight              int
	SortOrder           int
}

// ErrTemplateNotFound — нет активного шаблона с этим code+version.
var ErrTemplateNotFound = errors.New("service template not found or inactive")

// TemplateLoader — узкий контракт для LoadActiveTemplate. Реализуется
// *Repo (см. repo.go); вынесено отдельно, чтобы тесты Snapshot могли
// подменять источник, не таскать с собой полноценный *pgxpool.Pool.
type TemplateLoader interface {
	LoadActiveTemplate(ctx context.Context, code string, version int) (SnapshotTemplate, error)
}

// FunnelTemplateSummary — облегчённая сводка для GET /admin/funnel-templates.
// Используется Directus Flow «Create manual project» для дропдауна выбора
// воронки. Полный snapshot (со стадиями и шагами) даёт LoadActiveTemplate.
type FunnelTemplateSummary struct {
	TemplateID        uuid.UUID `json:"template_id"`
	Code              string    `json:"code"`
	Version           int       `json:"version"`
	Title             string    `json:"title"`
	RevisionsIncluded int       `json:"revisions_included"`
	StagesCount       int       `json:"stages_count"`
	StepsCount        int       `json:"steps_count"`
}

// ListActiveTemplates — для админ-эндпоинта выбора воронки при создании
// проекта. Возвращает только is_active=TRUE, отсортировано по code+version.
// Со счётчиками — менеджеру удобно видеть «маленькая воронка / большая».
func (r *Repo) ListActiveTemplates(ctx context.Context) ([]FunnelTemplateSummary, error) {
	const q = `
SELECT t.id, t.code, t.version, t.title, t.revisions_included,
       COALESCE(stages.cnt, 0) AS stages_count,
       COALESCE(steps.cnt, 0) AS steps_count
FROM service_templates t
LEFT JOIN (
    SELECT template_id, COUNT(*) AS cnt
    FROM service_template_stages GROUP BY template_id
) stages ON stages.template_id = t.id
LEFT JOIN (
    SELECT st.template_id, COUNT(*) AS cnt
    FROM service_template_steps stp
    JOIN service_template_stages st ON st.id = stp.stage_id
    GROUP BY st.template_id
) steps ON steps.template_id = t.id
WHERE t.is_active = TRUE
ORDER BY t.code, t.version DESC`
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query funnel templates: %w", err)
	}
	defer rows.Close()
	out := make([]FunnelTemplateSummary, 0, 4)
	for rows.Next() {
		var s FunnelTemplateSummary
		if err := rows.Scan(
			&s.TemplateID, &s.Code, &s.Version, &s.Title, &s.RevisionsIncluded,
			&s.StagesCount, &s.StepsCount,
		); err != nil {
			return nil, fmt.Errorf("scan funnel template: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LoadActiveTemplate (метод на Repo) — читает шаблон + стадии + шаги
// в один проход (3 запроса), собирает дерево. Сортировка по sort_order
// сохраняется. Если шаблон is_active=false — ошибка ErrTemplateNotFound:
// старт нового проекта по архивному шаблону не разрешён (живым проектам
// он остаётся доступен через project_steps, потому что snapshot).
func (r *Repo) LoadActiveTemplate(ctx context.Context, code string, version int) (SnapshotTemplate, error) {
	const tmplQ = `
SELECT id, code, version, title, revisions_included
FROM service_templates
WHERE code = $1 AND version = $2 AND is_active = TRUE`
	var snap SnapshotTemplate
	err := r.db.QueryRow(ctx, tmplQ, code, version).Scan(
		&snap.TemplateID, &snap.Code, &snap.Version, &snap.Title, &snap.RevisionsIncluded,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SnapshotTemplate{}, ErrTemplateNotFound
	}
	if err != nil {
		return SnapshotTemplate{}, fmt.Errorf("query service_template: %w", err)
	}

	// Стадии. Возвращаем сразу map id→ссылка для удобства последующего
	// раскидывания шагов по стадиям.
	const stagesQ = `
SELECT id, code, title, sort_order
FROM service_template_stages
WHERE template_id = $1
ORDER BY sort_order`
	stageRows, err := r.db.Query(ctx, stagesQ, snap.TemplateID)
	if err != nil {
		return SnapshotTemplate{}, fmt.Errorf("query template stages: %w", err)
	}
	defer stageRows.Close()

	// stageIndex: stage_id → индекс в snap.Stages, нужен чтобы быстро
	// найти куда положить шаг при следующем запросе.
	stageIndex := map[uuid.UUID]int{}
	for stageRows.Next() {
		var id uuid.UUID
		var s SnapshotStage
		if err := stageRows.Scan(&id, &s.Code, &s.Title, &s.SortOrder); err != nil {
			return SnapshotTemplate{}, fmt.Errorf("scan stage: %w", err)
		}
		stageIndex[id] = len(snap.Stages)
		snap.Stages = append(snap.Stages, s)
	}
	if err := stageRows.Err(); err != nil {
		return SnapshotTemplate{}, fmt.Errorf("stages iter: %w", err)
	}

	// Шаги. Один SQL по всем стадиям шаблона; маршрутизируем по stage_id.
	const stepsQ = `
SELECT s.stage_id, s.code, s.title, s.owner, s.duration_days,
       s.visible_to_client, s.visible_to_specialist, s.weight, s.sort_order
FROM service_template_steps s
JOIN service_template_stages st ON st.id = s.stage_id
WHERE st.template_id = $1
ORDER BY st.sort_order, s.sort_order`
	stepRows, err := r.db.Query(ctx, stepsQ, snap.TemplateID)
	if err != nil {
		return SnapshotTemplate{}, fmt.Errorf("query template steps: %w", err)
	}
	defer stepRows.Close()

	for stepRows.Next() {
		var stageID uuid.UUID
		var step SnapshotStep
		var ownerStr string
		if err := stepRows.Scan(
			&stageID, &step.Code, &step.Title, &ownerStr, &step.DurationDays,
			&step.VisibleToClient, &step.VisibleToSpecialist, &step.Weight, &step.SortOrder,
		); err != nil {
			return SnapshotTemplate{}, fmt.Errorf("scan step: %w", err)
		}
		step.Owner = StepOwner(ownerStr)
		idx, ok := stageIndex[stageID]
		if !ok {
			// Несогласованность БД (шаг с FK на стадию, которой нет в этом
			// шаблоне) — это баг миграции. Возвращаем ошибку явно.
			return SnapshotTemplate{}, fmt.Errorf("orphan step %s in stage %s", step.Code, stageID)
		}
		snap.Stages[idx].Steps = append(snap.Stages[idx].Steps, step)
	}
	if err := stepRows.Err(); err != nil {
		return SnapshotTemplate{}, fmt.Errorf("steps iter: %w", err)
	}

	return snap, nil
}

// Ensure repo type exists for the method receiver. Real Repo struct
// определяется в repo.go.
var _ = (*pgxpool.Pool)(nil)
