-- +goose Up
-- +goose StatementBegin

-- Закреплённая («промо») работа специалиста — рендерится флагманом сверху
-- публичной страницы /specialist/<handle>. До этой миграции поля не было
-- вообще: фронт мог только угадывать промо по sort_order, то есть выбор
-- был не за специалистом.
ALTER TABLE portfolio_items
    ADD COLUMN is_featured BOOLEAN NOT NULL DEFAULT FALSE;

-- Ровно одна закреплённая работа на специалиста. Partial unique: строки с
-- is_featured=FALSE в индекс не попадают, поэтому он остаётся крошечным
-- (≤1 запись на юзера). Заодно это защита от гонки — два параллельных
-- PUT /me/portfolio/{id}/featured не смогут оставить два флагмана,
-- второй словит unique_violation вместо тихой порчи данных.
CREATE UNIQUE INDEX portfolio_items_one_featured_idx
    ON portfolio_items(user_id)
 WHERE is_featured;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS portfolio_items_one_featured_idx;

ALTER TABLE portfolio_items
    DROP COLUMN is_featured;

-- +goose StatementEnd
