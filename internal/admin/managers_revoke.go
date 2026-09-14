package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Снятие роли менеджера с проверкой незавершённых проектов.
//
// Раньше revoke снимал роль молча. Проекты при этом оставались за
// человеком, который больше не может их открыть: карточка есть, в
// админском фильтре «менеджер» он есть, а зайти и продвинуть шаг —
// некому. Обнаруживалось это через неделю, когда клиент спрашивал,
// почему тишина.
//
// Теперь ручка отвечает 409 со списком проектов: сначала передайте их
// другому, потом снимайте роль.

// ActiveProjectRef — незавершённый проект в ответе 409. Названия хватает,
// чтобы админ узнал проект и решил, кому его передать.
type ActiveProjectRef struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ActiveProjectsError — на менеджере висят проекты. Отдельный тип, а не
// sentinel: хендлеру нужен сам список, иначе админ узнает «нельзя», но не
// узнает «что мешает».
type ActiveProjectsError struct {
	Projects []ActiveProjectRef
}

func (e *ActiveProjectsError) Error() string {
	return fmt.Sprintf("manager has %d active projects", len(e.Projects))
}

// activeProjectStatuses — «работа ещё идёт». done и cancelled не мешают
// снять роль: по ним ничего делать не нужно.
const activeProjectStatuses = `('draft','active','on_hold','dispute')`

// activeProjectsOfTx — незавершённые проекты менеджера. lock=true берёт
// строки под FOR UPDATE: проверка и снятие роли должны видеть один и тот
// же набор, иначе параллельное назначение проскочит между ними.
func activeProjectsOfTx(ctx context.Context, tx pgx.Tx, managerID uuid.UUID, lock bool) ([]ActiveProjectRef, error) {
	q := `
SELECT id, title, status::text, updated_at
FROM projects
WHERE assigned_to_user_id = $1 AND status IN ` + activeProjectStatuses + `
ORDER BY updated_at DESC`
	if lock {
		q += " FOR UPDATE"
	}
	rows, err := tx.Query(ctx, q, managerID)
	if err != nil {
		return nil, fmt.Errorf("active projects: %w", err)
	}
	defer rows.Close()
	out := make([]ActiveProjectRef, 0)
	for rows.Next() {
		var p ActiveProjectRef
		if err := rows.Scan(&p.ID, &p.Title, &p.Status, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan active project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
