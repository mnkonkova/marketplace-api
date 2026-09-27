-- +goose Up
-- +goose StatementBegin

-- Ступенчатый тариф.
--
-- Порогов в тарифе ДВА, и они про разное — не вздумайте «упростить» их
-- в один:
--   * порог НА РОЛИК (bonus_views_threshold, уже есть с 00037) —
--     защита от одной виральной удачи: просмотры сверх миллиона на
--     одном ролике оплачиваются по пониженной ставке и в месячные
--     ступени не идут вовсе;
--   * месячные СТУПЕНИ (ниже) — отражают рост аккаунта: полные сотни
--     тысяч просмотров, сложенные по всем роликам периода.
-- Одно другое не заменяет: три миллиона одним виральным роликом и три
-- миллиона работой за месяц стоят клиенту разного.
--
-- Клиент платит фикс за период плюс по ставке за каждую ПОЛНУЮ ступень
-- просмотров; за неполную не платит и не переносит её (решение владельца
-- продукта, зафиксировано намеренно — это не баг и пересматривать не
-- нужно). У креатора ступени свои, и остаток, наоборот, переносится в
-- следующий период.
--
-- Всё живёт в версии условий рядом с нынешними полями: проект считается
-- по той версии, на которой стоит, и выпуск новой не должен пересчитать
-- уже идущие проекты. Прежние версии остаются без этих чисел и считаются
-- по-старому — оклад плюс ставка за тысячу.
--
-- Признак «версия ступенчатая» — заполненный step_views: у старых версий
-- он NULL, и код уходит в прежнюю ветку расчёта.

-- ---- прайс площадки ----
ALTER TABLE terms_versions
    -- StepViews — размер ступени в просмотрах. Раньше сто тысяч жили
    -- константой в коде; теперь настраиваются вместе с остальным.
    ADD COLUMN step_views BIGINT,
    -- FirstPeriodFee — фикс за первый период проекта. Первый период
    -- считается иначе: там оплачивается запуск, а не результат.
    ADD COLUMN first_period_fee BIGINT,
    -- BaseFee — фикс со второго периода.
    ADD COLUMN base_fee BIGINT,
    -- StepFee — сколько стоит полная ступень до первого порога.
    ADD COLUMN step_fee BIGINT,
    -- StepTier2From — с какого объёма ступень дешевеет.
    ADD COLUMN step_tier2_from BIGINT,
    -- StepFeeOver — сколько стоит ступень после этого порога.
    ADD COLUMN step_fee_over BIGINT,
    -- StepCapViews — выше этого объёма ступени не оплачиваются вовсе.
    ADD COLUMN step_cap_views BIGINT,
    -- GuaranteeViews — гарантия: меньше этого объёма клиент всё равно
    -- платит как за гарантию, а недостающие просмотры мы ему должны и
    -- гасим из следующего периода. Долг в ПРОСМОТРАХ, не в деньгах —
    -- так в оферте: «недостающее доберём бесплатно».
    ADD COLUMN guarantee_views BIGINT,

    -- Креаторская сторона тех же ступеней. NULL означает «столько же,
    -- сколько у клиента» — то же правило, что у нынешних креаторских
    -- ставок. Настоящие числа владелец продукта выпустит из админки,
    -- когда решит; механика от этого не зависит.
    ADD COLUMN creator_first_period_fee BIGINT,
    ADD COLUMN creator_base_fee BIGINT,
    ADD COLUMN creator_step_fee BIGINT,
    ADD COLUMN creator_step_fee_over BIGINT;

-- ---- снимок условий у проекта ----
ALTER TABLE project_billing
    ADD COLUMN step_views BIGINT,
    ADD COLUMN first_period_fee BIGINT,
    ADD COLUMN base_fee BIGINT,
    ADD COLUMN step_fee BIGINT,
    ADD COLUMN step_tier2_from BIGINT,
    ADD COLUMN step_fee_over BIGINT,
    ADD COLUMN step_cap_views BIGINT,
    ADD COLUMN guarantee_views BIGINT,
    ADD COLUMN creator_first_period_fee BIGINT,
    ADD COLUMN creator_base_fee BIGINT,
    ADD COLUMN creator_step_fee BIGINT,
    ADD COLUMN creator_step_fee_over BIGINT;

-- Долг по гарантии — величина периода, как и перенос остатка креатора.
-- Замораживается подытогом вместе со срезом: пересчёт задним числом
-- поехал бы цепочкой по уже оплаченным периодам.
ALTER TABLE project_periods
    -- ClientDebtIn — сколько просмотров мы должны клиенту на входе в
    -- период: они не выставляются ему к оплате.
    ADD COLUMN client_debt_in BIGINT NOT NULL DEFAULT 0,
    -- ClientDebtOut — сколько остались должны на выходе.
    ADD COLUMN client_debt_out BIGINT NOT NULL DEFAULT 0;

