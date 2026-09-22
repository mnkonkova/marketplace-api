-- +goose Up
-- +goose StatementBegin

-- Доступы к аккаунтам бренда: где и под каким логином выходят ролики.
--
-- Заполняет менеджер, видит заказчик — это его аккаунты, и без них он
-- не может ни проверить, ни забрать их обратно. Креаторам эта таблица не
-- отдаётся: они публикуют со своих аккаунтов, а не с брендовых.
--
-- Пароль лежит ЗАШИФРОВАННЫМ (AES-256-GCM, ключ в env ACCOUNTS_SECRET_KEY),
-- а не текстом: дамп базы и бэкап уезжают в места, где чужие пароли от
-- соцсетей быть не должны. Ключ в базе не хранится намеренно — иначе
-- шифрование не отличается от хранения текстом. Нет ключа в окружении —
-- ручки отвечают 501, и это честнее тихого хранения в открытую.
CREATE TABLE project_accounts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- platform — та же пятёрка, что у выкладок, плюс other: доступы
    -- бывают и к почте, и к рекламному кабинету.
    platform   TEXT NOT NULL
        CHECK (platform IN ('tiktok', 'instagram', 'youtube', 'vk', 'likee', 'other')),
    -- title — как этот аккаунт называют люди: «основной», «второй, для
    -- тестов». Пусто — показываем площадку.
    title      TEXT NOT NULL DEFAULT '',
    -- url — ссылка на профиль. Именно она и нужна чаще пароля.
    url        TEXT NOT NULL DEFAULT '',
    login      TEXT NOT NULL DEFAULT '',
    -- secret_enc — nonce||ciphertext. NULL = пароль не заведён: это не
    -- то же самое, что пустая строка, и в ответе поле has_password.
    secret_enc BYTEA,
    note       TEXT NOT NULL DEFAULT '',
    sort_order INT NOT NULL DEFAULT 0,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_accounts_project_idx ON project_accounts(project_id, sort_order);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_accounts;
-- +goose StatementEnd
