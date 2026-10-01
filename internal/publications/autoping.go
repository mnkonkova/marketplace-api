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
	ManagerDigest bool `json:"manager_digest"`
	// LowViews — ролик вышел, а просмотров почти нет. Включён по
	// умолчанию: срабатывает редко и по делу, в отличие от DayBefore.
	LowViews bool `json:"low_views"`
	// DayBefore — креатору в бот НАКАНУНЕ срока. Единственный из видов,
	// выключенный по умолчанию: он появился позже остальных, и включать
	// его молча всем — значит завтра утром написать каждому креатору
	// каждого проекта, никого не спросив. Колокольчик напротив креатора
	// в плане выкладок сильнее этой настройки, см.
	// CreatorReminderPref.
	DayBefore bool       `json:"day_before"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// defaultReminderPrefs — всё включено. Именно это означает отсутствие
// строки в таблице: проект, где менеджер ничего не трогал, пингуется.
func defaultReminderPrefs(projectID uuid.UUID) ReminderPrefs {
	return ReminderPrefs{
		ProjectID: projectID,
		DueToday:  true, Overdue: true, Incomplete: true, ManagerDigest: true,
		DayBefore: false, LowViews: true,
	}
}

// enabled — включён ли этот вид напоминания.
func (p ReminderPrefs) enabled(kind string) bool {
	switch kind {
	case ReminderDueTomorrow:
		return p.DayBefore
	case ReminderDueToday:
		return p.DueToday
	case ReminderOverdue:
		return p.Overdue
	case ReminderIncomplete:
		return p.Incomplete
	case ReminderNoViews:
		return p.LowViews
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
SELECT due_today, overdue, incomplete, manager_digest, day_before, low_views, updated_at
FROM project_reminder_prefs WHERE project_id = $1`, projectID).
		Scan(&out.DueToday, &out.Overdue, &out.Incomplete, &out.ManagerDigest,
			&out.DayBefore, &out.LowViews, &out.UpdatedAt)
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
  (project_id, due_today, overdue, incomplete, manager_digest, day_before,
   low_views, updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (project_id) DO UPDATE SET
  due_today = EXCLUDED.due_today,
  overdue = EXCLUDED.overdue,
  incomplete = EXCLUDED.incomplete,
  manager_digest = EXCLUDED.manager_digest,
  day_before = EXCLUDED.day_before,
  low_views = EXCLUDED.low_views,
  updated_by = EXCLUDED.updated_by,
  updated_at = now()
RETURNING updated_at`,
		p.ProjectID, p.DueToday, p.Overdue, p.Incomplete, p.ManagerDigest,
		p.DayBefore, p.LowViews, actor).
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

// ---- колокольчик напротив креатора ----
//
// Настройка проекта отвечает на вопрос «пингуем ли мы вообще», а
// колокольчик в плане — «пингуем ли накануне ЭТОГО человека». Второе
// сильнее первого: в плане у восьми креаторов восемь строк, и
// напоминание включают тому, кто забывает, а не всем разом.

// CreatorReminderPref — состояние колокольчика одного креатора.
type CreatorReminderPref struct {
	ProjectID     uuid.UUID  `json:"project_id"`
	CreatorUserID uuid.UUID  `json:"creator_user_id"`
	DayBefore     bool       `json:"day_before"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// CreatorReminderPrefs — колокольчики всех креаторов проекта. В карте
// только те, кому его трогали: остальные живут по настройке проекта.
func (r *Repo) CreatorReminderPrefs(ctx context.Context, projectID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := r.db.Query(ctx, `
SELECT creator_user_id, day_before
FROM project_creator_reminder_prefs WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, fmt.Errorf("load creator reminder prefs: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var on bool
		if err := rows.Scan(&id, &on); err != nil {
			return nil, fmt.Errorf("scan creator reminder pref: %w", err)
		}
		out[id] = on
	}
	return out, rows.Err()
}

// SaveCreatorReminderPref — щелчок по колокольчику.
func (r *Repo) SaveCreatorReminderPref(ctx context.Context, projectID, creatorID uuid.UUID,
	dayBefore bool, actor uuid.UUID) (CreatorReminderPref, error) {

	out := CreatorReminderPref{ProjectID: projectID, CreatorUserID: creatorID, DayBefore: dayBefore}
	var at time.Time
	if err := r.db.QueryRow(ctx, `
INSERT INTO project_creator_reminder_prefs
  (project_id, creator_user_id, day_before, updated_by, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (project_id, creator_user_id) DO UPDATE SET
  day_before = EXCLUDED.day_before,
  updated_by = EXCLUDED.updated_by,
  updated_at = now()
RETURNING updated_at`, projectID, creatorID, dayBefore, actor).Scan(&at); err != nil {
		return CreatorReminderPref{}, fmt.Errorf("save creator reminder pref: %w", err)
	}
	out.UpdatedAt = &at
	return out, nil
}

// SaveCreatorReminderPref — сервисная обёртка.
func (s *Service) SaveCreatorReminderPref(ctx context.Context, projectID, creatorID uuid.UUID,
	dayBefore bool, actor uuid.UUID) (CreatorReminderPref, error) {

	return s.repo.SaveCreatorReminderPref(ctx, projectID, creatorID, dayBefore, actor)
}
