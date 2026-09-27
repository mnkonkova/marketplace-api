package orders

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Рассылка по заявке: приглашение уходит ВСЕМ известным креаторам.
//
// Очереди приглашений больше нет. Прежняя логика звала первых по
// приоритету, ждала трое суток, сжигала молчание и звала следующего —
// то есть заказчик ждал неделю ради состава из трёх человек. Теперь
// заявка уходит всем сразу, отвечают желающие, а состав из ответивших
// собирает менеджер. Отметка «хочу особенно» едет припиской в том же
// сообщении, а не отдельным приглашением: второй пинг тому же человеку
// съедает его дневной лимит и выглядит как беспорядок.

const (
	// broadcastKind — вид записи в notification_log. По нему же
	// считается дневной лимит: одна заявка — одно сообщение человеку,
	// сколько бы раз рассылку ни перезапускали.
	broadcastKind = "order_broadcast"

	// broadcastDailyCap — сколько сообщений в сутки допустимо одному
	// человеку. Не про технику: креатор, которому за день пришло
	// тридцать заявок, перестаёт читать тридцать первую, а следом и
	// все остальные наши сообщения.
	broadcastDailyCap = 30
)

// BroadcastResult — что получилось у рассылки.
type BroadcastResult struct {
	// Recipients — скольким ушло. Ноль — не ошибка: известных креаторов
	// может не быть вовсе, — но и не то, о чём стоит узнать через
	// неделю, поэтому событие пишется и с нулём.
	Recipients int `json:"recipients"`
	// Preferred — сколько из них заказчик отметил «хочу особенно».
	Preferred int `json:"preferred"`
	// SkippedOverLimit — сколько пропущено по дневному лимиту.
	SkippedOverLimit int `json:"skipped_over_limit"`
}

// Broadcast — разослать заявку известным креаторам.
//
// Идемпотентна: повторный вызов не напишет второй раз тому, кому уже
// написали (broadcast_at на строке кандидата и запись в
// notification_log), — а без этого перезапуск воркера или второй клик
// менеджера превращаются в веерную рассылку дублей.
func (r *Repo) Broadcast(ctx context.Context, orderID uuid.UUID, now time.Time) (BroadcastResult, error) {
	var out BroadcastResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var (
			clientID  uuid.UUID
			projectID *uuid.UUID
			status    OrderStatus
		)
		if err := tx.QueryRow(ctx, `
SELECT client_user_id, project_id, status FROM creator_orders WHERE id = $1 FOR UPDATE`,
			orderID).Scan(&clientID, &projectID, &status); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		if status == StatusCancelled {
			return ErrWrongStatus
		}

		// Кому шлём.
		//
		// «Известный креатор» — это активный специалист из категорий
		// блогеров и UGC, чей профиль одобрен или ждёт модерации.
		// Публикация в каталоге НЕ требуется: каталог — про витрину, а
		// человек без опубликованного профиля всё равно может снять
		// ролик. Когда появится бот креатора (docs/PLAN_TELEGRAM_BOTS.md),
		// сюда добавится условие «бот подключён» — сегодня подключать
		// нечего, и единственное, что делает слово «креатор»
		// осмысленным, это категории.
		//
		// Дневной лимит считается прямо здесь, а не после выборки:
		// иначе пришлось бы тащить в память всех и отсеивать половину.
		rows, err := tx.Query(ctx, `
SELECT u.id,
       (SELECT count(*) FROM notification_log n
         WHERE n.user_id = u.id AND n.sent_date = $3::date) >= $4 AS over_limit
FROM users u
JOIN specialist_profiles sp    ON sp.user_id = u.id
JOIN specialist_categories sc  ON sc.user_id = u.id
WHERE u.is_active AND NOT u.is_manager AND NOT u.is_admin
  AND u.kind IN ('specialist', 'both')
  AND u.id <> $1
  AND sp.moderation_status IN ('approved', 'pending_review')
  AND sc.category_code = ANY($2)
GROUP BY u.id`, clientID, CreatorCategories, now, broadcastDailyCap)
		if err != nil {
			return fmt.Errorf("known creators: %w", err)
		}
		ids := make([]uuid.UUID, 0, 64)
		for rows.Next() {
			var (
				id   uuid.UUID
				over bool
			)
			if err := rows.Scan(&id, &over); err != nil {
				rows.Close()
				return fmt.Errorf("scan known creator: %w", err)
			}
			if over {
				out.SkippedOverLimit++
				continue
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Строка кандидата у каждого, кому написали: отклик приходит от
		// того, кого в подборке заказчика не было, и приткнуть его
		// иначе некуда. Отметку «хочу особенно» не трогаем — она стоит
		// на строках, заведённых при создании заявки.
		//
		// priority здесь — просто порядок строк на экране: уникальным
		// он перестал быть вместе с очередью (00067).
		var next int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(priority), 0) FROM order_candidates WHERE order_id = $1`,
			orderID).Scan(&next); err != nil {
			return fmt.Errorf("next priority: %w", err)
		}
		priorities := make([]int, len(ids))
		for i := range ids {
			priorities[i] = next + i + 1
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO order_candidates (order_id, creator_user_id, priority, status, broadcast_at)
SELECT $1, c, p, 'reserve', $4 FROM unnest($2::uuid[], $3::int[]) AS t(c, p)
ON CONFLICT (order_id, creator_user_id) DO UPDATE
   SET broadcast_at = COALESCE(order_candidates.broadcast_at, EXCLUDED.broadcast_at)`,
			orderID, ids, priorities, now); err != nil {
			return fmt.Errorf("insert broadcast candidates: %w", err)
		}

		// Журнал — он же защита от второго сообщения: у отмеченных
		// заказчиком строка кандидата уже была, и по ней «писали или
		// нет» не отличить.
		day := now.UTC().Truncate(24 * time.Hour)
		sent := make([]uuid.UUID, 0, len(ids))
		for _, id := range ids {
			tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, id, broadcastKind, orderID, day)
			if err != nil {
				return fmt.Errorf("log broadcast: %w", err)
			}
			if tag.RowsAffected() > 0 {
				sent = append(sent, id)
			}
		}
		out.Recipients = len(sent)

		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM order_candidates
WHERE order_id = $1 AND is_preferred AND creator_user_id = ANY($2)`,
			orderID, sent).Scan(&out.Preferred); err != nil {
			return fmt.Errorf("count preferred: %w", err)
		}

		// Событие пишем ВСЕГДА, даже с нулём получателей: «рассылка
		// была и не нашла никого» и «рассылки не было» — разные
		// поломки, и различить их потом больше нечем.
		payload := map[string]any{
			"order_id":           orderID,
			"recipients":         out.Recipients,
			"preferred":          out.Preferred,
			"skipped_over_limit": out.SkippedOverLimit,
		}
		withProject(payload, projectID)
		return outbox.Emit(ctx, tx, outbox.AggregateProject, orderID.String(),
			outbox.EventOrderBroadcastSent, payload)
	})
	return out, err
}
