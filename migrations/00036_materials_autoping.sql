-- +goose Up
-- +goose StatementBegin

-- Две вещи, которых не хватало менеджеру на странице проекта: материалы
-- с понятной аудиторией и выключатели автопинга.

-- 1. Кому виден материал.
--
-- В project_materials до сих пор лежали две разные по смыслу вещи:
-- обучение и бренд-гайд для креаторов («Клиент их не видит» — прямым
-- текстом в макете) и результат работы, приложенный к сдаче общего
-- проекта, который как раз для клиента и есть. Отличать их по
-- «delivery_id IS NULL» можно, но это правило нигде не записано и
-- первый же материал-бриф от клиента его сломает.
ALTER TABLE project_materials
    ADD COLUMN audience TEXT NOT NULL DEFAULT 'creators'
        CHECK (audience IN ('creators', 'client'));

-- Приложенное к сдаче — результат работы, он для заказчика.
UPDATE project_materials SET audience = 'client' WHERE delivery_id IS NOT NULL;

-- Индекс по (project_id, sort_order) уже есть с 00032; аудитория —
-- третье измерение выборки «материалы этого проекта для этих глаз».
CREATE INDEX project_materials_audience_idx
    ON project_materials(project_id, audience, sort_order);

-- 2. Автопинг: какие напоминания бот шлёт сам по этому проекту.
--
-- Четыре выключателя ровно по четырём видам напоминаний, которые уже
-- умеет планировщик (см. internal/publications/reminders.go). Отдельной
-- таблицей, а не колонками в projects: строка появляется только там, где
-- менеджер что-то выключил, а отсутствие строки означает «всё включено» —
-- это и есть поведение по умолчанию, и его не приходится дублировать
-- в каждом проекте.
CREATE TABLE project_reminder_prefs (
    project_id     UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    -- Креатору в бот утром в день выкладки.
    due_today      BOOLEAN NOT NULL DEFAULT TRUE,
    -- Креатору в бот на следующий день после просрочки и дальше раз в сутки.
    overdue        BOOLEAN NOT NULL DEFAULT TRUE,
    -- Креатору в бот: ролик вышел, но собраны не все пять ссылок.
    incomplete     BOOLEAN NOT NULL DEFAULT TRUE,
    -- Сводка в общий чат менеджеров. Креаторы этот чат не видят.
    manager_digest BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by     UUID REFERENCES users(id),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS project_reminder_prefs;

DROP INDEX IF EXISTS project_materials_audience_idx;
ALTER TABLE project_materials DROP COLUMN IF EXISTS audience;

-- +goose StatementEnd
