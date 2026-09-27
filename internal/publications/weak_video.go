package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Ролик вышел, а просмотров нет.
//
// Через двое суток ролик уже либо пошёл, либо нет: в первый час ноль
// честный, на второй день — это ответ. Креатору об этом надо сказать
// самому — он единственный, кто может переснять, поправить обложку или
// перезалить, и узнать об этом из отчёта заказчика он не должен.
//
// Два разных сообщения, а не одно: «мало просмотров» и «ссылка не
// открывается» требуют разных действий. Первое — про сам ролик, второе
// — про то, что его вообще нет там, куда ведёт ссылка, и тут дело не в
// качестве.
const (
	// ReminderWeakVideo — ролик собрал меньше WeakViewsBelow.
	ReminderWeakVideo = "publication_weak"
	// ReminderDeadLink — площадка по ссылке ничего не отдаёт.
	ReminderDeadLink = "publication_dead_link"

	// WeakVideoAfterDays — через сколько суток после выхода спрашиваем.
	//
	// Двое: первые сутки ролик только расходится, и ноль в них ничего не
	// значит. На третьи было бы точнее, но переснимать уже поздно —
	// следующая выкладка обычно через день-два.
	WeakVideoAfterDays = 2

	// WeakViewsBelow — ниже этого числа считаем, что ролик не пошёл.
	WeakViewsBelow = 100
)

// WeakVideo — ролик, о котором говорим креатору.
type WeakVideo struct {
	PublicationID uuid.UUID `json:"publication_id"`
	ProjectID     uuid.UUID `json:"project_id"`
	ProjectTitle  string    `json:"project_title"`
	// CreatorID — чей ролик. Пусто у проекта без креаторов: событие
	// всё равно CRMOnly, собеседника у него нет ни там, ни здесь, и в
	// журнал оно ложится с user_id = NULL.
	CreatorID   *uuid.UUID `json:"creator_user_id,omitempty"`
	Title       string     `json:"title"`
	PublishedOn time.Time  `json:"published_on"`
	// Views — сколько набрал суммарно по всем площадкам.
	Views int64 `json:"views"`
	// Platforms — сколько площадок у ролика измерено. Ноль при Dead
	// значит, что не измерена ни одна: ссылки есть, а чисел нет.
	Platforms int `json:"platforms"`
	// Dead — ни одна площадка не ответила. Это не «мало просмотров», а
	// «ролика по ссылке нет», и говорить о нём надо другими словами.
	Dead bool `json:"dead"`
}

// Kind — вид напоминания: по нему бот выбирает текст.
func (w WeakVideo) Kind() string {
	if w.Dead {
		return ReminderDeadLink
	}
	return ReminderWeakVideo
}

// DueWeakVideos — ролики, по которым пора сказать креатору.
//
// Условия все обязательны и каждое отсекает свой ложный случай:
//   - ролику ровно WeakVideoAfterDays суток и он сдан — незаконченную
//     выкладку обсуждать рано, у неё ещё нет всех площадок;
//   - проект жив и не тестовый;
//   - сбор по нему идёт: у снятого с обхода числа заморожены, и «мало
//     просмотров» там означает «мы перестали считать», а не провал;
//   - хотя бы одна ссылка обойдена. Ни одного захода — это наша
//     очередь не разгреблась, а не его ролик не пошёл.
//
// Dead считается отдельно: ссылки есть, обходы были, а измеренных
// площадок ноль.
func (r *Repo) DueWeakVideos(ctx context.Context, now time.Time, limit int) ([]WeakVideo, error) {
	if limit <= 0 {
		limit = 100
	}
	day := truncateDay(now)
	rows, err := r.db.Query(ctx, `
WITH pub AS (
    SELECT p.id, p.project_id, p.creator_user_id, COALESCE(p.title, '') AS title,
           pr.title AS project_title,
           MIN(COALESCE(l.published_at, l.submitted_at))::date AS published_on,
           COUNT(*) FILTER (WHERE l.last_collected_at IS NOT NULL) AS collected,
           COUNT(*) FILTER (WHERE cur.views IS NOT NULL) AS measured,
           COALESCE(SUM(cur.views), 0)::bigint AS views
    FROM project_publications p
    JOIN projects pr ON pr.id = p.project_id
    JOIN publication_links l ON l.publication_id = p.id
    LEFT JOIN LATERAL (
        SELECT d.views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE p.status = 'done'
      -- Оба вида с выкладками: «ролик не пошёл» и «битая ссылка» —
      -- это про ролик, а не про человека, и у проекта без креаторов
      -- они такие же настоящие. Иначе метрика молчит там, где ссылки
      -- больше неоткуда взять.
      AND pr.kind IN ('creators_turnkey', 'brand_turnkey')
      AND pr.status = 'active'
      AND pr.is_test = FALSE
      AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)
    GROUP BY p.id, p.project_id, p.creator_user_id, p.title, pr.title
)
SELECT id, project_id, project_title, creator_user_id, title,
       published_on, views, measured, measured = 0
FROM pub
WHERE published_on = $1::date - $2::int
  AND collected > 0
  AND views < $3
  AND NOT EXISTS (
      SELECT 1 FROM notification_log n
      WHERE n.subject_id = pub.id AND n.kind IN ($4, $5))
ORDER BY views, published_on
LIMIT $6`,
		day, WeakVideoAfterDays, WeakViewsBelow, ReminderWeakVideo, ReminderDeadLink, limit)
	if err != nil {
		return nil, fmt.Errorf("list weak videos: %w", err)
	}
	defer rows.Close()

	out := make([]WeakVideo, 0, 8)
	for rows.Next() {
		var w WeakVideo
		if err := rows.Scan(&w.PublicationID, &w.ProjectID, &w.ProjectTitle, &w.CreatorID,
			&w.Title, &w.PublishedOn, &w.Views, &w.Platforms, &w.Dead); err != nil {
			return nil, fmt.Errorf("scan weak video: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SendWeakVideo — записать в журнал и положить событие.
//
// Один раз на ролик, а не раз в сутки: повторять «твой ролик не пошёл»
// каждый день — это не напоминание, а упрёк. Поэтому в журнале ищем
// свою запись по ролику вообще, без даты (см. DueWeakVideos), а сюда
// дата кладётся только для порядка.
func (r *Repo) SendWeakVideo(ctx context.Context, w WeakVideo, sentDate time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		w.CreatorID, w.Kind(), w.PublicationID, truncateDay(sentDate))
	if err != nil {
		return false, fmt.Errorf("log weak video: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, w.ProjectID.String(),
		"project."+w.Kind(), w); err != nil {
		return false, fmt.Errorf("emit weak video: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// RunWeakVideos — проход по роликам, которые не пошли.
//
// Ошибка на одном не роняет проход: остальные креаторы в этом не
// виноваты, а следующий тик попробует снова.
func (s *Service) RunWeakVideos(ctx context.Context, now time.Time, limit int) (int, error) {
	list, err := s.repo.DueWeakVideos(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, w := range list {
		ok, err := s.repo.SendWeakVideo(ctx, w, now)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}
