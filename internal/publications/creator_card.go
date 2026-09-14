package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Карточка проекта глазами креатора.
//
// До этого одиночной ручки не было вовсе, и верх страницы — название,
// период, менеджер, «вас добавили 04.08» — фронт собирал из списка
// выкладок. Отдельные поля проекта (бриф, месячный план, нужен ли
// черновик) там взять было неоткуда.

// CreatorProjectCard — шапка страницы выкладок.
type CreatorProjectCard struct {
	CreatorProject
	// Brief — что делаем. Лежит в projects.notes.
	Brief string `json:"brief,omitempty"`
	// StartedAt/DueDate — период проекта. DueDate у проекта с креаторами
	// обычно пуст: сроки стоят у выкладок, а не у проекта.
	StartedAt *time.Time `json:"started_at,omitempty"`
	DueDate   *time.Time `json:"due_date,omitempty"`
	// MonthlyPlan — сколько роликов за месяц по договору. Из него
	// считается «роликов 6 из 12» в шапке.
	MonthlyPlan *int `json:"monthly_plan,omitempty"`
	// DraftRequired — у выкладки два срока: сдать черновик и выложить.
	DraftRequired bool `json:"draft_required"`
	// Manager — кому писать. nil, пока проект никто не взял.
	Manager *Person `json:"manager,omitempty"`
	// Platforms — пять площадок, на которые идёт ролик. Отдаём списком,
	// чтобы фронт не держал свою копию порядка колонок.
	Platforms []string `json:"platforms"`
}

// CreatorProjectCard — карточка одного проекта. Доступ проверяется тем же
// условием, что и выдача списка: не в действующем составе — ErrNotFound,
// а не 403, иначе чужой project_id подтверждается как существующий.
func (r *Repo) CreatorProjectCard(ctx context.Context, projectID, creatorID uuid.UUID) (CreatorProjectCard, error) {
	var c CreatorProjectCard
	var managerID *uuid.UUID
	err := r.db.QueryRow(ctx, `
SELECT pc.project_id, pr.title, pr.kind::text, pr.status::text, pc.added_at,
       COALESCE(pr.notes, ''), pr.started_at, pr.due_date, pr.monthly_plan,
       pr.draft_required, pr.assigned_to_user_id
FROM project_creators pc
JOIN projects pr ON pr.id = pc.project_id
WHERE pc.project_id = $1 AND pc.creator_user_id = $2 AND pc.removed_at IS NULL`,
		projectID, creatorID).Scan(
		&c.ProjectID, &c.Title, &c.Kind, &c.Status, &c.AddedAt,
		&c.Brief, &c.StartedAt, &c.DueDate, &c.MonthlyPlan,
		&c.DraftRequired, &managerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreatorProjectCard{}, ErrNotFound
	}
	if err != nil {
		return CreatorProjectCard{}, fmt.Errorf("load creator project card: %w", err)
	}
	c.Platforms = AllPlatforms

	// Счётчики — тем же запросом, что и в списке: цифры в шапке и в списке
	// проектов обязаны сходиться, а два разных SQL для одного показателя
	// расходятся при первой правке.
	if err := r.db.QueryRow(ctx, `
SELECT COUNT(*) FILTER (WHERE status <> 'cancelled'),
       COUNT(*) FILTER (WHERE status IN ('planned', 'partial')),
       COUNT(*) FILTER (
         WHERE status IN ('planned', 'partial')
           AND due_date < CURRENT_DATE
           AND NOT EXISTS (
             SELECT 1 FROM publication_date_requests dr
             WHERE dr.publication_id = project_publications.id AND dr.status = 'pending')
       ),
       MIN(due_date) FILTER (WHERE status IN ('planned', 'partial'))
FROM project_publications
WHERE project_id = $1 AND creator_user_id = $2`, projectID, creatorID).
		Scan(&c.Total, &c.Open, &c.Overdue, &c.NextDueDate); err != nil {
		return CreatorProjectCard{}, fmt.Errorf("count creator publications: %w", err)
	}

	if managerID != nil {
		people, err := r.resolveNames(ctx, []uuid.UUID{*managerID})
		if err != nil {
			return CreatorProjectCard{}, err
		}
		c.Manager = &Person{UserID: *managerID, Name: people[*managerID]}
	}
	return c, nil
}

func (s *Service) CreatorProjectCard(ctx context.Context, projectID, creatorID uuid.UUID) (CreatorProjectCard, error) {
	return s.repo.CreatorProjectCard(ctx, projectID, creatorID)
}
