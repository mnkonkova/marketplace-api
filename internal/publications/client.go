package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Взгляд клиента: лента вышедших роликов и календарь месяца.
//
// Главное правило этого файла — клиент НЕ видит просрочек, пингов и
// внутренней кухни (требование Кл1). Поэтому статусы выкладок здесь
// схлопываются в два состояния: «вышло» и «запланировано». Отменённые
// не показываются вовсе.
//
// Это не косметика выдачи: если отдать клиенту настоящий статус, он
// увидит, что креатор просрочил, и разговор пойдёт мимо менеджера.

// ClientVideoStatus — то, что видит клиент вместо внутреннего статуса.
type ClientVideoStatus string

const (
	ClientStatusPublished ClientVideoStatus = "published"
	ClientStatusPlanned   ClientVideoStatus = "planned"
)

// clientStatus — перевод внутреннего статуса в клиентский.
// closed_manually для клиента — тоже «вышло»: ролик существует, а то,
// что менеджер закрыл выкладку неполной, — внутреннее дело.
func clientStatus(s Status) ClientVideoStatus {
	if s == StatusPartial || s == StatusDone || s == StatusClosedManually {
		return ClientStatusPublished
	}
	return ClientStatusPlanned
}

// ClientVideo — ролик в ленте клиента.
type ClientVideo struct {
	PublicationID uuid.UUID `json:"publication_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	// Title — название ролика, которое дал креатор при сдаче. Пусто у
	// роликов, сданных до того, как название начали спрашивать.
	Title       string     `json:"title,omitempty"`
	PublishedAt time.Time  `json:"published_at"`
	Platforms   []string   `json:"platforms"`
	Links       []string   `json:"links"`
	Views       int64      `json:"views"`
	Likes       int64      `json:"likes"`
	Comments    int64      `json:"comments"`
	CollectedAt *time.Time `json:"collected_at,omitempty"`
	// StatsHidden — цифры скрыты настройкой проекта. Отдельное поле, а не
	// молчаливые нули: ноль читается как «никто не смотрел».
	StatsHidden bool `json:"stats_hidden,omitempty"`
}

// CalendarItem — одна выкладка в дне календаря.
type CalendarItem struct {
	PublicationID uuid.UUID         `json:"publication_id"`
	CreatorUserID uuid.UUID         `json:"creator_user_id"`
	CreatorName   string            `json:"creator_name,omitempty"`
	Status        ClientVideoStatus `json:"status"`
}

// CalendarDay — день месяца с тем, что в нём выходит.
type CalendarDay struct {
	Date      time.Time      `json:"date"`
	Published int            `json:"published"`
	Planned   int            `json:"planned"`
	Items     []CalendarItem `json:"items"`
}

// NotificationPrefs — настройки уведомлений клиента по проекту.
type NotificationPrefs struct {
	ProjectID      uuid.UUID `json:"project_id"`
	UserID         uuid.UUID `json:"user_id"`
	OnNewVideo     bool      `json:"on_new_video"`
	OnWeeklyDigest bool      `json:"on_weekly_digest"`
	OnDateShift    bool      `json:"on_date_shift"`
	// ViewsThreshold — «ролик перешагнул порог просмотров». nil = не слать.
	ViewsThreshold *int64 `json:"views_threshold,omitempty"`
}

// ClientOwnsProject — проект принадлежит этому заказчику.
//
// Отдельно от ClientCanSeeStats: лента роликов и календарь доступны
// клиенту всегда, а показ статистики — по настройке проекта. Смешивать
// их значило бы закрыть клиенту календарь заодно с цифрами.
func (r *Repo) ClientOwnsProject(ctx context.Context, projectID, clientID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND client_user_id = $2)`,
		projectID, clientID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check project owner: %w", err)
	}
	return exists, nil
}

