-- +goose Up
-- +goose StatementBegin

-- Что платит клиент и что получает креатор — разные суммы.
--
-- До сих пор в creator_accruals лежало одно число, и оно работало сразу
-- за оба: за счёт клиенту и за выплату креатору. Пока ставки совпадают,
-- разницы не видно; как только у платформы появляется своя маржа, креатор
-- в личном кабинете видит цену клиента и считает её своим заработком.
--
-- Поэтому у тарифа теперь две стороны. NULL в креаторской ставке означает
-- «столько же, сколько платит клиент» — то есть до заполнения этих полей
-- поведение ровно прежнее, без нулевых выплат и без миграции данных.

ALTER TABLE terms_versions
    ADD COLUMN creator_salary_per_month BIGINT,
    ADD COLUMN creator_rate_per_1000_views BIGINT,
    ADD COLUMN creator_rate_per_1000_views_over BIGINT,
    ADD CONSTRAINT terms_creator_rates_non_negative CHECK (
        (creator_salary_per_month IS NULL OR creator_salary_per_month >= 0)
        AND (creator_rate_per_1000_views IS NULL OR creator_rate_per_1000_views >= 0)
        AND (creator_rate_per_1000_views_over IS NULL OR creator_rate_per_1000_views_over >= 0));

ALTER TABLE project_billing
    ADD COLUMN creator_salary_per_month BIGINT,
    ADD COLUMN creator_rate_per_1000_views BIGINT,
    ADD COLUMN creator_rate_per_1000_views_over BIGINT,
    ADD CONSTRAINT project_billing_creator_rates_non_negative CHECK (
        (creator_salary_per_month IS NULL OR creator_salary_per_month >= 0)
        AND (creator_rate_per_1000_views IS NULL OR creator_rate_per_1000_views >= 0)
        AND (creator_rate_per_1000_views_over IS NULL OR creator_rate_per_1000_views_over >= 0));

-- Выплата креатору считается теми же правилами, что и счёт клиенту:
-- оклад минус недостача плюс бонус по ступеням. Отличаются только ставки,
-- поэтому и раскладка хранится своя — по одному итогу потом не объяснить
-- креатору, почему вышло столько.
ALTER TABLE creator_accruals
    ADD COLUMN payout_salary      BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN payout_deduction   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN payout_views_bonus BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN payout_click_bonus BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN payout_total       BIGINT NOT NULL DEFAULT 0;

-- Уже посчитанные периоды: до этой миграции выплата равнялась счёту.
-- Переписываем только черновики — утверждённое и выплаченное задним
-- числом не трогаем, даже к лучшему.
UPDATE creator_accruals
SET payout_salary = salary,
    payout_deduction = deduction,
    payout_views_bonus = views_bonus,
    payout_click_bonus = click_bonus,
    payout_total = total
WHERE status = 'draft';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE creator_accruals
    DROP COLUMN IF EXISTS payout_total,
    DROP COLUMN IF EXISTS payout_click_bonus,
    DROP COLUMN IF EXISTS payout_views_bonus,
    DROP COLUMN IF EXISTS payout_deduction,
    DROP COLUMN IF EXISTS payout_salary;

ALTER TABLE project_billing
    DROP CONSTRAINT IF EXISTS project_billing_creator_rates_non_negative,
    DROP COLUMN IF EXISTS creator_rate_per_1000_views_over,
    DROP COLUMN IF EXISTS creator_rate_per_1000_views,
    DROP COLUMN IF EXISTS creator_salary_per_month;

ALTER TABLE terms_versions
    DROP CONSTRAINT IF EXISTS terms_creator_rates_non_negative,
    DROP COLUMN IF EXISTS creator_rate_per_1000_views_over,
    DROP COLUMN IF EXISTS creator_rate_per_1000_views,
    DROP COLUMN IF EXISTS creator_salary_per_month;

-- +goose StatementEnd
