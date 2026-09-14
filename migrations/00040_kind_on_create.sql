-- +goose Up
-- +goose StatementBegin

-- Воронка обязательна только продакшну.
--
-- В 00034 я записал правило как «воронка нужна всем, кроме общего
-- проекта». Это неверно: по требованиям воронки со стадиями нет и у
-- креаторов под ключ — вместо неё выкладки. Пока проект этого вида
-- заводили правкой базы, ошибка не всплывала.
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_pipeline_required;

ALTER TABLE projects ADD CONSTRAINT projects_pipeline_required
    CHECK (pipeline_id IS NOT NULL OR kind <> 'production_turnkey');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_pipeline_required;

-- Возврат к прежнему правилу возможен, только если проектов с креаторами
-- без воронки не осталось: иначе откат молча оставил бы базу в состоянии,
-- которое сам же запрещает.
ALTER TABLE projects ADD CONSTRAINT projects_pipeline_required
    CHECK (pipeline_id IS NOT NULL OR kind = 'general');

-- +goose StatementEnd
