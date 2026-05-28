-- +goose Up
-- +goose StatementBegin

-- Токены сброса пароля. Кладём только SHA-256 хеш самого токена — даже
-- админ с доступом к БД не сможет применить токен из таблицы; при
-- ConfirmPasswordReset бэк хеширует пришедший от пользователя token и
-- ищет по token_hash. TTL короткий (1 час), used_at гарантирует
-- одноразовое использование.
CREATE TABLE password_reset_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Активные (неиспользованные) токены пользователя — для возможной
-- инвалидации старых при выпуске нового и для cleanup-задач.
CREATE INDEX password_reset_tokens_user_active_idx
    ON password_reset_tokens(user_id) WHERE used_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS password_reset_tokens;

-- +goose StatementEnd
