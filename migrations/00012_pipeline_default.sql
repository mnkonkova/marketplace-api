-- +goose Up
-- +goose StatementBegin

-- Дефолтная воронка для брифов от клиентов: при POST /leads если бриф
-- идёт от авторизованного клиента, мы создаём из него проект автоматически
-- с этой воронкой и assigned_to=NULL → попадает в inbox менеджеров.

ALTER TABLE pipelines ADD COLUMN is_default BOOLEAN NOT NULL DEFAULT FALSE;

-- Только одна активная default воронка одновременно. Если is_default=FALSE,
-- индекс не действует — позволяет иметь много недефолтных.
CREATE UNIQUE INDEX pipelines_default_uniq ON pipelines(is_default)
    WHERE is_default = TRUE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS pipelines_default_uniq;
ALTER TABLE pipelines DROP COLUMN IF EXISTS is_default;

-- +goose StatementEnd
