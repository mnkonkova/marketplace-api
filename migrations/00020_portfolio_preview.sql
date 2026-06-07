-- +goose Up
-- Preview-видео для portfolio_items: маленький 480p MP4 (5-10 сек, ~500KB),
-- который фид крутит в карточке вместо оригинала (~30-50 МБ). Подробности
-- pipeline'a — в docs/VIDEO_TRANSCODING.md.
--
-- preview_status — конечный автомат:
--   pending    — запись создана, worker не подобрал
--   processing — worker взял в работу (lease, P3)
--   ready      — preview готов, preview_url заполнен
--   failed     — все попытки исчерпаны (см. preview_error)
--
-- Все три новых URL-поля nullable: на момент миграции у существующих
-- видео preview ещё нет, фронт показывает оригинал в degraded режиме.
ALTER TABLE portfolio_items
    ADD COLUMN preview_url          TEXT,
    ADD COLUMN preview_status       TEXT NOT NULL DEFAULT 'pending'
        CHECK (preview_status IN ('pending', 'processing', 'ready', 'failed')),
    ADD COLUMN preview_error        TEXT,
    ADD COLUMN preview_generated_at TIMESTAMPTZ;

-- Партиальный индекс для backfill/reconciliation: воркер периодически
-- может сканировать все pending'и (если outbox-событие потерялось).
-- Filter сделан узким — на проде индекс крошечный (десятки записей).
CREATE INDEX portfolio_items_pending_preview_idx
    ON portfolio_items(created_at)
    WHERE preview_status = 'pending';

-- +goose Down
DROP INDEX IF EXISTS portfolio_items_pending_preview_idx;
ALTER TABLE portfolio_items
    DROP COLUMN IF EXISTS preview_generated_at,
    DROP COLUMN IF EXISTS preview_error,
    DROP COLUMN IF EXISTS preview_status,
    DROP COLUMN IF EXISTS preview_url;
