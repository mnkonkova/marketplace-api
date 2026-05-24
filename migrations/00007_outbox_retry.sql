-- +goose Up
-- +goose StatementBegin

-- Backoff / DLQ / TTL для outbox. До этой миграции воркер ретраил ядовитое
-- событие бесконечно с шагом pollInterval (≈ раз в 500мс), забивая логи и
-- блокируя продвижение очереди для того же aggregate'а. Теперь у каждой
-- записи есть attempts + next_attempt_at (экспоненциальный backoff) и
-- dead_at (карантин после MAX_ATTEMPTS — воркер их больше не трогает,
-- разбираем руками / алертом на outbox_dead).
ALTER TABLE outbox
    ADD COLUMN attempts        INT          NOT NULL DEFAULT 0,
    ADD COLUMN last_error      TEXT,
    ADD COLUMN next_attempt_at TIMESTAMPTZ,
    ADD COLUMN dead_at         TIMESTAMPTZ;

-- Старый partial-индекс заточен под "всё незахендленное". Новый фильтр шире:
-- надо ещё отсеять dead-записи и те, чьё next_attempt_at в будущем. Время
-- сравниваем в WHERE запроса (now() не immutable, в predicate индекса не лезет).
DROP INDEX IF EXISTS outbox_unprocessed_idx;
CREATE INDEX outbox_pending_idx ON outbox(id)
    WHERE processed_at IS NULL AND dead_at IS NULL;

-- Для дашборда / алертов: быстро посчитать сколько висит в DLQ.
CREATE INDEX outbox_dead_idx ON outbox(dead_at)
    WHERE dead_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS outbox_dead_idx;
DROP INDEX IF EXISTS outbox_pending_idx;
CREATE INDEX outbox_unprocessed_idx ON outbox(created_at) WHERE processed_at IS NULL;

ALTER TABLE outbox
    DROP COLUMN IF EXISTS dead_at,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS attempts;

-- +goose StatementEnd
