-- Вход через внешних провайдеров (Яндекс и далее).
--
-- Отдельная таблица, а не колонка yandex_id в users: следующий провайдер
-- добавится строкой, а не миграцией. Пара (provider, provider_id) —
-- первичный ключ, поэтому один аккаунт Яндекса не может оказаться привязан
-- к двум пользователям.

-- +goose Up
-- Пароля у OAuth-пользователя нет. Заглушка вида 'oauth' сюда не годится:
-- она выглядит как валидный хеш и однажды пройдёт проверку по ошибке.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

CREATE TABLE user_identities (
  provider    TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email       TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, provider_id)
);

CREATE INDEX idx_user_identities_user ON user_identities(user_id);

-- +goose Down
DROP TABLE user_identities;
UPDATE users SET password_hash = '' WHERE password_hash IS NULL;
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
