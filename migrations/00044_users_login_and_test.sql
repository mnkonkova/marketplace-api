-- +goose Up
-- +goose StatementBegin

-- Когда человек последний раз заходил.
--
-- NULL здесь — не «давно», а «ни разу»: сотрудника завели, роль выдали,
-- а он так и не вошёл. Нулевой датой это не выразить — 1970 год читался
-- бы как вход, просто очень старый, и список команды врал бы ровно в том
-- случае, ради которого его открывают.
ALTER TABLE users
    ADD COLUMN last_login_at TIMESTAMPTZ;

-- Признак «пользователь заведён для проверки» — парный projects.is_test
-- (00043). Тестовых прячем из админских выдач по умолчанию: иначе счётчик
-- клиентов и список команды считают вместе с харнесс-пользователями,
-- которых после прогонов остаётся больше, чем настоящих.
ALTER TABLE users
    ADD COLUMN is_test BOOLEAN NOT NULL DEFAULT FALSE;

-- Листинг /admin/users и поиск идут по «не тестовым, новые сверху».
-- Частичный индекс отсекает тестовых и сразу даёт порядок.
CREATE INDEX users_admin_list_idx
    ON users (created_at DESC) WHERE is_test = FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS users_admin_list_idx;

ALTER TABLE users
    DROP COLUMN IF EXISTS is_test;

ALTER TABLE users
    DROP COLUMN IF EXISTS last_login_at;

-- +goose StatementEnd
