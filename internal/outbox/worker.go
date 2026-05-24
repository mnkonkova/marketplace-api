package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler func(ctx context.Context, aggregateID, eventType string, payload []byte) error

type Worker struct {
	db              *pgxpool.Pool
	handlers        map[string]Handler
	batchSize       int
	pollInterval    time.Duration
	idleBackoff     time.Duration
	maxAttempts     int
	backoffCap      time.Duration
	retention       time.Duration
	cleanupInterval time.Duration
	gaugeInterval   time.Duration
	logger          *slog.Logger
}

type Config struct {
	BatchSize    int
	PollInterval time.Duration
	IdleBackoff  time.Duration

	// MaxAttempts — после стольких неуспешных попыток запись уезжает в DLQ
	// (dead_at = now()) и больше не обрабатывается. 0 → дефолт 10.
	MaxAttempts int

	// BackoffCap — потолок задержки между ретраями. Backoff экспоненциальный:
	// 2^attempts секунд, но не больше cap. 0 → дефолт 10 минут.
	BackoffCap time.Duration

	// Retention — сколько хранить успешно обработанные записи. Cleanup-горутина
	// раз в CleanupInterval удаляет всё processed_at < now()-Retention (живых;
	// dead-записи не трогаем). 0 → дефолт 7 дней.
	Retention time.Duration

	// CleanupInterval — период работы cleanup-горутины. 0 → дефолт 1 час.
	CleanupInterval time.Duration

	// GaugeInterval — период обновления gauge'ей outbox_pending/outbox_dead.
	// 0 → дефолт 30 секунд.
	GaugeInterval time.Duration
}

