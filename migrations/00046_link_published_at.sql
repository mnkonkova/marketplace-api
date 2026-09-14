-- +goose Up
-- +goose StatementBegin

-- Когда ролик вышел на площадке.
--
-- Дата приходит от сборщика статистики на каждом обходе (PostMetrics.
-- PublishedAt) и до сих пор выбрасывалась: в базе её не было вовсе.
-- Без неё нельзя ответить на вопрос «сколько ролику дней», а на нём
-- стоит всё дальнейшее — зрелость ролика, окно в 14 дней, сравнение
-- роликов между собой.
--
-- NULL означает «не знаем»: площадка не отдала дату или ролик ещё не
-- собирали. Нулевой датой это выражать нельзя — «вышел в 1970-м» и
-- «неизвестно» разные вещи, и первое молча испортит любой расчёт
-- возраста.
ALTER TABLE publication_links
    ADD COLUMN published_at TIMESTAMPTZ;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE publication_links
    DROP COLUMN IF EXISTS published_at;

-- +goose StatementEnd
