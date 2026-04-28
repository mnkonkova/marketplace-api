package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Handler func(ctx context.Context, aggregateID, eventType string, payload []byte) error

type Worker struct {
	db           *pgxpool.Pool
	handlers     map[string]Handler
	batchSize    int
	pollInterval time.Duration
	idleBackoff  time.Duration
	logger       *slog.Logger
}

type Config struct {
	BatchSize    int
	PollInterval time.Duration
	IdleBackoff  time.Duration
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
	return &Worker{
		db:           db,
		handlers:     handlers,
		batchSize:    c.BatchSize,
		pollInterval: c.PollInterval,
		idleBackoff:  c.IdleBackoff,
		logger:       logger,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("outbox worker started", "batch", w.batchSize, "poll", w.pollInterval)
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
	id           int64
	aggregate    string
	aggregateID  string
	eventType    string
	payload      []byte
}

func (w *Worker) tick(ctx context.Context) (int, error) {
	tx, err := w.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
SELECT id, aggregate, aggregate_id, event_type, payload
FROM outbox
WHERE processed_at IS NULL
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
		if err := rows.Scan(&e.id, &e.aggregate, &e.aggregateID, &e.eventType, &e.payload); err != nil {
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
			w.logger.Warn("outbox no handler", "aggregate", e.aggregate, "id", e.id)
			processedIDs = append(processedIDs, e.id)
			continue
		}
		if err := h(ctx, e.aggregateID, e.eventType, e.payload); err != nil {
			w.logger.Error("outbox handler",
				"aggregate", e.aggregate, "event", e.eventType,
				"aggregate_id", e.aggregateID, "outbox_id", e.id, "err", err)
			continue
		}
		processedIDs = append(processedIDs, e.id)
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

func DecodePayload(raw []byte, into any) error {
	return json.Unmarshal(raw, into)
}
