-- +goose Up
-- +goose StatementBegin

-- Период проекта вместо календарного месяца.
--
-- Проект с креаторами больше не пересоздаётся каждый месяц — он один, а
-- внутри катятся периоды. Первый начинается ДАТОЙ ПЕРВОЙ ПУБЛИКАЦИИ и
-- длится ровно месяц: вышел первый ролик 15-го — периоды идут с 15-го по
-- 14-е. Календарный месяц к работе креаторов отношения не имел: он делил
-- пополам и съёмку, и оплату.
--
-- Старые таблицы месяца сносим, а не переносим: зафиксированных месяцев
-- нет ни на стенде, ни в тестовой базе, переносить нечего.
DROP TABLE IF EXISTS project_month_views;
DROP TABLE IF EXISTS project_month_publications;
DROP TABLE IF EXISTS project_months;

CREATE TABLE project_periods (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Seq — какой это период по счёту: первый, второй, третий. Хранится,
    -- а не вычисляется: на нём будут стоять пороги тарифа (тридцать
    -- роликов в первом, шестьдесят дальше), и вычислять его заново на
    -- каждый вопрос — значит однажды посчитать по-разному в двух местах.
    seq INT NOT NULL CHECK (seq >= 1),
    -- Границы включительные с обеих сторон: так они и читаются человеком
    -- («с 15 сентября по 14 октября»). В запросах конец берётся как
    -- ends_on + 1 день, чтобы день конца целиком принадлежал периоду.
    starts_on DATE NOT NULL,
    ends_on   DATE NOT NULL,
    -- PrevPeriodID — предыдущий период. Обязателен для всех, кроме
    -- первого: остаток ступени переносится из периода в период, и
    -- цепочка должна быть явной, а не восстанавливаться сортировкой.
    -- ON DELETE RESTRICT: удалить период, на который ссылается
    -- следующий, нельзя — это разорвало бы цепочку переноса.
    prev_period_id UUID REFERENCES project_periods(id) ON DELETE RESTRICT,

    status          TEXT NOT NULL DEFAULT 'open',
    locked_at       TIMESTAMPTZ,
    locked_by       UUID REFERENCES users(id) ON DELETE SET NULL,
    snapshot_as_of  DATE,
    snapshot_approx BOOLEAN NOT NULL DEFAULT FALSE,

    -- Перенос остатка ступени. Тариф становится ступенчатым (ступень —
    -- 100 000 просмотров), и остаток, не добравший до полной ступени,
    -- едет в следующий период и складывается с его просмотрами.
    --
    -- По стороне на каждое число: клиентская и креаторская стороны
    -- считаются по разным ставкам и разъедутся, а одно число на двоих
    -- скрыло бы это расхождение.
    --
    -- Заполнит расчёт, когда тариф утвердят; сейчас нули. Лежат они
    -- именно здесь, в зафиксированном периоде, потому что перенос —
    -- такая же замороженная величина, как просмотры: пересчитайся он
    -- задним числом, правка старого периода поехала бы цепочкой по всем
    -- следующим, а они уже оплачены.
    carry_in_client   BIGINT NOT NULL DEFAULT 0,
    carry_out_client  BIGINT NOT NULL DEFAULT 0,
    carry_in_creator  BIGINT NOT NULL DEFAULT 0,
    carry_out_creator BIGINT NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (project_id, seq),
    UNIQUE (project_id, starts_on),
    CONSTRAINT project_periods_status_check CHECK (status IN ('open', 'locked')),
    CONSTRAINT project_periods_locked_has_time CHECK ((status = 'locked') = (locked_at IS NOT NULL)),
    CONSTRAINT project_periods_order CHECK (ends_on >= starts_on),
    -- Первый период — единственный без предшественника.
    CONSTRAINT project_periods_chain CHECK ((seq = 1) = (prev_period_id IS NULL)),
    CONSTRAINT project_periods_carry_non_negative CHECK (
        carry_in_client >= 0 AND carry_out_client >= 0
        AND carry_in_creator >= 0 AND carry_out_creator >= 0
    )
);

-- Фоновая задача ищет периоды, которым пора закрыться.
CREATE INDEX project_periods_open_idx ON project_periods (ends_on) WHERE status = 'open';

