package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Уведомления ЗАКАЗЧИКУ по его же выключателям.
//
// У заказчика в кабинете четыре галочки (client_notification_prefs), и
// до сих пор из них работала ровно одна — порог просмотров. Остальные
// три сохранялись, отдавались обратно и не делали ничего: событий под
// них не существовало вовсе. Переключатель, который ничего не меняет,
// хуже отсутствующего — человек считает, что подписался, и ждёт.
//
// Здесь события заводятся. Отправка устроена так же, как у креатора:
// сперва спрашиваем выключатель, потом пишем строку в notification_log
// (она же дедуп), потом эмитим — всё в одной транзакции Repo.Send.
// Ничего не отправив, событие не порождаем: иначе журнал разойдётся с
// тем, что человек получил.
//
// Своё событие, а не переиспользование project.publication_submitted:
// то событие про факт («ролик сдан»), а это — про адресата («сказать
// заказчику, потому что он просил»). Склеить их значило бы решать
// вопрос доставки в маршрутизаторе, где о выключателях знать неоткуда.
const (
	// ReminderClientNewVideo — «вышел новый ролик». Выключатель
	// client_notification_prefs.on_new_video.
	ReminderClientNewVideo = "client_new_video"
	// ReminderClientDateShift — «дата выкладки сдвинулась». Выключатель
	// client_notification_prefs.on_date_shift.
	ReminderClientDateShift = "client_date_shift"
	// ReminderClientWeeklyDigest — недельная сводка по проекту.
	// Выключатель client_notification_prefs.on_weekly_digest.
	ReminderClientWeeklyDigest = "client_weekly_digest"
)

// clientWants — кому и надо ли слать. Возвращает заказчика проекта и
// признак «выключатель включён».
//
// Строки настроек у проекта может не быть вовсе: человек ни разу не
// заходил в уведомления. Тогда берём умолчание из схемы — on_new_video
// и on_date_shift включены (см. migrations/00032_project_page.sql), а
// решать за него «молчим» мы не вправе: он этого не выбирал.
func (r *Repo) clientWants(
	ctx context.Context, projectID uuid.UUID, column string,
) (uuid.UUID, bool, error) {
	var clientID uuid.UUID
	var on bool
	err := r.db.QueryRow(ctx, `
SELECT pr.client_user_id, COALESCE(c.`+column+`, TRUE)
FROM projects pr
LEFT JOIN client_notification_prefs c
       ON c.project_id = pr.id AND c.user_id = pr.client_user_id
WHERE pr.id = $1`, projectID).Scan(&clientID, &on)
	if err != nil {
		if isNoRows(err) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("read client prefs: %w", err)
	}
	if clientID == uuid.Nil {
		// Проект без заказчика — внутренний. Уведомлять некого.
		return uuid.Nil, false, nil
	}
	return clientID, on, nil
}

// NotifyClientNewVideo — «у вас вышел новый ролик».
//
// Зовётся, когда выкладка стала ВЫШЕДШЕЙ, а не при каждой досланной
// ссылке: заказчику важен ролик, а не то, что креатор добавил четвёртую
// площадку. Дедуп в notification_log по (заказчик, вид, выкладка, день)
// добивает остальное — пересдача в тот же день второго письма не даст.
func (r *Repo) NotifyClientNewVideo(
	ctx context.Context, projectID, pubID uuid.UUID, now time.Time,
) (bool, error) {
	clientID, on, err := r.clientWants(ctx, projectID, "on_new_video")
	if err != nil || !on || clientID == uuid.Nil {
		return false, err
	}
	return r.Send(ctx, Reminder{
		Kind:          ReminderClientNewVideo,
		ProjectID:     projectID,
		PublicationID: pubID,
	}, &clientID, now)
}

// NotifyClientDateShift — «дата выкладки изменилась».
//
// Один вид на перенос и на снятие: для заказчика это один и тот же
// вопрос — «когда теперь». Что именно случилось, видно в payload
// события по полям from/to.
func (r *Repo) NotifyClientDateShift(
	ctx context.Context, projectID, pubID uuid.UUID, now time.Time,
) (bool, error) {
	clientID, on, err := r.clientWants(ctx, projectID, "on_date_shift")
	if err != nil || !on || clientID == uuid.Nil {
		return false, err
	}
	return r.Send(ctx, Reminder{
		Kind:          ReminderClientDateShift,
		ProjectID:     projectID,
		PublicationID: pubID,
	}, &clientID, now)
}

// ClientDigest — недельная сводка по проекту для заказчика.
//
// Считается на сервере и уезжает готовыми числами: складывать их на
// стороне бота значило бы завести вторую правду о том же периоде.
type ClientDigest struct {
	ProjectID    uuid.UUID `json:"project_id"`
	ProjectTitle string    `json:"project_title"`
	ClientUserID uuid.UUID `json:"client_user_id"`
	// Published — сколько роликов вышло за неделю.
	Published int `json:"published"`
	// Views — просмотры проекта на сегодня, всего.
	Views int64 `json:"views"`
	// ViewsGained — сколько из них набрано за эту неделю.
	ViewsGained int64 `json:"views_gained"`
	// NextDue — ближайшая плановая выкладка. Пусто — план кончился, и
	// это само по себе повод написать менеджеру.
	NextDue *time.Time `json:"next_due,omitempty"`
}

