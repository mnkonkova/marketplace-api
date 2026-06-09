-- +goose Up
-- Модерация публикации спецов админом. До одобрения профиль не попадает в
-- OpenSearch (поиск и feed). Подробности — docs/SPECIALIST_MODERATION.md.
--
-- moderation_status — конечный автомат:
--   pending_review — публикация запрошена, ждёт админа
--   approved       — админ одобрил, попадает в OS (если is_published=TRUE)
--   rejected       — админ отклонил с указанием причины
--
-- Эффективная видимость спеца в каталоге:
--   is_published = TRUE AND moderation_status = 'approved'
ALTER TABLE specialist_profiles
    ADD COLUMN moderation_status      TEXT        NOT NULL DEFAULT 'pending_review'
        CHECK (moderation_status IN ('pending_review', 'approved', 'rejected')),
    ADD COLUMN moderation_reason      TEXT,
    ADD COLUMN moderation_reviewed_at TIMESTAMPTZ,
    ADD COLUMN moderation_reviewed_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- Backfill: уже-published спецы на проде должны остаться в каталоге, иначе
-- после деплоя пропадут из поиска до admin'ского клика. moderation_reviewed_at
-- проставляем «сейчас» как маркер «была авто-апруврена при миграции»;
-- reviewed_by NULL — конкретного admin'a, который это сделал, нет.
UPDATE specialist_profiles
SET moderation_status      = 'approved',
    moderation_reviewed_at = now()
WHERE is_published = TRUE;

-- Партиальный индекс на очередь модерации: админ читает её через
--   SELECT ... WHERE moderation_status='pending_review' AND is_published=TRUE
--   ORDER BY updated_at
-- FIFO-сортировка по updated_at — фронт всегда «старые сверху».
-- Индекс крошечный (десятки-сотни записей в любое время).
CREATE INDEX specialist_profiles_pending_moderation_idx
    ON specialist_profiles(updated_at)
    WHERE moderation_status = 'pending_review' AND is_published = TRUE;

-- +goose Down
DROP INDEX IF EXISTS specialist_profiles_pending_moderation_idx;
ALTER TABLE specialist_profiles
    DROP COLUMN IF EXISTS moderation_reviewed_by,
    DROP COLUMN IF EXISTS moderation_reviewed_at,
    DROP COLUMN IF EXISTS moderation_reason,
    DROP COLUMN IF EXISTS moderation_status;