// ClientFeed — вышедшие ролики, новые сверху.
//
// В ленту попадают только выкладки со сданными ссылками: для клиента
// «ролик появился» — это когда его можно посмотреть, а не когда стоит
// дата в плане.
func (r *Repo) ClientFeed(ctx context.Context, projectID uuid.UUID, limit int) ([]ClientVideo, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Дата в ленте — ФАКТИЧЕСКАЯ дата выхода, а не момент, когда креатор
	// прислал ссылку. Это разные события: ролик мог выйти 31 июля, а
	// ссылка приехать через неделю — и лента подписывала бы весь месяц
	// работы одним днём.
	//
	// Где источник даты выхода не отдал, честно падаем на момент сдачи:
	// это единственное, что мы про ролик знаем. То же правило определяет
	// принадлежность периоду (billing.publishedInPeriodSQL) — на одном
	// экране не должно быть двух разных «когда это вышло».
	//
	// Сортировка по той же величине, что и показ: иначе список поедет
	// относительно подписей.
	const q = `
SELECT p.id, p.creator_user_id, p.title, MIN(COALESCE(l.published_at, l.submitted_at)),
       array_agg(l.platform ORDER BY l.platform),
       array_agg(l.url_canonical ORDER BY l.platform),
       COALESCE(SUM(cur.views), 0), COALESCE(SUM(cur.likes), 0),
       COALESCE(SUM(cur.comments), 0), MAX(cur.collected_at)
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments, collected_at
    FROM video_stat_daily d WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC LIMIT 1
) cur ON TRUE
WHERE p.project_id = $1 AND p.status <> 'cancelled'
GROUP BY p.id, p.creator_user_id, p.title
ORDER BY MIN(COALESCE(l.published_at, l.submitted_at)) DESC
LIMIT $2`
	rows, err := r.db.Query(ctx, q, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("client feed: %w", err)
	}
	defer rows.Close()

	out := make([]ClientVideo, 0, limit)
	for rows.Next() {
		var v ClientVideo
		if err := rows.Scan(&v.PublicationID, &v.CreatorUserID, &v.Title, &v.PublishedAt,
			&v.Platforms, &v.Links, &v.Views, &v.Likes, &v.Comments, &v.CollectedAt); err != nil {
			return nil, fmt.Errorf("scan client feed: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(out))
	for _, v := range out {
		ids = append(ids, v.CreatorUserID)
	}
	names, err := r.resolveNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].CreatorName = names[out[i].CreatorUserID]
	}
	return out, nil
}

// Calendar — месяц: что вышло и что запланировано, с отметкой креатора.
//
// Просрочки в календаре нет по построению: выкладка с прошедшей датой и
// без ссылок остаётся «запланированной». Клиент видит план, а разбирается
// с отставанием менеджер.
func (r *Repo) Calendar(ctx context.Context, projectID uuid.UUID, month time.Time) ([]CalendarDay, error) {
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, -1)

	const q = `
SELECT p.due_date, p.id, p.creator_user_id, p.status::text
FROM project_publications p
WHERE p.project_id = $1
  AND p.status <> 'cancelled'
  AND p.due_date BETWEEN $2 AND $3
ORDER BY p.due_date, p.created_at`
	rows, err := r.db.Query(ctx, q, projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	defer rows.Close()

	byDay := make(map[time.Time]*CalendarDay, 31)
	order := make([]time.Time, 0, 31)
	for rows.Next() {
		var day time.Time
		var item CalendarItem
		var status string
		if err := rows.Scan(&day, &item.PublicationID, &item.CreatorUserID, &status); err != nil {
			return nil, fmt.Errorf("scan calendar: %w", err)
		}
		day = truncateDay(day)
		item.Status = clientStatus(Status(status))

		d, ok := byDay[day]
		if !ok {
			d = &CalendarDay{Date: day}
			byDay[day] = d
			order = append(order, day)
		}
		if item.Status == ClientStatusPublished {
			d.Published++
		} else {
			d.Planned++
		}
		d.Items = append(d.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]CalendarDay, 0, len(order))
	for _, day := range order {
		out = append(out, *byDay[day])
	}
	// Имена — одним запросом на месяц, а не по запросу на день.
	ids := make([]uuid.UUID, 0, 32)
	for _, d := range out {
		for _, it := range d.Items {
			ids = append(ids, it.CreatorUserID)
		}
	}
	names, err := r.resolveNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		for j := range out[i].Items {
			out[i].Items[j].CreatorName = names[out[i].Items[j].CreatorUserID]
		}
	}
	return out, nil
}