// clientDigestWeek — как часто уходит сводка. Ровно неделя, а не
// «каждый понедельник»: проекты стартуют в разные дни, и привязка к дню
// недели означала бы, что проект, заведённый во вторник, первую сводку
// ждёт шесть дней.
const clientDigestWeek = 7 * 24 * time.Hour

// DueClientDigests — кому пора слать недельную сводку.
//
// Отбираем по журналу, а не по расписанию: журнал и есть история
// отправок, и он же переживает перезапуск воркера. Проекты без выкладок
// не берём — сводка «ничего не вышло, просмотров ноль» не сводка, а шум.
func (r *Repo) DueClientDigests(ctx context.Context, now time.Time) ([]ClientDigest, error) {
	since := now.Add(-clientDigestWeek)
	rows, err := r.db.Query(ctx, `
SELECT pr.id, COALESCE(pr.title, ''), pr.client_user_id,
       (SELECT COUNT(*) FROM project_publications p
         WHERE p.project_id = pr.id AND p.status IN ('done', 'closed_manually')
           AND p.updated_at >= $2),
       COALESCE((
          SELECT SUM(cur.views) FROM project_publications p
          JOIN publication_links l ON l.publication_id = p.id
          LEFT JOIN LATERAL (
              SELECT views FROM video_stat_daily d
              WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
          ) cur ON TRUE
          WHERE p.project_id = pr.id AND p.status <> 'cancelled'
       ), 0),
       COALESCE((
          SELECT SUM(GREATEST(cur.views - COALESCE(prev.views, 0), 0))
          FROM project_publications p
          JOIN publication_links l ON l.publication_id = p.id
          LEFT JOIN LATERAL (
              SELECT views FROM video_stat_daily d
              WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
          ) cur ON TRUE
          LEFT JOIN LATERAL (
              SELECT views FROM video_stat_daily d
              WHERE d.link_id = l.id AND d.stat_date <= $2::date
              ORDER BY d.stat_date DESC LIMIT 1
          ) prev ON TRUE
          WHERE p.project_id = pr.id AND p.status <> 'cancelled'
       ), 0),
       (SELECT MIN(p.due_date) FROM project_publications p
         WHERE p.project_id = pr.id AND p.status = 'planned' AND p.due_date >= $3::date)
FROM projects pr
JOIN client_notification_prefs c
       ON c.project_id = pr.id AND c.user_id = pr.client_user_id
WHERE pr.status = 'active'
  AND pr.is_test = FALSE
  AND pr.client_user_id IS NOT NULL
  AND c.on_weekly_digest
  AND EXISTS (SELECT 1 FROM project_publications p WHERE p.project_id = pr.id)
  AND NOT EXISTS (
      SELECT 1 FROM notification_log n
      WHERE n.user_id = pr.client_user_id AND n.kind = $1
        AND n.subject_id = pr.id AND n.sent_date > $2::date)`,
		ReminderClientWeeklyDigest, since, now)
	if err != nil {
		return nil, fmt.Errorf("due client digests: %w", err)
	}
	defer rows.Close()

	out := make([]ClientDigest, 0, 8)
	for rows.Next() {
		var d ClientDigest
		if err := rows.Scan(&d.ProjectID, &d.ProjectTitle, &d.ClientUserID,
			&d.Published, &d.Views, &d.ViewsGained, &d.NextDue); err != nil {
			return nil, fmt.Errorf("scan client digest: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SendClientDigest — записать сводку в журнал и отправить.
//
// Предмет дедупа — ПРОЕКТ, а не выкладка: сводка про проект целиком, и
// вторая за тот же день означала бы, что воркер перезапустили.
func (r *Repo) SendClientDigest(ctx context.Context, d ClientDigest, now time.Time) (bool, error) {
	return r.sendClient(ctx, ReminderClientWeeklyDigest, d.ClientUserID, d.ProjectID, d, now)
}

// sendClient — общая отправка клиентского уведомления с предметом-проектом.
func (r *Repo) sendClient(
	ctx context.Context, kind string, recipient, projectID uuid.UUID, payload any, now time.Time,
) (bool, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING`, recipient, kind, projectID, truncateDay(now))
	if err != nil {
		return false, fmt.Errorf("log client notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		"project."+kind, payload); err != nil {
		return false, fmt.Errorf("emit client notification: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// RunClientDigests — недельные сводки заказчикам. Зовётся тикером
// воркера; ошибка по одному проекту не роняет остальные.
func (s *Service) RunClientDigests(ctx context.Context, now time.Time) (int, error) {
	items, err := s.repo.DueClientDigests(ctx, now)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, d := range items {
		ok, err := s.repo.SendClientDigest(ctx, d, now)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}
