-- +goose Up
-- +goose StatementBegin

-- Индексы под запросы, которые до сих пор шли перебором.
--
-- Все четыре — условия в WHERE или JOIN на горячих путях. Пока таблицы
-- были маленькими, перебор стоил дёшево; на объёмах живой площадки
-- (семьдесят проектов, у каждого тысячи ссылок и сотни тысяч замеров)
-- он стал заметен.

-- MoveProjectToStage и AdvanceStage ходят по шагам стадии в цикле, и
-- каждый заход — `WHERE stage_id = $1` внутри транзакции с FOR UPDATE:
-- перебор под блокировкой держит её дольше, чем нужно.
CREATE INDEX IF NOT EXISTS project_steps_stage_idx
    ON project_steps (stage_id);

-- Обход аккаунтов ищет по каноническому адресу: нашёл ролик на
-- площадке — проверь, не сдан ли он уже ссылкой. Без индекса это
-- полный скан всех ссылок площадки на КАЖДЫЙ найденный ролик.
CREATE INDEX IF NOT EXISTS publication_links_canonical_idx
    ON publication_links (url_canonical);

-- Отметки чеклиста читают по пункту (какие выкладки его отметили).
-- item_id — вторая колонка первичного ключа, то есть для поиска по
-- ней индекса нет.
CREATE INDEX IF NOT EXISTS publication_checklist_marks_item_idx
    ON publication_checklist_marks (item_id);

-- Начисления джойнят заказы по проекту: без индекса это построение
-- хеша по всей таблице заказов на каждое открытие денег проекта.
CREATE INDEX IF NOT EXISTS creator_orders_project_idx
    ON creator_orders (project_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS creator_orders_project_idx;
DROP INDEX IF EXISTS publication_checklist_marks_item_idx;
DROP INDEX IF EXISTS publication_links_canonical_idx;
DROP INDEX IF EXISTS project_steps_stage_idx;
-- +goose StatementEnd
