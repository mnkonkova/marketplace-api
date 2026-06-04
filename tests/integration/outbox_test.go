package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
	"marketpclce/tests/integration"
)

// Тесты outbox-worker'а на реальной БД. Каждый тест чистит за собой
// outbox-строки (без FK к проектам — сами).

// quietLogger — slog без вывода (чтобы тесты не спамили stdout).
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// emitOne — служебная функция: открыть tx, Emit, commit.
func emitOne(t *testing.T, aggregate, aggregateID, eventType string, payload any) int64 {
	t.Helper()
	pool := integration.Pool(t)
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := outbox.Emit(ctx, tx, aggregate, aggregateID, eventType, payload); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM outbox WHERE aggregate=$1 AND aggregate_id=$2 AND event_type=$3 ORDER BY id DESC LIMIT 1`,
		aggregate, aggregateID, eventType).Scan(&id); err != nil {
		t.Fatalf("locate row: %v", err)
	}
	return id
}

func cleanupOutbox(t *testing.T, ids ...int64) {
	t.Helper()
	pool := integration.Pool(t)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM outbox WHERE id = ANY($1)`, ids); err != nil {
		t.Logf("cleanup outbox: %v", err)
	}
}

// ---- Emit ----

func TestEmit_InsertsRowWithPayload(t *testing.T) {
	id := emitOne(t, "test", uuid.NewString(), "test.kind", map[string]string{"k": "v"})
	defer cleanupOutbox(t, id)

	pool := integration.Pool(t)
	var aggregate, eventType string
	var payload []byte
	var processedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT aggregate, event_type, payload, processed_at FROM outbox WHERE id = $1`,
		id).Scan(&aggregate, &eventType, &payload, &processedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if aggregate != "test" || eventType != "test.kind" {
		t.Errorf("wrong fields: agg=%s, evt=%s", aggregate, eventType)
	}
	if processedAt != nil {
		t.Errorf("processed_at должен быть NULL у нового события")
	}
	var got map[string]string
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Errorf("payload не валидный JSON: %v", err)
	}
	if got["k"] != "v" {
		t.Errorf("payload: %+v", got)
	}
}

// ---- Worker tick: успех ----

func TestWorkerTick_SuccessMarksProcessed(t *testing.T) {
	pool := integration.Pool(t)
	id := emitOne(t, "test-success", uuid.NewString(), "ok", "v")
	defer cleanupOutbox(t, id)

	called := false
	handler := func(_ context.Context, _, _ string, _ []byte) error {
		called = true
		return nil
	}
	w := outbox.NewWorker(pool, quietLogger(),
		map[string]outbox.Handler{"test-success": handler},
		outbox.Config{},
	)

	// tick — приватный, но запустим один цикл через Run в фоне с быстрой отменой.
	// Альтернатива — вызвать через рефлексию; делаем proxy через Run+timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	if !called {
		t.Fatalf("handler не вызван")
	}
	var processedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT processed_at FROM outbox WHERE id = $1`, id).Scan(&processedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if processedAt == nil {
		t.Errorf("processed_at должен быть выставлен")
	}
}

// ---- Worker tick: error → retry ----

func TestWorkerTick_ErrorSchedulesRetry(t *testing.T) {
	pool := integration.Pool(t)
	id := emitOne(t, "test-retry", uuid.NewString(), "fail", "v")
	defer cleanupOutbox(t, id)

	handler := func(_ context.Context, _, _ string, _ []byte) error {
		return errors.New("transient")
	}
	w := outbox.NewWorker(pool, quietLogger(),
		map[string]outbox.Handler{"test-retry": handler},
		outbox.Config{MaxAttempts: 5},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	var attempts int
	var deadAt *time.Time
	var nextAttemptAt *time.Time
	var lastErr *string
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts, dead_at, next_attempt_at, last_error FROM outbox WHERE id = $1`,
		id).Scan(&attempts, &deadAt, &nextAttemptAt, &lastErr); err != nil {
		t.Fatalf("query: %v", err)
	}
	if attempts == 0 {
		t.Errorf("attempts должен увеличиться, got %d", attempts)
	}
	if deadAt != nil {
		t.Errorf("dead_at не должен быть выставлен до MaxAttempts")
	}
	if nextAttemptAt == nil {
		t.Errorf("next_attempt_at должен быть запланирован")
	}
	if lastErr == nil || *lastErr == "" {
		t.Errorf("last_error должен быть записан")
	}
}

// ---- Worker tick: dead-letter after MaxAttempts ----

func TestWorkerTick_DeadLetterAfterMaxAttempts(t *testing.T) {
	pool := integration.Pool(t)
	id := emitOne(t, "test-dead", uuid.NewString(), "always-fails", "v")
	defer cleanupOutbox(t, id)

	// Эмулируем что событие уже почти достигло лимита: attempts = MaxAttempts-1.
	// Тогда следующий tick перенесёт его в dead_at.
	if _, err := pool.Exec(context.Background(),
		`UPDATE outbox SET attempts = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("set attempts: %v", err)
	}

	handler := func(_ context.Context, _, _ string, _ []byte) error {
		return errors.New("nope")
	}
	w := outbox.NewWorker(pool, quietLogger(),
		map[string]outbox.Handler{"test-dead": handler},
		outbox.Config{MaxAttempts: 5},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	var deadAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT dead_at FROM outbox WHERE id = $1`, id).Scan(&deadAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if deadAt == nil {
		t.Errorf("dead_at должен быть установлен после MaxAttempts")
	}
}

// ---- Worker: нет хендлера → mark processed (warning) ----

func TestWorkerTick_NoHandlerMarksProcessed(t *testing.T) {
	pool := integration.Pool(t)
	id := emitOne(t, "no-handler-aggregate", uuid.NewString(), "x", "v")
	defer cleanupOutbox(t, id)

	w := outbox.NewWorker(pool, quietLogger(),
		map[string]outbox.Handler{ /* пусто */ },
		outbox.Config{},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	var processedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT processed_at FROM outbox WHERE id = $1`, id).Scan(&processedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if processedAt == nil {
		t.Errorf("событие без хендлера должно помечаться processed (иначе зависнет)")
	}
}

// ---- DecodePayload helper ----

func TestDecodePayload_Roundtrip(t *testing.T) {
	type p struct {
		X int    `json:"x"`
		Y string `json:"y"`
	}
	raw, _ := json.Marshal(p{X: 42, Y: "hi"})

	var got p
	if err := outbox.DecodePayload(raw, &got); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if got.X != 42 || got.Y != "hi" {
		t.Errorf("decoded: %+v", got)
	}
}

func TestDecodePayload_BadJSON_Error(t *testing.T) {
	var into map[string]string
	err := outbox.DecodePayload([]byte("{not json"), &into)
	if err == nil {
		t.Errorf("ожидаем ошибку на невалидный JSON")
	}
}