func NewWorker(db *pgxpool.Pool, logger *slog.Logger, handlers map[string]Handler, c Config) *Worker {
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.IdleBackoff <= 0 {
		c.IdleBackoff = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	if c.BackoffCap <= 0 {
		c.BackoffCap = 10 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 7 * 24 * time.Hour
	}
	if c.CleanupInterval <= 0 {
		c.CleanupInterval = time.Hour
	}
	if c.GaugeInterval <= 0 {
		c.GaugeInterval = 30 * time.Second
	}
	return &Worker{
		db:              db,
		handlers:        handlers,
		batchSize:       c.BatchSize,
		pollInterval:    c.PollInterval,
		idleBackoff:     c.IdleBackoff,
		maxAttempts:     c.MaxAttempts,
		backoffCap:      c.BackoffCap,
		retention:       c.Retention,
		cleanupInterval: c.CleanupInterval,
		gaugeInterval:   c.GaugeInterval,
		logger:          logger,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("outbox worker started",
		"batch", w.batchSize, "poll", w.pollInterval,
		"max_attempts", w.maxAttempts, "backoff_cap", w.backoffCap,
		"retention", w.retention)

	// Sidecar-горутины: cleanup старых processed-записей и периодический
	// refresh gauge'ей. Контекст один и тот же — оба остановятся вместе
	// с основным циклом по SIGTERM.
	go w.runCleanup(ctx)
	go w.runGaugeRefresh(ctx)

	for {
		if ctx.Err() != nil {
			return nil
		}
		processed, err := w.tick(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			w.logger.Error("outbox tick", "err", err)
		}
		delay := w.pollInterval
		if processed == 0 {
			delay = w.idleBackoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

type entry struct {
	id          int64
	aggregate   string
	aggregateID string
	eventType   string
	payload     []byte
	attempts    int
}

func (w *Worker) tick(ctx context.Context) (int, error) {
	tx, err := w.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
SELECT id, aggregate, aggregate_id, event_type, payload, attempts
FROM outbox
WHERE processed_at IS NULL
  AND dead_at IS NULL
  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT $1`
	rows, err := tx.Query(ctx, q, w.batchSize)
	if err != nil {
		return 0, fmt.Errorf("select outbox: %w", err)
	}
	entries := make([]entry, 0, w.batchSize)
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.aggregate, &e.aggregateID, &e.eventType, &e.payload, &e.attempts); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox: %w", err)
		}
		entries = append(entries, e)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}

	processedIDs := make([]int64, 0, len(entries))
	for _, e := range entries {
		h, ok := w.handlers[e.aggregate]
		if !ok {
			// Нет хендлера — это либо аггрегат для другого инстанса воркера
			// (если будем шардировать), либо опечатка/мусор. Сейчас второе:
			// помечаем processed, чтобы не висело. Дополнительный лог даст
			// заметить, если такое посыплется.
			w.logger.Warn("outbox no handler", "aggregate", e.aggregate, "id", e.id)
			processedIDs = append(processedIDs, e.id)
			continue
		}
		hErr := h(ctx, e.aggregateID, e.eventType, e.payload)
		if hErr == nil {
			handlerSuccessTotal.WithLabelValues(e.aggregate, e.eventType).Inc()
			processedIDs = append(processedIDs, e.id)
			continue
		}
		// Shutdown — не наказываем счётчик. Запись останется как была
		// (attempts/next_attempt_at не двинутся), следующий запуск
		// подберёт её обычным путём.
		if errors.Is(hErr, context.Canceled) {
			return 0, hErr
		}
		handlerErrorsTotal.WithLabelValues(e.aggregate, e.eventType).Inc()

		nextAttempts := e.attempts + 1
		if nextAttempts >= w.maxAttempts {
			// DLQ: помечаем dead_at, чтобы воркер больше не трогал. Запись
			// останется в таблице для разбора (cleanup её не сносит).
			if _, uErr := tx.Exec(ctx, `
UPDATE outbox
SET attempts = $2, last_error = $3, dead_at = now(), next_attempt_at = NULL
WHERE id = $1`, e.id, nextAttempts, hErr.Error()); uErr != nil {
				return 0, fmt.Errorf("mark dead: %w", uErr)
			}
			deadTotal.WithLabelValues(e.aggregate, e.eventType).Inc()
			w.logger.Error("outbox dead-lettered",
				"aggregate", e.aggregate, "event", e.eventType,
				"aggregate_id", e.aggregateID, "outbox_id", e.id,
				"attempts", nextAttempts, "err", hErr)
			continue
		}

		next := time.Now().Add(w.backoffFor(nextAttempts))
		if _, uErr := tx.Exec(ctx, `
UPDATE outbox
SET attempts = $2, last_error = $3, next_attempt_at = $4
WHERE id = $1`, e.id, nextAttempts, hErr.Error(), next); uErr != nil {
			return 0, fmt.Errorf("mark retry: %w", uErr)
		}
		w.logger.Warn("outbox handler failed, scheduled retry",
			"aggregate", e.aggregate, "event", e.eventType,
			"aggregate_id", e.aggregateID, "outbox_id", e.id,
			"attempts", nextAttempts, "next_attempt_at", next, "err", hErr)
	}

	if len(processedIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox SET processed_at = now() WHERE id = ANY($1)`,
			processedIDs); err != nil {
			return 0, fmt.Errorf("mark processed: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(processedIDs), nil
}

// backoffFor — экспоненциальный backoff с потолком. attempts=1 → 2s,
// attempts=2 → 4s, ... attempts=9 → 512s, attempts≥10 → backoffCap.
func (w *Worker) backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// math.Pow возвращает float, кастуем безопасно — при больших attempts
	// поймает потолок до того, как float переполнится.
	secs := math.Pow(2, float64(attempts))
	d := time.Duration(secs) * time.Second
	if d > w.backoffCap || d <= 0 {
		return w.backoffCap
	}
	return d
}

func (w *Worker) runCleanup(ctx context.Context) {
	t := time.NewTicker(w.cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.cleanup(ctx)
			if err != nil {
				w.logger.Warn("outbox cleanup failed", "err", err)
				continue
			}
			if n > 0 {
				w.logger.Info("outbox cleanup", "deleted", n)
			}
		}
	}
}

func (w *Worker) cleanup(ctx context.Context) (int64, error) {
	// Чистим только processed-записи живых событий старше retention. Dead
	// не трогаем намеренно: их разбирают руками, потеря — потеря контекста
	// инцидента.
	tag, err := w.db.Exec(ctx, `
DELETE FROM outbox
WHERE processed_at IS NOT NULL
  AND processed_at < now() - make_interval(secs => $1)
  AND dead_at IS NULL`, w.retention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (w *Worker) runGaugeRefresh(ctx context.Context) {
	// Первый refresh — сразу, чтобы /metrics не отдавал нули до первого тика.
	w.refreshGauges(ctx)
	t := time.NewTicker(w.gaugeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.refreshGauges(ctx)
		}
	}
}

func (w *Worker) refreshGauges(ctx context.Context) {
	var pending, dead int64
	if err := w.db.QueryRow(ctx, `
SELECT
  (SELECT COUNT(*) FROM outbox WHERE processed_at IS NULL AND dead_at IS NULL),
  (SELECT COUNT(*) FROM outbox WHERE dead_at IS NOT NULL)
`).Scan(&pending, &dead); err != nil {
		if !errors.Is(err, context.Canceled) {
			w.logger.Warn("outbox refresh gauges", "err", err)
		}
		return
	}
	pendingGauge.Set(float64(pending))
	deadGauge.Set(float64(dead))
}

func DecodePayload(raw []byte, into any) error {
	return json.Unmarshal(raw, into)
}
