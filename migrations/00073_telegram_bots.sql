-- +goose Up
-- +goose StatementBegin

-- Привязка человека к боту. Ботов два: у исполнителя и у заказчика
-- разные разговоры, и одна строка не может обслуживать оба.
--
-- Третьего значения, 'manager', здесь нет намеренно (решение владельца
-- от 25 сентября): пинги, адресованные менеджеру, уходят в общий чат
-- менеджеров через CRM-вебхук, а не в личку. CHECK, разрешающий то,
-- чего в продукте нет, — это приглашение написать привязку, которую
-- никто не читает.
--
-- Одна таблица с колонкой bot, а не две: доставка — это один запрос
-- «чат этого человека в этом боте»; с двумя таблицами он превращается
-- в два и в ветку if на каждом вызове.
--
-- Заменяет bot_links из 00032: та заводилась под одного бота и не
-- использовалась ни одной строкой кода.
CREATE TABLE telegram_links (
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bot         TEXT NOT NULL CHECK (bot IN ('creator', 'client')),
    -- id Telegram давно за 32 бита.
    tg_user_id  BIGINT NOT NULL,
    -- В личке совпадает с tg_user_id, но это свойство, а не правило:
    -- писать надо в чат, а узнавать человека — по user_id.
    tg_chat_id  BIGINT NOT NULL,
    tg_username TEXT NOT NULL DEFAULT '',
    linked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Бот заблокирован пользователем. Строку НЕ удаляем: удаление
    -- выглядит как «никогда не подключал», и мы бы звали его заново.
    blocked_at  TIMESTAMPTZ,
    PRIMARY KEY (bot, tg_user_id),
    UNIQUE (bot, user_id)
);
-- Отбор получателей: «кому из этих людей можно написать в этот бот».
CREATE INDEX telegram_links_user_idx
    ON telegram_links(user_id, bot) WHERE blocked_at IS NULL;

DROP TABLE IF EXISTS bot_links;

-- Одноразовый код привязки из кабинета. Хранится хешем, как инвайты и
-- ссылки входа: дамп базы не должен давать привязать чужой аккаунт.
CREATE TABLE telegram_link_codes (
    code_hash  TEXT PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bot        TEXT NOT NULL CHECK (bot IN ('creator', 'client')),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX telegram_link_codes_user_idx ON telegram_link_codes(user_id, created_at DESC);

-- Человек из Telegram не приносит ни почты, ни телефона.
--
-- Старое правило «email ИЛИ phone» не про почту — оно про то, что
-- человека должно быть чем опознать. Telegram опознаёт не хуже: id там
-- вечный и подделать его нельзя, подпись initData проверяется нашим же
-- токеном бота.
ALTER TABLE users ADD COLUMN telegram_user_id BIGINT UNIQUE;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_check;
ALTER TABLE users ADD CONSTRAINT users_contact_present
    CHECK (email IS NOT NULL OR phone IS NOT NULL OR telegram_user_id IS NOT NULL);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS telegram_link_codes;
DROP TABLE IF EXISTS telegram_links;
-- Вернуть старое правило можно только там, где нет людей, опознанных
-- одним телеграмом: у них ни почты, ни телефона, и CHECK их отвергнет.
DELETE FROM users WHERE email IS NULL AND phone IS NULL AND telegram_user_id IS NOT NULL;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_contact_present;
ALTER TABLE users DROP COLUMN IF EXISTS telegram_user_id;
ALTER TABLE users ADD CONSTRAINT users_check
    CHECK (email IS NOT NULL OR phone IS NOT NULL);
-- bot_links не восстанавливаем: её не читала ни одна строка кода.
-- +goose StatementEnd
