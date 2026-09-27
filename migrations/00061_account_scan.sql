-- +goose Up
-- +goose StatementBegin

-- Расписание обхода аккаунтов креаторов.
--
-- Аккаунты уже лежат в project_accounts: креатор заводит их сам, и
-- именно там написано, где он выкладывает. Обходу не нужно ни новой
-- таблицы, ни нового ввода — нужна только очередь: кого и когда брать.
--
-- Колонки — зеркало publication_links.next_collect_at /
-- last_collected_at, и по той же причине. Обход стоит кредит у
-- поставщика, и «не чаще раза в сутки» должно держаться ВЫБОРКОЙ, а не
-- периодом тикера: иначе рестарт воркера (а при каждом деплое их
-- несколько секунд живёт два) обходил бы всё заново и жёг бюджет.
--
-- Разделение на две колонки существенно. next_scan_at двигается дважды:
-- коротким лизом в момент взятия строки в работу и на сутки вперёд
-- после ответа сервиса. last_scanned_at ставится только по факту
-- похода — по нему и проверяется суточное правило. Умри воркер посреди
-- пачки, лиз истечёт, строка вернётся в очередь, но повторно за сутки
-- в instacurl по ней не пойдут.
ALTER TABLE project_accounts
    ADD COLUMN IF NOT EXISTS last_scanned_at TIMESTAMPTZ,
    -- DEFAULT now(): уже заведённые аккаунты становятся в очередь сразу.
    -- Это безопасно ровно потому, что обход по умолчанию выключен
    -- (ACCOUNT_SCAN_ENABLED), а после включения суточное правило не даст
    -- взять их дважды.
    ADD COLUMN IF NOT EXISTS next_scan_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Очередь обхода. Частичный индекс: брендовые доступы (почта,
-- рекламный кабинет) владельца не имеют и обходить их нечего — в
-- очереди им делать нечего, и класть их в индекс тоже.
CREATE INDEX IF NOT EXISTS project_accounts_scan_idx
    ON project_accounts(next_scan_at)
    WHERE creator_user_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS project_accounts_scan_idx;
ALTER TABLE project_accounts
    DROP COLUMN IF EXISTS next_scan_at,
    DROP COLUMN IF EXISTS last_scanned_at;

-- +goose StatementEnd
