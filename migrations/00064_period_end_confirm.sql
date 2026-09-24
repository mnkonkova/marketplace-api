-- +goose Up
-- +goose StatementBegin

-- Конец периода, подтверждённый менеджером.
--
-- Границу по-прежнему считает автомат: месяц от первой выкладки, отсечка
-- через две недели после конца. Но план знает человек, а не календарь:
-- последняя выкладка периода может стоять не в тот день, в который
-- месяц кончается по арифметике. Поэтому подтверждённая дата сильнее
-- вычисленной — и от неё же едет начало следующего периода.
--
-- Пусто — никто не подтверждал, работает автомат. Это не «забыли»:
-- проект, в котором менеджер ничего не трогал, обязан считаться сам.
ALTER TABLE project_periods
    ADD COLUMN IF NOT EXISTS ends_on_confirmed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ends_on_confirmed_by UUID REFERENCES users(id);

-- Подтверждать можно только то, что ещё не подытожено: под подытогом
-- стоит счёт, и двигать его границу задним числом нельзя.
ALTER TABLE project_periods
    ADD CONSTRAINT project_periods_confirm_pair
    CHECK ((ends_on_confirmed_at IS NULL) = (ends_on_confirmed_by IS NULL));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE project_periods DROP CONSTRAINT IF EXISTS project_periods_confirm_pair;
ALTER TABLE project_periods
    DROP COLUMN IF EXISTS ends_on_confirmed_by,
    DROP COLUMN IF EXISTS ends_on_confirmed_at;
-- +goose StatementEnd
