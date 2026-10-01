-- +goose Up
-- +goose StatementBegin

-- Очередь сообщений для телеграм-ботов.
--
-- Зачем она появилась. Доставка была «мы стучимся к боту»: воркер
-- слал HTTP-запрос в сервис бота на Railway. Первого октября 2026 это
-- сломалось на проде — сервис спал, холодный старт шёл дольше нашего
-- таймаута, каждая из десяти попыток обрывалась на ожидании
-- заголовков, и событие ушло в мёртвую очередь. Лечится таймаутом, но
-- причина глубже: доставка зависела от того, дозвонится ли российская
-- ВДС до чужого облака. Железно это не гарантировать.
--
-- Обратное направление при этом работает всегда: сервис бота ходит в
-- наш API за привязкой и пользователями — на нём живёт мини-апп.
-- Поэтому стрелку переворачиваем: мы кладём сообщение сюда, бот
-- забирает его сам и квитирует. Дозваниваться до облака больше не
-- нужно никому.
CREATE TABLE bot_messages (
    id BIGSERIAL PRIMARY KEY,
    -- Какой из двух ботов говорит: с креатором или с заказчиком.
    bot TEXT NOT NULL CHECK (bot IN ('creator', 'client')),
    -- Кому адресовано: лично человеку или в общий чат менеджеров.
    -- Чат один и известен самому боту — список получателей у такого
    -- сообщения пуст, и это не ошибка.
    audience TEXT NOT NULL CHECK (audience IN ('person', 'managers')),
    event_type TEXT NOT NULL,
    -- Конверт целиком: payload события плюс посчитанные НАМИ
    -- получатели. Бот ничего не досчитывает — у него нет ни привязок,
    -- ни дневного потолка.
    envelope JSONB NOT NULL,
    -- id события в outbox. Ключ идемпотентности: outbox повторяет
    -- доставку при сбое, и повтор обязан не плодить вторую строку.
    event_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Выдано боту. Аренда, а не «удалено»: бот мог упасть между
    -- выдачей и отправкой, и сообщение должно вернуться в очередь.
    taken_at TIMESTAMPTZ,
    delivered_at TIMESTAMPTZ,
    attempts INT NOT NULL DEFAULT 0,
    last_error TEXT
);

-- Выдача: самые старые неотправленные. Частичный индекс — таблица
-- растёт доставленными, а читаем мы только хвост.
CREATE INDEX bot_messages_pending_idx ON bot_messages (id)
    WHERE delivered_at IS NULL;

-- Одно событие — одна строка. ON CONFLICT DO NOTHING на вставке
-- делает повтор outbox безвредным.
CREATE UNIQUE INDEX bot_messages_event_idx ON bot_messages (event_id)
    WHERE event_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bot_messages;
-- +goose StatementEnd
