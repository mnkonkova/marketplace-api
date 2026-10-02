-- +goose Up
-- +goose StatementBegin

-- Документы: шаблоны с версиями и документы конкретному человеку.
--
-- До сих пор документ был материалом проекта: ссылка с видом «договор»,
-- видная всему составу сразу. Адресовать документ одному человеку было
-- нельзя — договор с суммой одного креатора видели все, — а удаление
-- материала стирало его бесследно: потом не узнать, что человеку
-- выдавали.
--
-- Хранение — как у проектов и прайса: ничего не удаляется. Шаблон
-- уходит в архив и возвращается, версия шаблона неизменна (правка —
-- новая версия), выданный документ отзывается, а не стирается. Тяжёлого
-- здесь нет — только ссылки, — поэтому и чистить со временем нечего.

-- Шаблон документа: «Договор с креатором», «Акт», «NDA». Сам текст
-- живёт в версиях.
CREATE TABLE document_templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind        TEXT NOT NULL CHECK (kind IN ('contract', 'act', 'nda', 'other')),
    title       TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    -- Кому шаблон выдают: креаторам или заказчику. Один шаблон — одна
    -- сторона: договор с креатором заказчику не выдаётся и наоборот.
    audience    TEXT NOT NULL CHECK (audience IN ('creators', 'client')),
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Архив вместо удаления: из выбора у менеджера уходит, история и
    -- выданные по нему документы остаются.
    archived_at TIMESTAMPTZ,
    archived_by UUID REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX document_templates_active_idx
    ON document_templates (audience, title) WHERE archived_at IS NULL;

-- Версия шаблона. Неизменна, как версия прайса: на неё ссылаются
-- выданные документы, и переписать её задним числом значило бы
-- поменять то, что человеку уже отдали.
CREATE TABLE document_template_versions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id  UUID NOT NULL REFERENCES document_templates(id) ON DELETE RESTRICT,
    version      INT  NOT NULL CHECK (version > 0),
    url          TEXT NOT NULL CHECK (length(url) BETWEEN 1 AND 2000),
    -- Что изменилось относительно прошлой версии — для истории в
    -- админке, человеку не показывается.
    note         TEXT NOT NULL DEFAULT '',
    published_by UUID REFERENCES users(id) ON DELETE SET NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (template_id, version)
);

-- Документ конкретному человеку: креатору проекта или заказчику.
--
-- Отдельная таблица, а не колонка «кому» в материалах: материалы — это
-- задание всему составу, документ — условия одного человека. Проект
-- обязателен: выдаёт менеджер своего проекта, и доступ проверяется по
-- нему.
CREATE TABLE user_documents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id          UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Кем адресат приходится проекту — по нему выбирается бот и экран.
    audience            TEXT NOT NULL CHECK (audience IN ('creators', 'client')),
    -- Из какой версии шаблона выдан. Пусто — менеджер приложил свою
    -- ссылку. Новая версия шаблона выданное не трогает.
    template_version_id UUID REFERENCES document_template_versions(id) ON DELETE RESTRICT,
    kind                TEXT NOT NULL CHECK (kind IN ('contract', 'act', 'nda', 'other')),
    title               TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    url                 TEXT NOT NULL CHECK (length(url) BETWEEN 1 AND 2000),
    -- Пояснение менеджера к выдаче: «подпишите и пришлите в комментарии».
    note                TEXT NOT NULL DEFAULT '',
    sent_by             UUID REFERENCES users(id) ON DELETE SET NULL,
    sent_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Когда адресат впервые открыл. Ответ — подписанный экземпляр,
    -- вопрос — он пишет в комментарии проекта; «открыл» нужно, чтобы
    -- менеджер видел, что документ дошёл.
    opened_at           TIMESTAMPTZ,
    -- Отзыв вместо удаления: у адресата пропадает, у менеджера остаётся
    -- в истории выдачи.
    revoked_at          TIMESTAMPTZ,
    revoked_by          UUID REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX user_documents_recipient_idx
    ON user_documents (recipient_user_id, sent_at DESC) WHERE revoked_at IS NULL;
CREATE INDEX user_documents_project_idx
    ON user_documents (project_id, sent_at DESC);

-- Материалы проекта — мягким удалением, как всё остальное. Убранный из
-- проекта договор больше не стирается бесследно.
ALTER TABLE project_materials
    ADD COLUMN deleted_at TIMESTAMPTZ,
    ADD COLUMN deleted_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE project_materials DROP COLUMN IF EXISTS deleted_by;
ALTER TABLE project_materials DROP COLUMN IF EXISTS deleted_at;
DROP TABLE IF EXISTS user_documents;
DROP TABLE IF EXISTS document_template_versions;
DROP TABLE IF EXISTS document_templates;

-- +goose StatementEnd
