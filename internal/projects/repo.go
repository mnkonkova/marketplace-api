package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("project not found")
	// ErrConflict — PATCH прислал устаревший updated_at; кто-то параллельно
	// успел отредактировать. Возвращается из мутационных методов (Фаза 4),
	// здесь объявлен для общего пакета.
	ErrConflict = errors.New("project updated_at mismatch")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// Pool — для соседних доменов (invites, notifications), которым нужен
// доступ к транзакциям общего проекта.
func (r *Repo) Pool() *pgxpool.Pool { return r.db }

// ─── Чтение проекта целиком (без шагов) ──────────────────────────────

// GetAdminByID возвращает полную форму проекта. Используется в
// admin-handlers и Directus. Без фильтра по ролям — middleware уже
// проверил access.
func (r *Repo) GetAdminByID(ctx context.Context, id uuid.UUID) (ProjectAdminView, error) {
	const q = `
SELECT id, lead_id, client_user_id, specialist_user_id, assigned_to_user_id,
       template_id, title, source, status, revisions_included, revisions_used,
       budget, COALESCE(notes, ''), started_at, completed_at, created_at, updated_at
FROM projects WHERE id = $1`
	var p ProjectAdminView
	err := r.db.QueryRow(ctx, q, id).Scan(
		&p.ID, &p.LeadID, &p.ClientUserID, &p.SpecialistUserID, &p.AssignedToUserID,
		&p.TemplateID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed,
		&p.Budget, &p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectAdminView{}, ErrNotFound
	}
	if err != nil {
		return ProjectAdminView{}, fmt.Errorf("query admin project: %w", err)
	}
	return p, nil
}

// GetClientByID — выдача для самого клиента. Дополнительная проверка
// `client_user_id == userID` — защита от ошибки в handler'е (даже если
// middleware не проверил).
func (r *Repo) GetClientByID(ctx context.Context, id, userID uuid.UUID) (ProjectClientView, error) {
	const q = `
SELECT id, lead_id, specialist_user_id, title, status,
       revisions_included, revisions_used,
       started_at, completed_at, created_at, updated_at
FROM projects
WHERE id = $1 AND client_user_id = $2`
	var p ProjectClientView
	err := r.db.QueryRow(ctx, q, id, userID).Scan(
		&p.ID, &p.LeadID, &p.SpecialistUserID, &p.Title, &p.Status,
		&p.RevisionsIncluded, &p.RevisionsUsed,
		&p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectClientView{}, ErrNotFound
	}
	if err != nil {
		return ProjectClientView{}, fmt.Errorf("query client project: %w", err)
	}
	return p, nil
}

// GetSpecialistByID — выдача для назначенного специалиста (read-only).
// Дополнительная проверка `specialist_user_id == userID`.
func (r *Repo) GetSpecialistByID(ctx context.Context, id, userID uuid.UUID) (ProjectSpecialistView, error) {
	const q = `
SELECT id, client_user_id, title, status,
       revisions_included, revisions_used,
       started_at, completed_at, created_at, updated_at
FROM projects
WHERE id = $1 AND specialist_user_id = $2`
	var p ProjectSpecialistView
	err := r.db.QueryRow(ctx, q, id, userID).Scan(
		&p.ID, &p.ClientUserID, &p.Title, &p.Status,
		&p.RevisionsIncluded, &p.RevisionsUsed,
		&p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSpecialistView{}, ErrNotFound
	}
	if err != nil {
		return ProjectSpecialistView{}, fmt.Errorf("query specialist project: %w", err)
	}
	return p, nil
}

// ─── Списки ─────────────────────────────────────────────────────────

