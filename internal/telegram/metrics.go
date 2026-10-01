package telegram

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Метрики очереди сообщений для ботов.
//
// Нужны по той же причине, по которой очередь вообще появилась:
// раньше о непрошедшем уведомлении говорил алерт про мёртвую очередь
// outbox. После переворота доставки туда ничего не попадает — воркер
// кладёт строку в таблицу и считает дело сделанным, — и без
// собственного счётчика молчание стало бы неотличимо от «всё
// доставлено».
var (
	// botQueuePending — сколько сообщений ждут отправки прямо сейчас.
	// В норме около нуля: бот опрашивает очередь раз в несколько
	// секунд. Стабильно ненулевая — бот не приходит.
	botQueuePending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bot_queue_pending",
		Help: "Bot messages waiting to be delivered (delivered_at IS NULL).",
	})

	// botQueueOldestSeconds — возраст самого старого неотправленного.
	// Именно по нему и стоит тревожить: одна застрявшая строка важнее
	// их количества, а очередь короткая и по счётчику этого не видно.
	botQueueOldestSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bot_queue_oldest_seconds",
		Help: "Age of the oldest undelivered bot message, seconds. 0 when the queue is empty.",
	})

	// botQueueStuck — сообщения, которые бот брал и не доставил.
	// Отличается от pending: эти уже пробовали отправить, и причина
	// записана в last_error.
	botQueueStuck = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bot_queue_failed",
		Help: "Bot messages with a recorded delivery error (last_error IS NOT NULL).",
	})
)

// RefreshMetrics — снять показания очереди. Зовётся воркером по
// таймеру, рядом с gauge'ами outbox.
func (r *Repo) RefreshMetrics(ctx context.Context) error {
	var pending, failed int64
	var oldest *time.Time
	if err := r.db.QueryRow(ctx, `
SELECT
  (SELECT COUNT(*) FROM bot_messages WHERE delivered_at IS NULL),
  (SELECT COUNT(*) FROM bot_messages WHERE delivered_at IS NULL AND last_error IS NOT NULL),
  (SELECT MIN(created_at) FROM bot_messages WHERE delivered_at IS NULL)
`).Scan(&pending, &failed, &oldest); err != nil {
		return err
	}
	botQueuePending.Set(float64(pending))
	botQueueStuck.Set(float64(failed))
	if oldest == nil {
		botQueueOldestSeconds.Set(0)
		return nil
	}
	botQueueOldestSeconds.Set(time.Since(*oldest).Seconds())
	return nil
}
