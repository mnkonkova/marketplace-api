-- +goose Up
-- +goose StatementBegin

-- Бриф заказчика — первый шаг воронки «под ключ».
--
-- Отдельной таблицей, а не колонками в creator_orders: заказ — это
-- состояние сделки (статусы, деньги, даты), бриф — текст, который
-- правят. Половина заказчиков вспоминает про референсы уже после
-- «Отправить», и правка текста не должна трогать строку, по которой
-- считаются деньги.
CREATE TABLE order_briefs (
    order_id   UUID PRIMARY KEY REFERENCES creator_orders(id) ON DELETE CASCADE,
    -- Пять вопросов, на которые менеджер иначе идёт в переписку:
    -- что продвигаем, кому, каким тоном, на что равняться.
    goal       TEXT NOT NULL DEFAULT '',
    product    TEXT NOT NULL DEFAULT '',
    audience   TEXT NOT NULL DEFAULT '',
    tone       TEXT NOT NULL DEFAULT '',
    refs       TEXT NOT NULL DEFAULT '',
    -- Площадки, которые заказчик хочет. Пусто — все пять: так работает
    -- пакет по умолчанию, и заставлять отмечать их ради этого незачем.
    platforms  TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Отмеченные заказчиком: «хочу особенно этих».
--
-- Это НЕ приоритет очереди — очереди больше нет. Отметка уходит в текст
-- приглашения («вас хотят особенно») и встаёт звёздочкой в списке у
-- менеджера, когда он утверждает состав.
ALTER TABLE order_candidates
    ADD COLUMN is_preferred BOOLEAN NOT NULL DEFAULT FALSE;

-- Кому и когда ушла рассылка. NULL — не отправляли.
--
-- Отличать «не отправляли» от «отправили, молчит» обязательно: иначе
-- повторный прогон рассылки напишет человеку второй раз, а человек с
-- двумя одинаковыми приглашениями решает, что у нас беспорядок.
ALTER TABLE order_candidates
    ADD COLUMN broadcast_at TIMESTAMPTZ;

-- Приоритет перестаёт быть уникальным смыслом: в подборке заказчика
-- порядка нет, есть галочки. Колонку не удаляем — на неё смотрит код
-- сортировки и тесты, — но снимаем уникальный индекс: рассылка ВСЕМ
-- известным креаторам вставит десятки строк, и втиснуть их в уникальные
-- номера нечем.
DROP INDEX IF EXISTS order_candidates_priority_uniq;

-- Лимит «1..3 креатора» перестаёт быть обещанием клиенту: состав
-- утверждает менеджер, а заказчик отмечает, кого хочет. Верхнюю границу
-- оставляем санитарной: полсотни отметок — это уже не подборка.
ALTER TABLE creator_orders DROP CONSTRAINT IF EXISTS creator_orders_needed_positive;
ALTER TABLE creator_orders ADD CONSTRAINT creator_orders_needed_sane
    CHECK (needed BETWEEN 0 AND 50);

COMMENT ON COLUMN creator_orders.project_id IS
    'Проект заявки. Создаётся вместе с заказом; NULL только у строк, заведённых до 26.09.2026.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS order_briefs;
ALTER TABLE order_candidates DROP COLUMN IF EXISTS is_preferred;
ALTER TABLE order_candidates DROP COLUMN IF EXISTS broadcast_at;
ALTER TABLE creator_orders DROP CONSTRAINT IF EXISTS creator_orders_needed_sane;
ALTER TABLE creator_orders ADD CONSTRAINT creator_orders_needed_positive
    CHECK (needed >= 1 AND needed <= 3);
-- +goose StatementEnd