// Prefs — настройки уведомлений. Если строки нет, возвращаются значения
// по умолчанию: включено всё, кроме порога просмотров.
func (r *Repo) Prefs(ctx context.Context, projectID, userID uuid.UUID) (NotificationPrefs, error) {
	p := NotificationPrefs{
		ProjectID: projectID, UserID: userID,
		OnNewVideo: true, OnWeeklyDigest: true, OnDateShift: true,
	}
	err := r.db.QueryRow(ctx, `
SELECT on_new_video, on_weekly_digest, on_date_shift, views_threshold
FROM client_notification_prefs WHERE project_id = $1 AND user_id = $2`,
		projectID, userID).Scan(&p.OnNewVideo, &p.OnWeeklyDigest, &p.OnDateShift, &p.ViewsThreshold)
	if err != nil {
		if isNoRows(err) {
			return p, nil
		}
		return p, fmt.Errorf("read prefs: %w", err)
	}
	return p, nil
}

// PrefsPatch — частичное изменение настроек. nil означает «не трогать».
type PrefsPatch struct {
	OnNewVideo     *bool
	OnWeeklyDigest *bool
	OnDateShift    *bool
	// ViewsThreshold: nil — не трогать; указатель на 0 — выключить.
	ViewsThreshold **int64
}

// PatchPrefs — изменить только заданные поля, одним запросом.
//
// Read-modify-write здесь не годится: клиент открывает настройки в двух
// вкладках, в одной выключает сводку, в другой ставит порог — и вторая
// запись затирает первую прочитанным до изменения значением. COALESCE
// решает это без блокировок и лишнего чтения.
func (r *Repo) PatchPrefs(ctx context.Context, projectID, userID uuid.UUID, patch PrefsPatch) error {
	var threshold *int64
	thresholdGiven := patch.ViewsThreshold != nil
	if thresholdGiven {
		threshold = *patch.ViewsThreshold
		if threshold != nil && *threshold <= 0 {
			// Ноль означает «не уведомлять», а не «порог в ноль просмотров».
			threshold = nil
		}
	}

	_, err := r.db.Exec(ctx, `
INSERT INTO client_notification_prefs
    (project_id, user_id, on_new_video, on_weekly_digest, on_date_shift, views_threshold, updated_at)
VALUES ($1, $2, COALESCE($3, TRUE), COALESCE($4, TRUE), COALESCE($5, TRUE), $6, now())
ON CONFLICT (project_id, user_id) DO UPDATE
SET on_new_video     = COALESCE($3, client_notification_prefs.on_new_video),
    on_weekly_digest = COALESCE($4, client_notification_prefs.on_weekly_digest),
    on_date_shift    = COALESCE($5, client_notification_prefs.on_date_shift),
    views_threshold  = CASE WHEN $7 THEN $6 ELSE client_notification_prefs.views_threshold END,
    updated_at = now()`,
		projectID, userID, patch.OnNewVideo, patch.OnWeeklyDigest, patch.OnDateShift,
		threshold, thresholdGiven)
	if err != nil {
		return fmt.Errorf("patch prefs: %w", err)
	}
	return nil
}

// SavePrefs — сохранить настройки целиком.
func (r *Repo) SavePrefs(ctx context.Context, p NotificationPrefs) error {
	_, err := r.db.Exec(ctx, `
INSERT INTO client_notification_prefs
    (project_id, user_id, on_new_video, on_weekly_digest, on_date_shift, views_threshold, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (project_id, user_id) DO UPDATE
SET on_new_video = EXCLUDED.on_new_video,
    on_weekly_digest = EXCLUDED.on_weekly_digest,
    on_date_shift = EXCLUDED.on_date_shift,
    views_threshold = EXCLUDED.views_threshold,
    updated_at = now()`,
		p.ProjectID, p.UserID, p.OnNewVideo, p.OnWeeklyDigest, p.OnDateShift, p.ViewsThreshold)
	if err != nil {
		return fmt.Errorf("save prefs: %w", err)
	}
	return nil
}

