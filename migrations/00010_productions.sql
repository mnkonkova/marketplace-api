-- +goose Up
-- +goose StatementBegin

-- productions — справочник студий/продакшенов, ведёт админ платформы через
-- Directus. Специалист выбирает свой продакшен из активных. Деактивация
-- (is_active=false) — soft-delete: запись остаётся, ссылки в specialist_profiles
-- продолжают работать, но в публичный /productions запись не попадает.
CREATE TABLE productions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Имя уникально среди активных (case-insensitive). После soft-delete можно
-- завести новую запись с тем же именем — индекс частичный по is_active.
CREATE UNIQUE INDEX productions_active_name_idx
    ON productions(LOWER(name)) WHERE is_active = TRUE;

-- Привязка специалиста к продакшену + флаг фриланса. CHECK гарантирует
-- взаимоисключение: либо специалист в продакшене, либо фрилансер, либо
-- ничего (вариант «пока не выбрал»). Двойная защита на уровне сервиса
-- даёт понятный 400 вместо 500 от констрейнта.
ALTER TABLE specialist_profiles
    ADD COLUMN production_id UUID NULL REFERENCES productions(id) ON DELETE SET NULL,
    ADD COLUMN is_freelance  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT specialist_profiles_freelance_xor_production
        CHECK (NOT (production_id IS NOT NULL AND is_freelance = TRUE));

CREATE INDEX specialist_profiles_production_idx
    ON specialist_profiles(production_id) WHERE production_id IS NOT NULL;

-- Авторизационная роль. Отдельно от users.kind (=тип аккаунта): kind
-- остаётся client/specialist/both, role — client/specialist/admin/moderator.
-- Платформенные роли (admin/moderator) проверяются в RequireAdminOrModerator.
ALTER TABLE users
    ADD COLUMN role TEXT NOT NULL DEFAULT 'client'
        CHECK (role IN ('client', 'specialist', 'admin', 'moderator'));

-- Service-account для Directus. Логин через UI невозможен (dummy-хеш),
-- система обращается к API под service-token (см. internal/auth/tokens.go,
-- TokenService). На users этой строки выставлен role='admin', чтобы при
-- генерации service-токена выдача role в claims была согласована.
INSERT INTO users (email, password_hash, kind, role, email_verified_at)
VALUES (
    'directus-service@platform.internal',
    '!service-account-no-password-login!',
    'client',
    'admin',
    now()
) ON CONFLICT (email) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM users WHERE email = 'directus-service@platform.internal';

ALTER TABLE users DROP COLUMN IF EXISTS role;

DROP INDEX IF EXISTS specialist_profiles_production_idx;
ALTER TABLE specialist_profiles
    DROP CONSTRAINT IF EXISTS specialist_profiles_freelance_xor_production,
    DROP COLUMN IF EXISTS is_freelance,
    DROP COLUMN IF EXISTS production_id;

DROP INDEX IF EXISTS productions_active_name_idx;
DROP TABLE IF EXISTS productions;

-- +goose StatementEnd
