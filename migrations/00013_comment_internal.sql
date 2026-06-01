-- +goose Up
ALTER TABLE project_comments
  ADD COLUMN is_internal BOOLEAN NOT NULL DEFAULT FALSE;

-- Индекс: при выдаче клиенту делаем `... AND NOT is_internal` — пусть планировщик
-- не идёт по всей таблице, когда внутренних комментариев накопится много.
CREATE INDEX idx_project_comments_visible
  ON project_comments (project_id, created_at)
  WHERE is_internal = FALSE AND deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_project_comments_visible;
ALTER TABLE project_comments DROP COLUMN IF EXISTS is_internal;
