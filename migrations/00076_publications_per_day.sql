-- +goose Up
-- +goose StatementBegin

-- Несколько роликов в один день.
--
-- До сих пор день вмещал ровно один ролик, и держали это два
-- уникальных индекса: по (проект, креатор, день) там, где есть состав,
-- и по (проект, день) у проекта без креаторов. На них же стояла
-- идемпотентность простановки пачкой: повторное нажатие кнопки не
-- плодило дублей именно потому, что вторая строка на тот же день
-- физически не вставлялась.
--
-- Снять уникальность совсем значило бы отдать эту защиту: двойной клик
-- по «Создать выкладки» молча удвоил бы месяц, и разбирать это
-- пришлось бы руками. Поэтому вместо снятия — уточнение: у ролика
-- появляется НОМЕР ВНУТРИ ДНЯ, и уникальна теперь четвёрка. Пачка
-- по-прежнему идемпотентна: она приводит план к заданному «роликов в
-- день», а повтор не добавляет ничего.
ALTER TABLE project_publications
    ADD COLUMN day_slot INT NOT NULL DEFAULT 1;

-- Существующие строки получают первый номер: старый индекс
-- гарантировал, что в дне их не больше одной, — столкнуться не с чем.
DROP INDEX IF EXISTS publications_creator_day_uniq;
DROP INDEX IF EXISTS publications_project_day_uniq;

CREATE UNIQUE INDEX publications_creator_day_uniq
    ON project_publications (project_id, creator_user_id, due_date, day_slot)
    WHERE status <> 'cancelled';

CREATE UNIQUE INDEX publications_project_day_uniq
    ON project_publications (project_id, due_date, day_slot)
    WHERE creator_user_id IS NULL AND status <> 'cancelled';

-- Разумный потолок. Не про технику: тридцать роликов в один день —
-- это опечатка в поле «роликов в день», а не план съёмок, и дешевле
-- отказать сразу, чем вычищать потом.
ALTER TABLE project_publications
    ADD CONSTRAINT publications_day_slot_sane CHECK (day_slot BETWEEN 1 AND 10);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Назад — только если в днях не завелось второго ролика: иначе
-- уникальность не встанет, и откат обязан упасть громко, а не
-- выбросить чью-то работу.
DROP INDEX IF EXISTS publications_creator_day_uniq;
DROP INDEX IF EXISTS publications_project_day_uniq;
ALTER TABLE project_publications DROP CONSTRAINT IF EXISTS publications_day_slot_sane;
ALTER TABLE project_publications DROP COLUMN IF EXISTS day_slot;
CREATE UNIQUE INDEX publications_creator_day_uniq
    ON project_publications (project_id, creator_user_id, due_date)
    WHERE status <> 'cancelled';
CREATE UNIQUE INDEX publications_project_day_uniq
    ON project_publications (project_id, due_date)
    WHERE creator_user_id IS NULL AND status <> 'cancelled';
-- +goose StatementEnd
