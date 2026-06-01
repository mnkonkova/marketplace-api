-- +goose Up
-- +goose StatementBegin

-- Контактные поля клиентов. Раньше client_name/contact_email/contact_phone
-- лежали в форме создания брифа (передавались сразу с заявкой). Теперь
-- клиент заполняет их один раз в своём кабинете /me, и при создании
-- проекта/брифа они подтягиваются автоматически.
--
-- Профиль клиента (отдельная таблица, чтобы не смешивать с
-- specialist_profiles которая семантически про витрину спецов).
CREATE TABLE client_profiles (
    user_id      UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    display_name TEXT NOT NULL DEFAULT '',
    phone        TEXT NOT NULL DEFAULT '',
    telegram     TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS client_profiles;

-- +goose StatementEnd
