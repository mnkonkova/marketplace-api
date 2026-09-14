package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// «В каких проектах я креатор» — до этого ответить было нечем, и попасть
// на страницу выкладок можно было только по прямой ссылке.

// CreatorProject — строка списка «мои проекты» у креатора.
type CreatorProject struct {
	ProjectID uuid.UUID `json:"project_id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	AddedAt   time.Time `json:"added_at"`
	// Счётчики — только по СВОИМ выкладкам. Чужие креатор не видит нигде,
	// и в списке проектов тоже не должен.
	Total   int `json:"publications_total"`
	Open    int `json:"publications_open"`
	Overdue int `json:"publications_overdue"`
	// NextDueDate — ближайший срок из несданного. nil, когда сдано всё.
	NextDueDate *time.Time `json:"next_due_date,omitempty"`
}

// CreatorProjects — проекты, где пользователь в действующем составе.
//
// Выбывшие (removed_at) не показываются: доступа к проекту у человека
// больше нет, и строка в списке вела бы на 404.
func (r *Repo) CreatorProjects(ctx context.Context, creatorID uuid.UUID) ([]CreatorProject, error) {
	rows, err := r.db.Query(ctx, `
SELECT pc.project_id, pr.title, pr.kind::text, pr.status::text, pc.added_at,
       COUNT(p.id) FILTER (WHERE p.status <> 'cancelled'),
       COUNT(p.id) FILTER (WHERE p.status IN ('planned', 'partial')),
       COUNT(p.id) FILTER (
         WHERE p.status IN ('planned', 'partial')
           AND p.due_date < CURRENT_DATE
           -- Просьба о переносе снимает просрочку — так же, как в
           -- напоминаниях. Иначе список и письма расходятся в цифрах.
           AND NOT EXISTS (
             SELECT 1 FROM publication_date_requests dr
             WHERE dr.publication_id = p.id AND dr.status = 'pending')
       ),
       MIN(p.due_date) FILTER (WHERE p.status IN ('planned', 'partial'))
FROM project_creators pc
JOIN projects pr ON pr.id = pc.project_id
LEFT JOIN project_publications p
       ON p.project_id = pc.project_id AND p.creator_user_id = pc.creator_user_id
WHERE pc.creator_user_id = $1 AND pc.removed_at IS NULL
GROUP BY pc.project_id, pr.title, pr.kind, pr.status, pc.added_at
ORDER BY pr.status = 'done', pc.added_at DESC`, creatorID)
	if err != nil {
		return nil, fmt.Errorf("list creator projects: %w", err)
	}
	defer rows.Close()
	out := make([]CreatorProject, 0)
	for rows.Next() {
		var p CreatorProject
		if err := rows.Scan(&p.ProjectID, &p.Title, &p.Kind, &p.Status, &p.AddedAt,
			&p.Total, &p.Open, &p.Overdue, &p.NextDueDate); err != nil {
			return nil, fmt.Errorf("scan creator project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) CreatorProjects(ctx context.Context, creatorID uuid.UUID) ([]CreatorProject, error) {
	return s.repo.CreatorProjects(ctx, creatorID)
}

func (s *Service) ProjectCreators(ctx context.Context, projectID uuid.UUID) ([]Person, error) {
	return s.repo.ListProjectCreators(ctx, projectID)
}
