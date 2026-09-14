-- +goose Up
-- +goose StatementBegin

-- Общий проект: клиент сам выбирает исполнителя, ставит один срок и ждёт
-- сдачу. Ни воронки, ни выкладок, ни статистики, ни менеджера — самый
-- короткий из трёх видов проекта.
--
-- Новой таблицы проектов здесь нет и не нужно: вид уже задан projects.kind,
-- исполнитель — specialist_user_id, срок — due_date (обе колонки из 00032).
-- Единственное, чего в ядре не было, — журнал сдач: кто что сдал, принял ли
-- клиент и сколько раз возвращал на доработку.

-- 1. Воронка перестаёт быть обязательной.
-- pipeline_id стоял NOT NULL с 00010, потому что проект без воронки тогда
-- не существовал. У общего вида воронки нет вовсе: materializePipeline для
-- него не вызывается, project_stages/project_steps остаются пустыми.
-- Обязательность для остальных двух видов сохраняем проверкой, а не
-- отсутствием её: иначе продакшн-проект можно было бы завести без воронки
-- и он молча остался бы без шагов.
ALTER TABLE projects ALTER COLUMN pipeline_id DROP NOT NULL;

ALTER TABLE projects ADD CONSTRAINT projects_pipeline_required
    CHECK (pipeline_id IS NOT NULL OR kind = 'general');

-- 2. Общий проект без исполнителя и срока смысла не имеет: он весь про
-- «кому» и «к какому числу». Проверка держит это на уровне БД, а не только
-- в валидации сервиса.
ALTER TABLE projects ADD CONSTRAINT projects_general_complete
    CHECK (kind <> 'general'
           OR (specialist_user_id IS NOT NULL AND due_date IS NOT NULL));

-- Выборка «мои общие проекты» у исполнителя и у клиента. Частичный индекс —
-- тем же приёмом, что projects_creators_idx в 00032.
CREATE INDEX projects_general_specialist_idx
    ON projects(specialist_user_id, due_date)
    WHERE kind = 'general';

-- 3. Журнал сдач.
-- pending — сдано, клиент ещё не ответил; accepted — принято, проект done;
-- rework — возвращено на доработку, исполнитель сдаёт следующей попыткой.
CREATE TYPE delivery_decision AS ENUM ('pending', 'accepted', 'rework');

CREATE TABLE general_deliveries (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- attempt — номер попытки, 1 для первой сдачи. Считается от числа
    -- возвратов, а не автоинкрементом: он должен быть виден в интерфейсе
    -- («правка 2 из 2») и сходиться с projects.revisions_used.
    attempt       INT  NOT NULL CHECK (attempt > 0),
    submitted_by  UUID NOT NULL REFERENCES users(id),
    submitted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    note          TEXT NOT NULL DEFAULT '',
    decision      delivery_decision NOT NULL DEFAULT 'pending',
    decided_by    UUID REFERENCES users(id),
    decided_at    TIMESTAMPTZ,
    rework_reason TEXT NOT NULL DEFAULT '',
    UNIQUE (project_id, attempt),
    -- Решение и его автор появляются вместе.
    CONSTRAINT general_deliveries_decided_together
        CHECK ((decision = 'pending') = (decided_at IS NULL)
               AND (decision = 'pending') = (decided_by IS NULL))
);

-- Незакрытая сдача в проекте ровно одна. Без этого исполнитель двойным
-- кликом заводит две «сданные» попытки, и «что именно принимает клиент»
-- становится вопросом без ответа.
CREATE UNIQUE INDEX general_deliveries_pending_uniq
    ON general_deliveries(project_id) WHERE decision = 'pending';

CREATE INDEX general_deliveries_project_idx
    ON general_deliveries(project_id, submitted_at DESC);

-- 4. Материал, приложенный к конкретной сдаче.
-- NULL — материал проекта вообще (бриф, референсы от клиента), так вели
-- себя все строки до этой миграции.
ALTER TABLE project_materials
    ADD COLUMN delivery_id UUID REFERENCES general_deliveries(id) ON DELETE CASCADE;

CREATE INDEX project_materials_delivery_idx
    ON project_materials(delivery_id) WHERE delivery_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS project_materials_delivery_idx;
ALTER TABLE project_materials DROP COLUMN IF EXISTS delivery_id;

DROP TABLE IF EXISTS general_deliveries;
DROP TYPE IF EXISTS delivery_decision;

DROP INDEX IF EXISTS projects_general_specialist_idx;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_general_complete;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_pipeline_required;

-- Вернуть NOT NULL можно только если общих проектов не осталось: у них
-- pipeline_id пуст по определению. Откат на живой базе с ними упадёт —
-- это лучше, чем молча удалить проекты.
ALTER TABLE projects ALTER COLUMN pipeline_id SET NOT NULL;

-- +goose StatementEnd
