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

// R4: outboxID — выделенная уникальная по таблице outbox колонка id
// (BIGSERIAL). Используется handler'ами для построения уникального
// EventID при отправке во внешние системы (n8n). Раньше handler'ы
// строили EventID как aggregateID+"/"+eventType, и два события
// одного типа на один aggregate имели одинаковый EventID — n8n не
// мог их дедупнуть.
type Handler func(ctx context.Context, outboxID int64, aggregateID, eventType string, payload []byte) error

// ErrPermanent — sentinel для перманентных ошибок handler'a. Worker
// при errors.Is(hErr, ErrPermanent) НЕ ретраит и сразу переводит
// событие в DLQ (dead_at = now(), attempts++). Используется для
// 4xx-ответов внешних webhook'ов (n8n вернул 400/401/404 — повторы
// не помогут, нужно ручное разбирательство), битых payload-ов
// (JSON unmarshal failed) и т.п. P4.
var ErrPermanent = errors.New("permanent: do not retry")

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

// leaseDuration — на сколько вперёд сдвигаем next_attempt_at при "лизе"
// записи (P3). Должно быть больше суммарного handler-таймаута + запаса,
// иначе другой воркер подхватит запись, пока первый ещё её обрабатывает.
// 10 минут с запасом покрывают n8n webhook (10s × ~50 в батче) + любые
// побочные эффекты handler'a.
const leaseDuration = 10 * time.Minute

// tick: P3 — lease-and-release.
//   Phase 1 (короткая tx): SELECT FOR UPDATE SKIP LOCKED + UPDATE
//     next_attempt_at = now()+lease для этих id. COMMIT. Локов больше нет.
//   Phase 2 (без tx): для каждого entry запускаем handler. HTTP-вызовы
//     в n8n идут НЕ внутри PG-транзакции — autovacuum работает, bloat
//     не растёт, idle-in-tx не маячит.
//   Phase 3 (короткая tx): по результатам ставим processed_at /
//     next_attempt_at(backoff) / dead_at.
// Если воркер крашится между phase 2 и phase 3, записи "leased" до
// next_attempt_at в будущем — другой воркер подхватит их после истечения
// аренды. n8n идемпотентен по EventID (см. R4), повторов не страшно.
func (w *Worker) tick(ctx context.Context) (int, error) {
	// --- Phase 1: lease ---
	leaseTx, err := w.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin lease tx: %w", err)
	}
	const selectQ = `
SELECT id, aggregate, aggregate_id, event_type, payload, attempts
FROM outbox
WHERE processed_at IS NULL
  AND dead_at IS NULL
  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT $1`
	rows, err := leaseTx.Query(ctx, selectQ, w.batchSize)
	if err != nil {
		_ = leaseTx.Rollback(ctx)
		return 0, fmt.Errorf("select outbox: %w", err)
	}
	entries := make([]entry, 0, w.batchSize)
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.aggregate, &e.aggregateID, &e.eventType, &e.payload, &e.attempts); err != nil {
			rows.Close()
			_ = leaseTx.Rollback(ctx)
			return 0, fmt.Errorf("scan outbox: %w", err)
		}
		entries = append(entries, e)
	}
	rows.Close()
	if rows.Err() != nil {
		_ = leaseTx.Rollback(ctx)
		return 0, rows.Err()
	}
	if len(entries) == 0 {
		_ = leaseTx.Rollback(ctx)
		return 0, nil
	}
	leaseExpiry := time.Now().Add(leaseDuration)
	leaseIDs := make([]int64, 0, len(entries))
	for _, e := range entries {
		leaseIDs = append(leaseIDs, e.id)
	}
	if _, err := leaseTx.Exec(ctx,
		`UPDATE outbox SET next_attempt_at = $2 WHERE id = ANY($1)`,
		leaseIDs, leaseExpiry); err != nil {
		_ = leaseTx.Rollback(ctx)
		return 0, fmt.Errorf("lease: %w", err)
	}
	if err := leaseTx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit lease: %w", err)
	}

	// --- Phase 2: handlers вне tx ---
	type result struct {
		entry     entry
		hErr      error
		permanent bool
	}
	results := make([]result, 0, len(entries))
	for _, e := range entries {
		h, ok := w.handlers[e.aggregate]
		if !ok {
			w.logger.Warn("outbox no handler", "aggregate", e.aggregate, "id", e.id)
			// success-эквивалент: помечаем processed (см. ниже).
			results = append(results, result{entry: e})
			continue
		}
		hErr := h(ctx, e.id, e.aggregateID, e.eventType, e.payload)
		if errors.Is(hErr, context.Canceled) {
			// Shutdown посреди батча — оставляем оставшиеся записи "leased",
			// при следующем старте обработаются после истечения аренды.
			return 0, hErr
		}
		results = append(results, result{
			entry:     e,
			hErr:      hErr,
			permanent: hErr != nil && errors.Is(hErr, ErrPermanent),
		})
	}

	// --- Phase 3: маркировка результатов ---
	markTx, err := w.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin mark tx: %w", err)
	}
	defer func() { _ = markTx.Rollback(ctx) }()

	processedIDs := make([]int64, 0, len(results))
	for _, r := range results {
		e := r.entry
		if r.hErr == nil {
			handlerSuccessTotal.WithLabelValues(e.aggregate, e.eventType).Inc()
			processedIDs = append(processedIDs, e.id)
			continue
		}
		handlerErrorsTotal.WithLabelValues(e.aggregate, e.eventType).Inc()
		nextAttempts := e.attempts + 1
		if r.permanent || nextAttempts >= w.maxAttempts {
			if _, uErr := markTx.Exec(ctx, `
UPDATE outbox
SET attempts = $2, last_error = $3, dead_at = now(), next_attempt_at = NULL
WHERE id = $1`, e.id, nextAttempts, r.hErr.Error()); uErr != nil {
				return 0, fmt.Errorf("mark dead: %w", uErr)
			}
			deadTotal.WithLabelValues(e.aggregate, e.eventType).Inc()
			reason := "max_attempts"
			if r.permanent {
				reason = "permanent"
			}
			w.logger.Error("outbox dead-lettered",
				"aggregate", e.aggregate, "event", e.eventType,
				"aggregate_id", e.aggregateID, "outbox_id", e.id,
				"attempts", nextAttempts, "reason", reason, "err", r.hErr)
			continue
		}
		next := time.Now().Add(w.backoffFor(nextAttempts))
		if _, uErr := markTx.Exec(ctx, `
UPDATE outbox
SET attempts = $2, last_error = $3, next_attempt_at = $4
WHERE id = $1`, e.id, nextAttempts, r.hErr.Error(), next); uErr != nil {
			return 0, fmt.Errorf("mark retry: %w", uErr)
		}
		w.logger.Warn("outbox handler failed, scheduled retry",
			"aggregate", e.aggregate, "event", e.eventType,
			"aggregate_id", e.aggregateID, "outbox_id", e.id,
			"attempts", nextAttempts, "next_attempt_at", next, "err", r.hErr)
	}

	if len(processedIDs) > 0 {
		if _, err := markTx.Exec(ctx,
			`UPDATE outbox SET processed_at = now(), next_attempt_at = NULL WHERE id = ANY($1)`,
			processedIDs); err != nil {
			return 0, fmt.Errorf("mark processed: %w", err)
		}
	}

	if err := markTx.Commit(ctx); err != nil {
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
