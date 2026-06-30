-- +goose Up
-- +goose StatementBegin

-- username — публичный handle спеца для красивых URL'ов вида /specialist/foxy
-- вместо /specialist/<uuid>. Опционально (NULL = ещё не выбрал, фронт берёт
-- fallback на uuid). После выбора менять можно, но это сломает разосланные
-- ссылки — фронт об этом предупредит.
--
-- Хранение: LOWERCASE в БД через CHECK constraint. UNIQUE для prevent
-- collision. Длина 3-30 — баланс «достаточно для имени» vs «не URL-bomb».
-- Allowed chars: a-z, 0-9, _, - (URL-safe без encoding'а).
ALTER TABLE specialist_profiles
    ADD COLUMN username TEXT
        CHECK (
            username IS NULL
            OR (
                length(username) BETWEEN 3 AND 30
                AND username ~ '^[a-z0-9_-]+$'
            )
        );

CREATE UNIQUE INDEX specialist_profiles_username_idx
    ON specialist_profiles (username)
    WHERE username IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS specialist_profiles_username_idx;
ALTER TABLE specialist_profiles DROP COLUMN IF EXISTS username;

-- +goose StatementEnd
