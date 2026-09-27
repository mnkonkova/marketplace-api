-- +goose Up
-- +goose StatementBegin

-- 1. Выкладка без креатора.
--
-- creator_user_id заводился NOT NULL (00032:88) потому, что выкладка
-- была поручением человеку: менеджер задавал КОГДА, а снимал конкретный
-- креатор. У проекта без креаторов поручать некому — ролик принадлежит
-- проекту, и выдумывать ему владельца нельзя: любой подставной
-- (менеджер, «технический пользователь») тут же полезет в отчёт по
-- людям, в состав и в начисления как настоящий участник.
--
-- NULL означает ровно одно: «это ролик проекта, а не чей-то».
ALTER TABLE project_publications
    ALTER COLUMN creator_user_id DROP NOT NULL;

-- 2. Агрегация по дате — уникальным индексом, а не договорённостью.
--
-- Старый publications_creator_day_uniq (00032:123-126) стоит на
-- (project_id, creator_user_id, due_date). В уникальном индексе
-- Postgres считает NULL'ы различными, поэтому на выкладках без
-- креатора он перестаёт защищать хоть что-нибудь: два прохода
-- автопривязки в одну секунду заведут два ролика на один день, и
-- заметит это только заказчик в ленте.
--
-- Здесь же живёт и само требование «один ролик = один день»: дата
-- выхода и есть ключ, по которому площадки сводятся в один ролик.
CREATE UNIQUE INDEX publications_project_day_uniq
    ON project_publications(project_id, due_date)
    WHERE creator_user_id IS NULL AND status <> 'cancelled';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS publications_project_day_uniq;

-- Возврат NOT NULL возможен, только если выкладок без креатора не
-- осталось. Иначе откат молча упрётся в собственное ограничение — и
-- это правильно: чинить надо данные, а не ограничение.
ALTER TABLE project_publications
    ALTER COLUMN creator_user_id SET NOT NULL;

-- +goose StatementEnd