ALTER TABLE project_periods
    ADD CONSTRAINT project_periods_debt_non_negative
        CHECK (client_debt_in >= 0 AND client_debt_out >= 0);

-- Действующая версия прайса со ступенями: числа из коммерческого
-- предложения владельца продукта (сентябрь 2026). Контрольные точки со
-- второго периода: 300 тыс. → 78 000 ₽, 500 тыс. → 90 000, 1 млн →
-- 120 000, 2 млн → 180 000, 3 млн → 195 000.
--
-- Выпускаем новой версией, а не правкой прежней: на прежней стоят
-- проекты, и переписать её значило бы пересчитать их задним числом.
INSERT INTO terms_versions (
    version, body,
    salary_per_month, videos_first_month, videos_next_months,
    rate_per_1000_views, bonus_views_threshold, rate_per_1000_views_over,
    step_views, first_period_fee, base_fee, step_fee,
    step_tier2_from, step_fee_over, step_cap_views, guarantee_views
) VALUES (
    (SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions),
    'Ступенчатый тариф: первый период 65 000 ₽ за 30 роликов (цель 140 000 просмотров), '
    'со второго 60 000 ₽ плюс 6 000 ₽ за каждые полные 100 000 просмотров до 2 млн '
    'и 1 500 ₽ за ступень от 2 до 3 млн; свыше 3 млн ступени не оплачиваются. '
    'Гарантия 300 000 просмотров: при недоборе период оплачивается как 300 000, '
    'а недостающие просмотры добираются бесплатно в следующем периоде. '
    'Просмотры сверх 1 000 000 на одном ролике оплачиваются отдельно по 6 ₽ за тысячу '
    'и в ступени не идут.',
    -- Оклада старой модели в ступенчатой версии нет: расчёт уходит в
    -- ступенчатую ветку и до него не доходит. Ноль здесь — не
    -- «бесплатно», а «этой моделью не считаем».
    0, 30, 30,
    -- А вот порог на ролик и ставки за тысячу работают и здесь: по ним
    -- считается виральный хвост. Базовая ставка — это та же ступень,
    -- выраженная за тысячу: 6 000 ₽ за 100 000 просмотров = 60 ₽.
    -- Пониженная ровно в десять раз ниже базовой — правило тарифа, а не
    -- случайное число (см. версию 1: 90 ₽ и 9 ₽).
    6000, 1000000, 600,
    100000, 6500000, 6000000, 600000,
    2000000, 150000, 3000000, 300000
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM terms_versions WHERE step_views IS NOT NULL;

ALTER TABLE project_periods
    DROP CONSTRAINT IF EXISTS project_periods_debt_non_negative;
ALTER TABLE project_periods
    DROP COLUMN IF EXISTS client_debt_out,
    DROP COLUMN IF EXISTS client_debt_in;

ALTER TABLE project_billing
    DROP COLUMN IF EXISTS creator_step_fee_over,
    DROP COLUMN IF EXISTS creator_step_fee,
    DROP COLUMN IF EXISTS creator_base_fee,
    DROP COLUMN IF EXISTS creator_first_period_fee,
    DROP COLUMN IF EXISTS guarantee_views,
    DROP COLUMN IF EXISTS step_cap_views,
    DROP COLUMN IF EXISTS step_fee_over,
    DROP COLUMN IF EXISTS step_tier2_from,
    DROP COLUMN IF EXISTS step_fee,
    DROP COLUMN IF EXISTS base_fee,
    DROP COLUMN IF EXISTS first_period_fee,
    DROP COLUMN IF EXISTS step_views;

ALTER TABLE terms_versions
    DROP COLUMN IF EXISTS creator_step_fee_over,
    DROP COLUMN IF EXISTS creator_step_fee,
    DROP COLUMN IF EXISTS creator_base_fee,
    DROP COLUMN IF EXISTS creator_first_period_fee,
    DROP COLUMN IF EXISTS guarantee_views,
    DROP COLUMN IF EXISTS step_cap_views,
    DROP COLUMN IF EXISTS step_fee_over,
    DROP COLUMN IF EXISTS step_tier2_from,
    DROP COLUMN IF EXISTS step_fee,
    DROP COLUMN IF EXISTS base_fee,
    DROP COLUMN IF EXISTS first_period_fee,
    DROP COLUMN IF EXISTS step_views;

-- +goose StatementEnd