// ---- сервис ----

func (s *Service) ClientOwnsProject(ctx context.Context, projectID, clientID uuid.UUID) (bool, error) {
	return s.repo.ClientOwnsProject(ctx, projectID, clientID)
}

func (s *Service) ClientFeed(ctx context.Context, projectID uuid.UUID, limit int) ([]ClientVideo, error) {
	return s.repo.ClientFeed(ctx, projectID, limit)
}

func (s *Service) Calendar(ctx context.Context, projectID uuid.UUID, month time.Time) ([]CalendarDay, error) {
	return s.repo.Calendar(ctx, projectID, month)
}

func (s *Service) Prefs(ctx context.Context, projectID, userID uuid.UUID) (NotificationPrefs, error) {
	return s.repo.Prefs(ctx, projectID, userID)
}

// PatchPrefs — изменить только заданные поля.
func (s *Service) PatchPrefs(ctx context.Context, projectID, userID uuid.UUID, patch PrefsPatch) error {
	return s.repo.PatchPrefs(ctx, projectID, userID, patch)
}

// SavePrefs — с проверкой порога: ноль и отрицательные значения означают
// «не слать», и хранить их как порог нельзя.
func (s *Service) SavePrefs(ctx context.Context, p NotificationPrefs) error {
	if p.ViewsThreshold != nil && *p.ViewsThreshold <= 0 {
		p.ViewsThreshold = nil
	}
	return s.repo.SavePrefs(ctx, p)
}

// ReminderClientThreshold — «ролик перешагнул порог просмотров».
const ReminderClientThreshold = "client_views_threshold"

// NotifyThresholdCrossed — уведомить клиента, что ролик перешагнул
// заданный им порог просмотров.
//
// Ровно один раз за ролик, а не каждый день после превышения: проверяем
// журнал БЕЗ учёта даты. Обычный дедуп по (получатель, вид, предмет, день)
// здесь не годится — он позволил бы слать одно и то же каждые сутки, пока
// просмотры остаются выше порога.
func (r *Repo) NotifyThresholdCrossed(ctx context.Context, projectID, pubID uuid.UUID, now time.Time) (bool, error) {
	var clientID uuid.UUID
	var threshold *int64
	err := r.db.QueryRow(ctx, `
SELECT pr.client_user_id, c.views_threshold
FROM projects pr
LEFT JOIN client_notification_prefs c
       ON c.project_id = pr.id AND c.user_id = pr.client_user_id
WHERE pr.id = $1`, projectID).Scan(&clientID, &threshold)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("read threshold: %w", err)
	}
	if threshold == nil || *threshold <= 0 {
		return false, nil
	}

	var views int64
	if err := r.db.QueryRow(ctx, `
SELECT COALESCE(SUM(cur.views), 0)
FROM publication_links l
LEFT JOIN LATERAL (
    SELECT views FROM video_stat_daily d WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC LIMIT 1
) cur ON TRUE
WHERE l.publication_id = $1`, pubID).Scan(&views); err != nil {
		return false, fmt.Errorf("sum publication views: %w", err)
	}
	if views < *threshold {
		return false, nil
	}

	var already bool
	if err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM notification_log
    WHERE user_id = $1 AND kind = $2 AND subject_id = $3
)`, clientID, ReminderClientThreshold, pubID).Scan(&already); err != nil {
		return false, fmt.Errorf("check threshold log: %w", err)
	}
	if already {
		return false, nil
	}

	rem := Reminder{
		Kind:          ReminderClientThreshold,
		ProjectID:     projectID,
		PublicationID: pubID,
	}
	return r.Send(ctx, rem, &clientID, now)
}
