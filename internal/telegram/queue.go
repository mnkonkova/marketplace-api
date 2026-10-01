package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Очередь сообщений для ботов: мы кладём, бот забирает.
//
// Так доставка перестала зависеть от того, дозвонится ли наша ВДС до
// чужого облака. Раньше воркер слал HTTP-запрос в сервис бота, и
// первого октября 2026 это стоило потерянного уведомления: сервис
// спал, холодный старт шёл дольше таймаута, все десять попыток
// оборвались на ожидании заголовков, событие уехало в DLQ.
//
// Обратное направление работает всегда — сервис бота и так ходит в
// наш API за привязкой и пользователями, на этом живёт мини-апп.
// Поэтому стрелку перевернули: очередь здесь, бот опрашивает её сам.

// LeaseTTL — сколько выданное сообщение считается «в работе».
//
// Бот мог упасть между выдачей и отправкой, и тогда сообщение обязано
// вернуться в очередь. Минута: опрос у бота секундный, и дольше
// держать смысла нет.
const LeaseTTL = time.Minute

// MessagesBatch — сколько строк отдаём за один опрос. Потолок, а не
// цель: обычно в очереди пусто.
const MessagesBatch = 20

// Message — одно сообщение в очереди, как его видит бот.
type Message struct {
	ID       int64           `json:"id"`
	Bot      string          `json:"bot"`
	Audience string          `json:"audience"`
	Type     string          `json:"event_type"`
	Envelope json.RawMessage `json:"envelope"`
}

// Queue — запись в очередь. Реализует eventroute.BotSender, поэтому
// маршрутизация событий о переходе на опрос не знает вовсе: она
// по-прежнему «отправляет конверт», просто адрес у него теперь —
// строка в таблице.
type Queue struct {
	db *pgxpool.Pool
}

func NewQueue(db *pgxpool.Pool) *Queue { return &Queue{db: db} }

// конвертHead — те поля конверта, которые нужны самой очереди:
// по ним она адресует, дедуплицирует и показывает в логе.
type envelopeHead struct {
	Bot       string `json:"bot"`
	Audience  string `json:"audience"`
	EventType string `json:"event_type"`
	EventID   string `json:"event_id"`
}

// SendEnvelope — положить конверт в очередь.
//
// Разбираем собственный JSON обратно, вместо того чтобы требовать от
// вызывающего типизированную структуру: конверт собирает eventroute, и
// тащить его тип сюда значило бы связать домен доставки с доменом
// маршрутизации ради четырёх полей.
func (q *Queue) SendEnvelope(ctx context.Context, v any) error {
	if q == nil {
		return nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	var head envelopeHead
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("read envelope head: %w", err)
	}
	if head.Bot == "" || head.Audience == "" {
		return fmt.Errorf("envelope without bot/audience")
	}
	// event_id в конверте — строка (id записи outbox). Пустая строка
	// значит «событие не из outbox»: такое не дедуплицируем, но и не
	// отбрасываем.
	var eventID *string
	if head.EventID != "" {
		eventID = &head.EventID
	}
	_, err = q.db.Exec(ctx, `
INSERT INTO bot_messages (bot, audience, event_type, envelope, event_id)
VALUES ($1, $2, $3, $4, NULLIF($5, '')::bigint)
ON CONFLICT (event_id) WHERE event_id IS NOT NULL DO NOTHING`,
		head.Bot, head.Audience, head.EventType, body, derefOrEmpty(eventID))
	if err != nil {
		return fmt.Errorf("enqueue bot message: %w", err)
	}
	return nil
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Lease — выдать боту пачку неотправленных сообщений.
//
// Выдача — аренда, а не удаление: пока бот не подтвердил доставку,
// сообщение остаётся в очереди и через LeaseTTL достанется следующему
// опросу. Потерять сообщение из-за упавшего контейнера нельзя, а
// отправить дважды — можно пережить: на стороне бота стоит дедуп по
// event_id.
func (r *Repo) Lease(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 || limit > MessagesBatch {
		limit = MessagesBatch
	}
	rows, err := r.db.Query(ctx, `
UPDATE bot_messages SET taken_at = now(), attempts = attempts + 1
WHERE id IN (
    SELECT id FROM bot_messages
    WHERE delivered_at IS NULL
      AND (taken_at IS NULL OR taken_at < now() - $2::interval)
    ORDER BY id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, bot, audience, event_type, envelope`, limit, LeaseTTL.String())
	if err != nil {
		return nil, fmt.Errorf("lease bot messages: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Bot, &m.Audience, &m.Type, &m.Envelope); err != nil {
			return nil, fmt.Errorf("scan bot message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Ack — что бот с пачкой сделал.
//
// Доставленные закрываем, упавшие возвращаем в очередь с причиной:
// следующий опрос возьмёт их снова. Отдельного DLQ здесь нет
// намеренно — очередь короткая, и неудача видна по attempts.
func (r *Repo) Ack(ctx context.Context, delivered []int64, failed map[int64]string) error {
	if len(delivered) > 0 {
		if _, err := r.db.Exec(ctx, `
UPDATE bot_messages SET delivered_at = now(), last_error = NULL
WHERE id = ANY($1) AND delivered_at IS NULL`, delivered); err != nil {
			return fmt.Errorf("ack delivered: %w", err)
		}
	}
	for id, reason := range failed {
		if _, err := r.db.Exec(ctx, `
UPDATE bot_messages SET taken_at = NULL, last_error = left($2, 500)
WHERE id = $1 AND delivered_at IS NULL`, id, reason); err != nil {
			return fmt.Errorf("ack failed: %w", err)
		}
	}
	return nil
}

// CleanupDelivered — убрать доставленное старше срока.
//
// Очередь — транспорт, а не журнал: что ушло в телеграм, записано в
// notification_log, и держать это здесь второй раз незачем.
func (r *Repo) CleanupDelivered(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx, `
DELETE FROM bot_messages
WHERE delivered_at IS NOT NULL AND delivered_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("cleanup bot messages: %w", err)
	}
	return tag.RowsAffected(), nil
}
