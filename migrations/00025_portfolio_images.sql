-- +goose Up
-- +goose StatementBegin

-- portfolio_images — N изображений на один portfolio_items.id (kind='image').
-- Один photo-set = одна карточка в ленте с горизонтальной каруселью фото.
-- На уровне БД дублируем kind='image' в portfolio_items + N строк здесь;
-- видео-айтемы (kind='video') в эту таблицу не пишем.
--
-- Решение в пользу child-таблицы (а не jsonb-массива в portfolio_items):
--   • удобный ORDER BY sort_order при батч-загрузке для ленты
--   • DELETE отдельных фото без переписи родителя
--   • будущие per-image поля (width/height/alt) живут естественно
CREATE TABLE portfolio_images (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    portfolio_item_id UUID NOT NULL REFERENCES portfolio_items(id) ON DELETE CASCADE,
    image_url         TEXT NOT NULL,
    sort_order        INTEGER NOT NULL DEFAULT 0,
    width             INTEGER,
    height            INTEGER,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Покрывающий индекс под батч-загрузку: WHERE portfolio_item_id IN (...)
-- ORDER BY sort_order. Используется в ленте и на странице спеца.
CREATE INDEX portfolio_images_item_idx
    ON portfolio_images(portfolio_item_id, sort_order);

-- Аналог portfolio_items_videos_idx для photo-сетов: лента грузит
-- последние kind='image' конкретного спеца.
CREATE INDEX portfolio_items_photos_idx
    ON portfolio_items(user_id, sort_order, created_at DESC)
    WHERE kind = 'image';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS portfolio_items_photos_idx;
DROP INDEX IF EXISTS portfolio_images_item_idx;
DROP TABLE IF EXISTS portfolio_images;

-- +goose StatementEnd