// ListByClient — проекты конкретного клиента (для /me/projects).
// Сортировка по created_at DESC (новые сверху).
func (r *Repo) ListByClient(ctx context.Context, userID uuid.UUID) ([]ProjectClientView, error) {
	const q = `
SELECT id, lead_id, specialist_user_id, title, status,
       revisions_included, revisions_used,
       started_at, completed_at, created_at, updated_at
FROM projects
WHERE client_user_id = $1
ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("query client projects: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectClientView, 0, 16)
	for rows.Next() {
		var p ProjectClientView
		if err := rows.Scan(
			&p.ID, &p.LeadID, &p.SpecialistUserID, &p.Title, &p.Status,
			&p.RevisionsIncluded, &p.RevisionsUsed,
			&p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan client project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListByClientByLead — проекты клиента по конкретному лиду
// (для группировки в /me/projects/by_lead/{lead_id}).
func (r *Repo) ListByClientByLead(ctx context.Context, userID, leadID uuid.UUID) ([]ProjectClientView, error) {
	const q = `
SELECT id, lead_id, specialist_user_id, title, status,
       revisions_included, revisions_used,
       started_at, completed_at, created_at, updated_at
FROM projects
WHERE client_user_id = $1 AND lead_id = $2
ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, userID, leadID)
	if err != nil {
		return nil, fmt.Errorf("query client projects by lead: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectClientView, 0, 4)
	for rows.Next() {
		var p ProjectClientView
		if err := rows.Scan(
			&p.ID, &p.LeadID, &p.SpecialistUserID, &p.Title, &p.Status,
			&p.RevisionsIncluded, &p.RevisionsUsed,
			&p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListBySpecialist — назначенные на специалиста проекты.
func (r *Repo) ListBySpecialist(ctx context.Context, userID uuid.UUID) ([]ProjectSpecialistView, error) {
	const q = `
SELECT id, client_user_id, title, status,
       revisions_included, revisions_used,
       started_at, completed_at, created_at, updated_at
FROM projects
WHERE specialist_user_id = $1
ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("query specialist projects: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectSpecialistView, 0, 16)
	for rows.Next() {
		var p ProjectSpecialistView
		if err := rows.Scan(
			&p.ID, &p.ClientUserID, &p.Title, &p.Status,
			&p.RevisionsIncluded, &p.RevisionsUsed,
			&p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AdminListFilter — фильтры для GET /admin/projects.
// AssignedTo nil = «не фильтровать», not-nil = «assigned_to_user_id = X»
// (в т.ч. uuid.Nil = «без менеджера», но это нестандартный сценарий).
// Overdue = true → есть хотя бы один step с eta_date < today и status
// не в финале.
type AdminListFilter struct {
	Status     ProjectStatus // "" = no filter
	Source     ProjectSource // "" = no filter
	Client     *uuid.UUID
	Specialist *uuid.UUID
	AssignedTo *uuid.UUID
	Overdue    bool
	Limit      int
	Offset     int
}

func (r *Repo) ListAdmin(ctx context.Context, f AdminListFilter) ([]ProjectAdminView, error) {
	// Строим WHERE динамически. Параметризация позиционная для pgx.
	q := `
SELECT p.id, p.lead_id, p.client_user_id, p.specialist_user_id, p.assigned_to_user_id,
       p.template_id, p.title, p.source, p.status, p.revisions_included, p.revisions_used,
       p.budget, COALESCE(p.notes, ''), p.started_at, p.completed_at, p.created_at, p.updated_at
FROM projects p
WHERE 1=1`
	args := []any{}
	if f.Status != "" {
		args = append(args, f.Status)
		q += fmt.Sprintf(" AND p.status = $%d", len(args))
	}
	if f.Source != "" {
		args = append(args, f.Source)
		q += fmt.Sprintf(" AND p.source = $%d", len(args))
	}
	if f.Client != nil {
		args = append(args, *f.Client)
		q += fmt.Sprintf(" AND p.client_user_id = $%d", len(args))
	}
	if f.Specialist != nil {
		args = append(args, *f.Specialist)
		q += fmt.Sprintf(" AND p.specialist_user_id = $%d", len(args))
	}
	if f.AssignedTo != nil {
		args = append(args, *f.AssignedTo)
		q += fmt.Sprintf(" AND p.assigned_to_user_id = $%d", len(args))
	}
	if f.Overdue {
		// «Просроченные» = есть шаг с eta_date < CURRENT_DATE, который ещё
		// не закрыт (status not in done/skipped/rejected).
		q += ` AND EXISTS (
            SELECT 1 FROM project_steps s
            WHERE s.project_id = p.id
              AND s.eta_date < CURRENT_DATE
              AND s.status NOT IN ('done','skipped','rejected')
        )`
	}
	q += " ORDER BY p.created_at DESC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
		if f.Offset > 0 {
			args = append(args, f.Offset)
			q += fmt.Sprintf(" OFFSET $%d", len(args))
		}
	}

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query admin projects: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectAdminView, 0, 32)
	for rows.Next() {
		var p ProjectAdminView
		if err := rows.Scan(
			&p.ID, &p.LeadID, &p.ClientUserID, &p.SpecialistUserID, &p.AssignedToUserID,
			&p.TemplateID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed,
			&p.Budget, &p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan admin project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ─── Стадии и шаги проекта ───────────────────────────────────────────

// VisibilityFilter — фильтр шагов по аудитории. NoFilter — admin/Directus.
type VisibilityFilter int

const (
	VisibilityNoFilter        VisibilityFilter = iota
	VisibilityForClient                            // visible_to_client = TRUE
	VisibilityForSpecialist                        // visible_to_specialist = TRUE
)

// ListStages — стадии проекта в порядке sort_order. Шаги здесь НЕ
// прикладываем — это отдельный вызов ListSteps, чтобы избежать N+1
// в случае нескольких проектов и контролировать фильтр видимости.
func (r *Repo) ListStages(ctx context.Context, projectID uuid.UUID) ([]StageView, error) {
	const q = `
SELECT id, code, title, sort_order, started_at, completed_at
FROM project_stages
WHERE project_id = $1
ORDER BY sort_order`
	rows, err := r.db.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("query project stages: %w", err)
	}
	defer rows.Close()
	out := make([]StageView, 0, 8)
	for rows.Next() {
		var s StageView
		if err := rows.Scan(&s.ID, &s.Code, &s.Title, &s.SortOrder, &s.StartedAt, &s.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan stage: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSteps — шаги проекта с фильтром по видимости. Возвращаются плоско;
// маршрутизация по стадиям — на стороне service-слоя при сборке funnel.
func (r *Repo) ListSteps(ctx context.Context, projectID uuid.UUID, vis VisibilityFilter) ([]StepView, error) {
	q := `
SELECT id, code, title, owner, status, duration_days, weight, sort_order,
       eta_date, cta_payload, started_at, completed_at, updated_at, stage_id
FROM project_steps
WHERE project_id = $1`
	switch vis {
	case VisibilityForClient:
		q += " AND visible_to_client = TRUE"
	case VisibilityForSpecialist:
		q += " AND visible_to_specialist = TRUE"
	}
	q += " ORDER BY sort_order"

	rows, err := r.db.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("query project steps: %w", err)
	}
	defer rows.Close()
	out := make([]StepView, 0, 16)
	for rows.Next() {
		var s StepView
		var ownerStr string
		var stageID uuid.UUID // считываем для service-слоя через расширенный вариант
		if err := rows.Scan(
			&s.ID, &s.Code, &s.Title, &ownerStr, &s.Status, &s.DurationDays, &s.Weight, &s.SortOrder,
			&s.ETADate, &s.CTAPayload, &s.StartedAt, &s.CompletedAt, &s.UpdatedAt, &stageID,
		); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		s.Owner = StepOwner(ownerStr)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListStepsWithStage — расширенный вариант, возвращает также stage_id
// каждого шага. Нужен service-слою для построения funnel-дерева
// (stage → steps[]) без второго SQL.
type StepWithStage struct {
	StepView
	StageID uuid.UUID
}

func (r *Repo) ListStepsWithStage(ctx context.Context, projectID uuid.UUID, vis VisibilityFilter) ([]StepWithStage, error) {
	q := `
SELECT id, code, title, owner, status, duration_days, weight, sort_order,
       eta_date, cta_payload, started_at, completed_at, updated_at, stage_id
FROM project_steps
WHERE project_id = $1`
	switch vis {
	case VisibilityForClient:
		q += " AND visible_to_client = TRUE"
	case VisibilityForSpecialist:
		q += " AND visible_to_specialist = TRUE"
	}
	q += " ORDER BY sort_order"

	rows, err := r.db.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("query project steps: %w", err)
	}
	defer rows.Close()
	out := make([]StepWithStage, 0, 16)
	for rows.Next() {
		var s StepWithStage
		var ownerStr string
		if err := rows.Scan(
			&s.ID, &s.Code, &s.Title, &ownerStr, &s.Status, &s.DurationDays, &s.Weight, &s.SortOrder,
			&s.ETADate, &s.CTAPayload, &s.StartedAt, &s.CompletedAt, &s.UpdatedAt, &s.StageID,
		); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		s.Owner = StepOwner(ownerStr)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ─── Audit-log events (read-only в Фазе 1) ───────────────────────────

type StepEvent struct {
	ID          int64       `json:"id"`
	StepID      uuid.UUID   `json:"step_id"`
	ActorUserID *uuid.UUID  `json:"actor_user_id,omitempty"`
	ActorType   string      `json:"actor_type"`
	FromStatus  *StepStatus `json:"from_status,omitempty"`
	ToStatus    StepStatus  `json:"to_status"`
	Comment     string      `json:"comment,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
}

// ListEvents — аудит-лог проекта. Используется в admin-UI/Directus.
// Сортировка DESC по created_at (свежие сверху).
func (r *Repo) ListEvents(ctx context.Context, projectID uuid.UUID, limit int) ([]StepEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
SELECT id, step_id, actor_user_id, actor_type,
       from_status, to_status, COALESCE(comment, ''), created_at
FROM project_step_events
WHERE project_id = $1
ORDER BY created_at DESC
LIMIT $2`
	rows, err := r.db.Query(ctx, q, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	out := make([]StepEvent, 0, limit)
	for rows.Next() {
		var e StepEvent
		if err := rows.Scan(
			&e.ID, &e.StepID, &e.ActorUserID, &e.ActorType,
			&e.FromStatus, &e.ToStatus, &e.Comment, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
