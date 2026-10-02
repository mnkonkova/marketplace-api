-- +goose Up
-- +goose StatementBegin

-- Какое сообщение бота к какому проекту относится.
--
-- Зачем. Человек отвечает боту на пинг «завтра выкладка · Проект» —
-- и ответ должен оказаться в переписке ЭТОГО проекта, а не потеряться
-- в личке бота, которую никто не читает. Telegram в ответе присылает
-- только reply_to_message_id; что это за сообщение, знаем лишь мы.
--
-- Хранится здесь, а не в боте: сервис бота без состояния намеренно
-- (см. deploy/telegram-bot/src/server.js). Своя память у бота означала
-- бы второй ответ на вопрос «чей это разговор» и потерю его на каждом
-- перезапуске контейнера Railway.
--
-- Строки появляются двумя путями:
--   • квитанция очереди: бот сообщает, под каким message_id ушло
--     каждое личное уведомление (bot_message_id заполнен);
--   • приглашение «Напишите комментарий в проект…» и подтверждение
--     «Записала в проект…» — бот регистрирует их сам, после проверки
--     доступа на нашей стороне (bot_message_id пуст).
CREATE TABLE bot_sent_messages (
    bot TEXT NOT NULL CHECK (bot IN ('creator', 'client')),
    -- В личке совпадает с tg_user_id, но ключ — чат: message_id
    -- уникален только внутри чата.
    chat_id BIGINT NOT NULL,
    tg_message_id BIGINT NOT NULL,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Кому писали. Ответ принимаем только от него же: чужая строка не
    -- должна давать писать в чужой проект.
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Строка очереди, из которой сообщение родилось. Без внешнего
    -- ключа: очередь чистится через две недели, а связь нужна дольше —
    -- для разбора «откуда взялся этот комментарий».
    bot_message_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bot, chat_id, tg_message_id)
);

-- Ищем только по первичному ключу: комментарием становится лишь ответ
-- на запомненное сообщение, проект «по последнему пингу» не угадываем.
-- Этот индекс — для ежедневной чистки старых строк (CleanupSent).
CREATE INDEX bot_sent_messages_created_idx
    ON bot_sent_messages (created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bot_sent_messages;
-- +goose StatementEnd