-- Срез периода. Причины те же, что были у месяца: снимок обязан пережить
-- и удаление выкладки, и чистку поденного ряда, поэтому внешних ключей на
-- выкладки и ссылки здесь нет, а копии полей есть.
CREATE TABLE project_period_publications (
    period_id       UUID NOT NULL REFERENCES project_periods(id) ON DELETE CASCADE,
    publication_id  UUID NOT NULL,
    creator_user_id UUID NOT NULL,
    status          TEXT NOT NULL,
    -- PublishedOn — дата, по которой выкладка попала в этот период:
    -- фактическая публикация, а где её нет — сдача ссылки.
    published_on DATE NOT NULL,
    PRIMARY KEY (period_id, publication_id)
);

CREATE TABLE project_period_views (
    period_id      UUID NOT NULL REFERENCES project_periods(id) ON DELETE CASCADE,
    publication_id UUID NOT NULL,
    platform       TEXT NOT NULL,
    link_id        UUID,
    views          BIGINT,
    likes          BIGINT,
    comments       BIGINT,
    stat_date      DATE,
    published_at   TIMESTAMPTZ,
    PRIMARY KEY (period_id, publication_id, platform)
);

-- Начисления теперь про период, а не про календарный месяц. Колонка
-- хранит дату НАЧАЛА периода: она уникальна внутри проекта, и по ней
-- строка однозначно ложится на свой период.
ALTER TABLE creator_accruals RENAME COLUMN period_month TO period_start;

-- Проверка «первое число месяца» больше не верна: период начинается
-- датой первой публикации и с календарём не совпадает.
ALTER TABLE creator_accruals DROP CONSTRAINT IF EXISTS creator_accruals_month_is_first;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE creator_accruals RENAME COLUMN period_start TO period_month;

-- Возврат проверки: строки не с первого числа откату мешают, поэтому
-- сначала сдвигаем их на начало своего месяца.
UPDATE creator_accruals SET period_month = date_trunc('month', period_month)::date
WHERE EXTRACT(day FROM period_month) <> 1;

ALTER TABLE creator_accruals DROP CONSTRAINT IF EXISTS creator_accruals_month_is_first;
ALTER TABLE creator_accruals
    ADD CONSTRAINT creator_accruals_month_is_first
    CHECK (EXTRACT(day FROM period_month) = 1);

DROP TABLE IF EXISTS project_period_views;
DROP TABLE IF EXISTS project_period_publications;
DROP TABLE IF EXISTS project_periods;

CREATE TABLE project_months (
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    period_month DATE NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open',
    locked_at    TIMESTAMPTZ,
    locked_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    snapshot_as_of  DATE,
    snapshot_approx BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, period_month),
    CONSTRAINT project_months_status_check CHECK (status IN ('open', 'locked')),
    CONSTRAINT project_months_locked_has_time CHECK ((status = 'locked') = (locked_at IS NOT NULL))
);
CREATE INDEX project_months_open_idx ON project_months (period_month) WHERE status = 'open';

CREATE TABLE project_month_publications (
    project_id      UUID NOT NULL,
    period_month    DATE NOT NULL,
    publication_id  UUID NOT NULL,
    creator_user_id UUID NOT NULL,
    status          TEXT NOT NULL,
    due_date        DATE NOT NULL,
    PRIMARY KEY (project_id, period_month, publication_id),
    FOREIGN KEY (project_id, period_month)
        REFERENCES project_months (project_id, period_month) ON DELETE CASCADE
);

CREATE TABLE project_month_views (
    project_id     UUID NOT NULL,
    period_month   DATE NOT NULL,
    publication_id UUID NOT NULL,
    platform       TEXT NOT NULL,
    link_id        UUID,
    views          BIGINT,
    likes          BIGINT,
    comments       BIGINT,
    stat_date      DATE,
    published_at   TIMESTAMPTZ,
    PRIMARY KEY (project_id, period_month, publication_id, platform),
    FOREIGN KEY (project_id, period_month)
        REFERENCES project_months (project_id, period_month) ON DELETE CASCADE
);

-- +goose StatementEnd
