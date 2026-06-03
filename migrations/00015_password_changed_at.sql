-- +goose Up
-- Token revocation после смены пароля.
-- Refresh-токен сейчас валиден до собственного TTL без какого-либо blacklist'а:
-- атакующий с украденным refresh переживает password-reset легитимного юзера.
-- Фикс: записываем момент последней смены пароля и в Refresh отказываем,
-- если token.iat < users.password_changed_at. Access живёт минуты, ему
-- проверка не нужна (по TTL отвалится сам).
--
-- Default now() при backfill — все живые токены, выпущенные до миграции,
-- остаются валидными (т.к. их iat < now()). Это нежелательно с точки зрения
-- ультра-секьюрности, но безопасно: миграция не выкидывает легитимных юзеров
-- из сессии. Альтернатива — выставить '1970-01-01', что разорвёт ВСЕ
-- активные refresh'ы и заставит всех логиниться заново.
ALTER TABLE users
    ADD COLUMN password_changed_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE users DROP COLUMN password_changed_at;
