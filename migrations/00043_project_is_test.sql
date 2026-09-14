-- +goose Up
-- +goose StatementBegin

-- Признак «проект заведён для проверки».
--
-- Половина админского списка — «тест т8т 1234»: строки, оставшиеся от
-- прогонов на стенде. Отличить их от настоящих можно только по названию,
-- то есть на глаз и каждый раз заново — а именно в этом списке ищут, что
-- встало и кто за это отвечает.
--
-- Признак ставит тот, кто заводит проект: вывести его из данных нельзя.
-- По клиенту-сотруднику не получится (тестовый проект часто вешают на
-- настоящего заказчика, а сотрудник иногда и есть заказчик), по названию
-- тем более — «тест» в названии пишут не всегда.
ALTER TABLE projects
    ADD COLUMN is_test BOOLEAN NOT NULL DEFAULT FALSE;

-- Запрос админского списка по умолчанию — «не тестовые, самые давние
-- сверху». Без индекса это seq scan + сортировка всей таблицы на каждую
-- страницу; частичный индекс отсекает тестовые и сразу даёт порядок.
CREATE INDEX projects_admin_list_idx
    ON projects (updated_at) WHERE is_test = FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS projects_admin_list_idx;

ALTER TABLE projects
    DROP COLUMN IF EXISTS is_test;

-- +goose StatementEnd
