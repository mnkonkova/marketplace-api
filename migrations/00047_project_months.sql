-- +goose Up
-- +goose StatementBegin

-- Месяц проекта как сущность.
--
-- До сих пор месяца не было вовсе: был period_month в строке начисления,
-- и только. Утверждение начисления замораживало деньги, но не просмотры —
-- числа, из которых выросла сумма, продолжали меняться. Открыв тот же
-- месяц через полгода, менеджер видел рядом с прежней суммой другие
-- просмотры, и объяснить это было нечем.
--
-- Состояний два: open (идёт, всё считается на лету) и locked
-- (зафиксирован). Статус текстом с CHECK, а не enum'ом: следующим
-- состоянием будет перенос недобора гарантии на следующий месяц, и
-- добавить его в CHECK — одна строка миграции, а в enum — пересоздание
-- типа со всеми зависимостями.
CREATE TABLE project_months (
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- period_month — первое число месяца, как в creator_accruals.
    period_month DATE NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open',
    locked_at    TIMESTAMPTZ,
    -- locked_by — кто зафиксировал. NULL означает «фоновая задача»:
    -- автоматическая фиксация через 14 дней после конца месяца — самый
    -- частый путь, и приписывать её человеку было бы враньём.
    locked_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, period_month),
    CONSTRAINT project_months_status_check CHECK (status IN ('open', 'locked')),
    -- Зафиксирован ровно тогда, когда есть отметка о фиксации: два поля
    -- об одном и том же обязаны сходиться, иначе первый же откат
    -- оставит месяц «закрытым без даты закрытия».
    CONSTRAINT project_months_locked_has_time
        CHECK ((status = 'locked') = (locked_at IS NOT NULL))
);

-- Фоновая задача ищет месяцы, которым пора закрыться: обход по статусу.
CREATE INDEX project_months_open_idx ON project_months (period_month) WHERE status = 'open';

-- Срез месяца: выкладки, которые в нём считались.
--
-- Отдельная таблица, а не пересчёт из живых данных: в этом вся суть
-- фиксации. Никаких внешних ключей на выкладки и ссылки — снимок обязан
-- пережить и удаление выкладки, и чистку ежедневной статистики
-- (video_stat_daily схлопывается через 28 дней после закрытия проекта).
-- По той же причине здесь лежат копии creator_user_id, status и
-- due_date: снимок — это фотография, а не модель, и «пересобрать месяц»
-- должно работать, даже когда исходники изменились.
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

-- Просмотры по каждой площадке каждой выкладки на момент фиксации.
--
-- Именно по площадкам, а не итогом по ролику: оценка ролика, доли и
-- процентили считаются по площадкам, и пересобрать их потом из итога
-- нельзя. Итог складывается из этих строк, обратно — нет.
CREATE TABLE project_month_views (
    project_id     UUID NOT NULL,
    period_month   DATE NOT NULL,
    publication_id UUID NOT NULL,
    platform       TEXT NOT NULL,
    -- link_id — для связи с живой ссылкой, пока она есть. Без внешнего
    -- ключа намеренно: ссылку могут удалить, срез остаётся.
    link_id      UUID,
    views        BIGINT NOT NULL DEFAULT 0,
    likes        BIGINT,
    comments     BIGINT,
    published_at TIMESTAMPTZ,
    PRIMARY KEY (project_id, period_month, publication_id, platform),
    FOREIGN KEY (project_id, period_month)
        REFERENCES project_months (project_id, period_month) ON DELETE CASCADE
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS project_month_views;
DROP TABLE IF EXISTS project_month_publications;
DROP TABLE IF EXISTS project_months;

-- +goose StatementEnd
