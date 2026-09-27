-- +goose Up
-- +goose StatementBegin

-- Срез месяца снимается НА ДАТУ ОТСЕЧКИ, а не «на сейчас».
--
-- Отсечка — конец месяца плюс две недели. Раньше срез брал последний
-- снимок каждой ссылки, и это было верно ровно до первого опоздания:
-- воркер простоял сутки — и месяц запомнил просмотры, набранные уже
-- после отсечки. Расхождение тихое, связать его с простоем потом
-- невозможно.
--
-- Поденный ряд накопительный, поэтому «просмотры на дату» достаются
-- штатно: последняя строка, не позже отсечки.
ALTER TABLE project_months
    -- SnapshotAsOf — на какую дату сняты числа. Показывается рядом с
    -- ними: «за сентябрь по состоянию на 14 октября» — это ответ, а
    -- просто «за сентябрь» — нет.
    ADD COLUMN snapshot_as_of DATE,
    -- SnapshotApprox — числам не на что опереться: поденный ряд к
    -- моменту фиксации уже схлопнут, восстанавливать нечего. Месяц
    -- всё равно фиксируется (иначе он завис бы открытым навсегда), но
    -- число, про которое неизвестно, настоящее оно или подтянутое,
    -- обязано быть подписано.
    --
    -- Это НЕ то же самое, что «предварительно»: предварительно —
    -- про то, что месяц ещё идёт и числа изменятся; приблизительно —
    -- про то, что опереться не на что. Могут стоять одновременно.
    ADD COLUMN snapshot_approx BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE project_month_views
    -- views становится nullable: NULL означает «на отсечку данных не
    -- было». Ноль тут врал бы — он неотличим от честного нуля
    -- просмотров, а это разные вещи.
    ALTER COLUMN views DROP DEFAULT,
    ALTER COLUMN views DROP NOT NULL,
    -- stat_date — из какого дня ряда взято число. NULL = данных на
    -- отсечку не нашлось. Заодно видно, насколько снимок свежий
    -- относительно отсечки: ряд ведётся не каждый день.
    ADD COLUMN stat_date DATE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Возврат NOT NULL требует заполнить пропуски: «данных нет» в старой
-- схеме выражать нечем, поэтому пишем ноль.
UPDATE project_month_views SET views = 0 WHERE views IS NULL;

ALTER TABLE project_month_views
    DROP COLUMN IF EXISTS stat_date,
    ALTER COLUMN views SET NOT NULL,
    ALTER COLUMN views SET DEFAULT 0;

ALTER TABLE project_months
    DROP COLUMN IF EXISTS snapshot_approx,
    DROP COLUMN IF EXISTS snapshot_as_of;

-- +goose StatementEnd
