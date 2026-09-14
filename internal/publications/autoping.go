package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Автопинг: какие напоминания бот шлёт по проекту сам.
//
// Четыре выключателя ровно по четырём видам напоминаний, которые умеет
// планировщик. Выключенный вид не откладывается и не копится — он просто
// не отправляется; менеджеру остаётся кнопка «напомнить» руками, она
// автопингом не управляется никогда.

// ReminderPrefs — настройки автопинга проекта.
type ReminderPrefs struct {
	ProjectID uuid.UUID `json:"project_id"`
	// DueToday — креатору в бот утром в день выкладки.
	DueToday bool `json:"due_today"`
	// Overdue — на следующий день после просрочки и дальше раз в сутки.
	Overdue bool `json:"overdue"`
	// Incomplete — ролик вышел, но собраны не все пять ссылок.
	Incomplete bool `json:"incomplete"`
	// ManagerDigest — сводка в общий чат менеджеров. Креаторы её не видят.
	ManagerDigest bool       `json:"manager_digest"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// defaultReminderPrefs — всё включено. Именно это означает отсутствие
// строки в таблице: проект, где менеджер ничего не трогал, пингуется.
func defaultReminderPrefs(projectID uuid.UUID) ReminderPrefs {
	return ReminderPrefs{
		ProjectID: projectID,
		DueToday:  true, Overdue: true, Incomplete: true, ManagerDigest: true,
	}
}

// enabled — включён ли этот вид напоминания.
func (p ReminderPrefs) enabled(kind string) bool {
	switch kind {
	case ReminderDueToday:
		return p.DueToday
	case ReminderOverdue:
		return p.Overdue
	case ReminderIncomplete:
		return p.Incomplete
	case ReminderManagerDigest:
		return p.ManagerDigest
	default:
		// Ручное напоминание менеджера автопингом не управляется.
		return true
	}
}

// ReminderPrefs — настройки проекта. Нет строки — всё включено.
func (r *Repo) ReminderPrefs(ctx context.Context, projectID uuid.UUID) (ReminderPrefs, error) {
	out := defaultReminderPrefs(projectID)
	err := r.db.QueryRow(ctx, `
SELECT due_today, overdue, incomplete, manager_digest, updated_at
FROM project_reminder_prefs WHERE project_id = $1`, projectID).
		Scan(&out.DueToday, &out.Overdue, &out.Incomplete, &out.ManagerDigest, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return ReminderPrefs{}, fmt.Errorf("load reminder prefs: %w", err)
	}
	return out, nil
}

// SaveReminderPrefs — записать настройки целиком.
func (r *Repo) SaveReminderPrefs(ctx context.Context, p ReminderPrefs, actor uuid.UUID) (ReminderPrefs, error) {
	// Одним UPSERT'ом, а не «прочитать-изменить-записать»: два менеджера,
	// щёлкнувшие разные тумблеры одновременно, иначе затирают друг друга.
	if err := r.db.QueryRow(ctx, `
INSERT INTO project_reminder_prefs
  (project_id, due_today, overdue, incomplete, manager_digest, updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (project_id) DO UPDATE SET
  due_today = EXCLUDED.due_today,
  overdue = EXCLUDED.overdue,
  incomplete = EXCLUDED.incomplete,
  manager_digest = EXCLUDED.manager_digest,
  updated_by = EXCLUDED.updated_by,
  updated_at = now()
RETURNING updated_at`,
		p.ProjectID, p.DueToday, p.Overdue, p.Incomplete, p.ManagerDigest, actor).
		Scan(&p.UpdatedAt); err != nil {
		return ReminderPrefs{}, fmt.Errorf("save reminder prefs: %w", err)
	}
	return p, nil
}

// ---- сервис ----

func (s *Service) ReminderPrefs(ctx context.Context, projectID uuid.UUID) (ReminderPrefs, error) {
	return s.repo.ReminderPrefs(ctx, projectID)
}

func (s *Service) SaveReminderPrefs(ctx context.Context, p ReminderPrefs, actor uuid.UUID) (ReminderPrefs, error) {
	return s.repo.SaveReminderPrefs(ctx, p, actor)
}

// ---- настройки проекта, которыми управляет менеджер ----

// ProjectSettings — переключатели проекта «креаторы под ключ».
//
// Оба поля меняют не оформление, а работу: этап черновика добавляет
// каждой выкладке второй срок, по которому пингует бот, а показ
// статистики закрывает заказчику и отчёт, и цифры в ленте.
type ProjectSettings struct {
	// DraftRequired — у выкладки два срока: сдать черновик и выложить.
	DraftRequired bool `json:"draft_required"`
	// ClientSeesStats — заказчик видит цифры.
	ClientSeesStats bool `json:"client_sees_stats"`
}

func (r *Repo) ProjectSettings(ctx context.Context, projectID uuid.UUID) (ProjectSettings, error) {
	var s ProjectSettings
	err := r.db.QueryRow(ctx,
		`SELECT draft_required, client_sees_stats FROM projects WHERE id = $1`, projectID).
		Scan(&s.DraftRequired, &s.ClientSeesStats)
	if err != nil {
		return ProjectSettings{}, ErrNotFound
	}
	return s, nil
}

// SaveProjectSettings — сохранить переключатели.
//
// Выключение этапа черновика не стирает уже проставленные сроки: они
// остаются в выкладках, где стоят, и просто перестают появляться у
// новых. Стереть их значило бы задним числом отменить договорённость,
// по которой креатор уже сдаёт.
func (r *Repo) SaveProjectSettings(
	ctx context.Context, projectID uuid.UUID, s ProjectSettings,
) (ProjectSettings, error) {
	tag, err := r.db.Exec(ctx, `
UPDATE projects SET draft_required = $2, client_sees_stats = $3, updated_at = now()
WHERE id = $1`, projectID, s.DraftRequired, s.ClientSeesStats)
	if err != nil {
		return ProjectSettings{}, fmt.Errorf("save project settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ProjectSettings{}, ErrNotFound
	}
	return s, nil
}

func (s *Service) ProjectSettings(ctx context.Context, projectID uuid.UUID) (ProjectSettings, error) {
	return s.repo.ProjectSettings(ctx, projectID)
}

func (s *Service) SaveProjectSettings(
	ctx context.Context, projectID uuid.UUID, in ProjectSettings,
) (ProjectSettings, error) {
	return s.repo.SaveProjectSettings(ctx, projectID, in)
}
