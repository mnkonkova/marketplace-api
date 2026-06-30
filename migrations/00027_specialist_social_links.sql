-- +goose Up
-- +goose StatementBegin

-- social_links — JSONB с ссылками на соцсети спеца: telegram/whatsapp/vk/
-- youtube/instagram/tiktok/behance/dribbble/website. Структура свободная
-- ("key": "url-or-handle"); фронт знает фиксированный список ключей и
-- рендерит иконки. JSONB вместо колонок — добавлять новые сети без миграций.
-- DEFAULT '{}' и NOT NULL чтобы не разбираться с NULL'ами в UPDATE'ах.
ALTER TABLE specialist_profiles
    ADD COLUMN social_links JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE specialist_profiles DROP COLUMN IF EXISTS social_links;

-- +goose StatementEnd
