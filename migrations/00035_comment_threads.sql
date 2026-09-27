-- +goose Up
-- +goose StatementBegin

-- Переписка в проекте разъезжается на ветки, и появляется форматирование.
--
-- До этой миграции комментарий был один на проект, с единственным флагом
-- is_internal («клиенту не показывать»). Этого хватало, пока в проекте были
-- только клиент и менеджер. С креаторами схема ломается: креатор, попав в
-- проект, увидел бы переписку клиента с менеджером — то есть ровно то, что
-- в требованиях запрещено обеим сторонам.
--
-- Веток три:
--   client   — клиент ↔ менеджер. Креаторы не видят.
--   creator  — конкретный креатор ↔ менеджер. Ни клиент, ни другие
--              креаторы не видят, поэтому ветка адресная (thread_user_id).
--   internal — только менеджеры и админы, бывший is_internal.
-- Менеджер видит все три.

CREATE TYPE comment_thread AS ENUM ('client', 'creator', 'internal');

ALTER TABLE project_comments
    ADD COLUMN thread comment_thread NOT NULL DEFAULT 'client',
    -- thread_user_id — креатор, которому принадлежит ветка. Для остальных
    -- веток NULL: они одни на проект.
    ADD COLUMN thread_user_id UUID REFERENCES users(id),
    -- body_text — тот же текст без разметки. Нужен там, где HTML показывать
    -- нельзя или незачем: лента активности, payload в n8n, превью в боте.
    -- Считается при записи, а не при чтении: чтений сильно больше.
    ADD COLUMN body_text TEXT NOT NULL DEFAULT '';

UPDATE project_comments SET thread = 'internal' WHERE is_internal;
-- Существующие комментарии — plain, разметки в них нет.
UPDATE project_comments SET body_text = body WHERE body_text = '';

-- is_internal остаётся: на него смотрит уже написанный код и индекс из
-- 00013. Проверка держит две колонки в согласии, чтобы не появилось
-- «внутренний комментарий в клиентской ветке».
ALTER TABLE project_comments ADD CONSTRAINT project_comments_thread_internal
    CHECK ((thread = 'internal') = is_internal);

-- Креаторская ветка всегда адресная, остальные — никогда.
ALTER TABLE project_comments ADD CONSTRAINT project_comments_thread_addressed
    CHECK ((thread = 'creator') = (thread_user_id IS NOT NULL));

-- Выдача всегда идёт по одной ветке — индекс повторяет её форму.
-- NULLS NOT DISTINCT не нужен: thread_user_id входит в ключ как есть.
CREATE INDEX project_comments_thread_idx
    ON project_comments (project_id, thread, thread_user_id, created_at)
    WHERE deleted_at IS NULL;

-- Упоминания. Разбираются из размеченного тела при записи и проверяются по
-- участникам ветки: упомянуть можно только того, кто эту ветку и так видит.
-- Иначе @-меню превращается в способ дёрнуть любого пользователя площадки.
CREATE TABLE comment_mentions (
    comment_id UUID NOT NULL REFERENCES project_comments(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id),
    PRIMARY KEY (comment_id, user_id)
);

-- «Где меня упоминали» — выборка по пользователю.
CREATE INDEX comment_mentions_user_idx ON comment_mentions (user_id, comment_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS comment_mentions;

DROP INDEX IF EXISTS project_comments_thread_idx;
ALTER TABLE project_comments
    DROP CONSTRAINT IF EXISTS project_comments_thread_addressed,
    DROP CONSTRAINT IF EXISTS project_comments_thread_internal,
    DROP COLUMN IF EXISTS body_text,
    DROP COLUMN IF EXISTS thread_user_id,
    DROP COLUMN IF EXISTS thread;

DROP TYPE IF EXISTS comment_thread;

-- +goose StatementEnd
