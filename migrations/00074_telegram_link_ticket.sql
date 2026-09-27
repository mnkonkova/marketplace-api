-- +goose Up
-- +goose StatementBegin

-- Билет привязки для человека, у которого аккаунта ещё НЕТ.
--
-- Регистрации в мини-аппе не будет (решение владельца от 27 сентября):
-- анкета с категориями и портфолио заполняется на сайте, а «я новый»
-- нажимали и те, у кого аккаунт давно есть, — телеграм привязывался к
-- пустому дублю, и разбирали это руками в базе.
--
-- Но заставлять человека после регистрации возвращаться в бот и
-- нажимать там что-то ещё — значит потерять половину: он уже получил,
-- зачем приходил. Поэтому мини-апп выдаёт билет, билет уезжает в
-- адрес анкеты, а браузер гасит его сразу после того, как аккаунт
-- появился.
--
-- Та же таблица, что у кодов из кабинета, и это не экономия: код и
-- билет — одно и то же по смыслу («предъявитель вправе привязать этот
-- телеграм»), различаются только тем, что известно на момент выдачи.
-- Из кабинета известен человек, из мини-аппа — телеграм.
ALTER TABLE telegram_link_codes ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE telegram_link_codes ADD COLUMN tg_user_id  BIGINT;
ALTER TABLE telegram_link_codes ADD COLUMN tg_chat_id  BIGINT;
ALTER TABLE telegram_link_codes ADD COLUMN tg_username TEXT NOT NULL DEFAULT '';

-- Пустая строка не бывает: без обеих сторон билет не привязывает
-- ничего и остаётся мусором, который однажды попробуют погасить.
ALTER TABLE telegram_link_codes ADD CONSTRAINT telegram_link_codes_side_known
    CHECK (user_id IS NOT NULL OR tg_user_id IS NOT NULL);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM telegram_link_codes WHERE user_id IS NULL;
ALTER TABLE telegram_link_codes DROP CONSTRAINT IF EXISTS telegram_link_codes_side_known;
ALTER TABLE telegram_link_codes DROP COLUMN IF EXISTS tg_username;
ALTER TABLE telegram_link_codes DROP COLUMN IF EXISTS tg_chat_id;
ALTER TABLE telegram_link_codes DROP COLUMN IF EXISTS tg_user_id;
ALTER TABLE telegram_link_codes ALTER COLUMN user_id SET NOT NULL;
-- +goose StatementEnd
