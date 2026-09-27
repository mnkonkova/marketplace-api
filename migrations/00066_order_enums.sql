-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin

-- Новые состояния заказа и кандидата — ОТДЕЛЬНОЙ миграцией и без
-- транзакции. Правило Postgres: значение ENUM, добавленное в
-- транзакции, в ней же использовать нельзя. Положи рядом UPDATE ... SET
-- status = 'submitted' — и выкатка упадёт с «unsafe use of new value of
-- enum type», причём на проде, а не на тесте.
--
-- submitted — заявка отправлена: заказ и проект заведены, менеджер
--   считает. Это состояние появилось вместе с новой логикой «под ключ»:
--   раньше между «нажал отправить» и «оплачено» у заказа не было имени.
-- finalized — менеджер утвердил состав, цену и даты. Дальше живёт уже
--   проект, а заказ становится историей сделки.
-- responded — креатор откликнулся на рассылку: прислал файл или указал
--   свои ролики. Это НЕ «согласился»: согласие подтверждает менеджер,
--   когда добавляет человека в состав.
ALTER TYPE creator_order_status   ADD VALUE IF NOT EXISTS 'submitted';
ALTER TYPE creator_order_status   ADD VALUE IF NOT EXISTS 'finalized';
ALTER TYPE order_candidate_status ADD VALUE IF NOT EXISTS 'responded';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Значения ENUM в Postgres не удаляются. Откат — пересоздание типа со
-- всеми зависимостями; на этом объёме дешевле накатить вперёд.
SELECT 1;
-- +goose StatementEnd
