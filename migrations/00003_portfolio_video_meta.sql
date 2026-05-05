-- +goose Up
-- +goose StatementBegin

ALTER TABLE portfolio_items
    ADD COLUMN kind         TEXT NOT NULL DEFAULT 'external'
                            CHECK (kind IN ('video', 'external', 'image')),
    ADD COLUMN duration_sec INTEGER,
    ADD COLUMN aspect       TEXT;

-- Уже залитым роликам ставим kind='video' — критерий: есть video_url.
UPDATE portfolio_items
   SET kind = 'video'
 WHERE video_url IS NOT NULL AND video_url <> '';

-- Лента берёт только опубликованные видео конкретного user_id, поэтому индекс
-- по (user_id, kind) ускоряет batch-load в /feed.
CREATE INDEX portfolio_items_user_kind_idx
    ON portfolio_items(user_id, kind);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS portfolio_items_user_kind_idx;

ALTER TABLE portfolio_items
    DROP COLUMN aspect,
    DROP COLUMN duration_sec,
    DROP COLUMN kind;

-- +goose StatementEnd
