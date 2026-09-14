-- +goose Up
-- +goose StatementBegin

-- Деньги: условия, платежи заказчика и начисления креаторам.
--
-- Устройство намеренно скромное. Платёжного провайдера нет и не
-- предполагается: клиент знакомится с условиями, платит мимо системы, а
-- менеджер подтверждает получение кнопкой — тем же способом, каким он
-- двигает этапы в продакшн-проекте. Автоматически считается ровно одно —
-- бонус по просмотрам, потому что просмотры мы и так собираем каждый день.

-- 1. Условия перестают быть текстовым блоком.
--
-- terms_versions.body остаётся: это то, что человек читает и с чем
-- соглашается. Но собрать из него карточку «оклад 60 000, 90 ₽/1000,
-- порог 1 000 000» нельзя, а показывать её надо — поэтому те же числа
-- лежат рядом полями.
ALTER TABLE terms_versions
    -- Оклад креатора за месяц, в копейках. В копейках, а не в рублях:
    -- проценты и деления на 1000 в целых рублях дают расхождение в
    -- последнем знаке, и оно всплывает при сверке.
    ADD COLUMN salary_per_month BIGINT NOT NULL DEFAULT 0,
    -- Ставка за 1000 просмотров ДО порога, в копейках (90 ₽).
    ADD COLUMN rate_per_1000_views BIGINT NOT NULL DEFAULT 0,
    -- Порог просмотров НА РОЛИК, суммой по пяти площадкам (1 000 000).
    ADD COLUMN bonus_views_threshold BIGINT NOT NULL DEFAULT 0,
    -- Ставка за 1000 просмотров СВЕРХ порога, в копейках (9 ₽).
    -- Ставка падает после порога намеренно: так виральный ролик не
    -- съедает бюджет клиента.
    ADD COLUMN rate_per_1000_views_over BIGINT NOT NULL DEFAULT 0,
    -- Переходы по UTM устроены так же, но порог у них МЕСЯЧНЫЙ, а не на
    -- ролик: 70 ₽ за первую тысячу переходов за месяц, 7 ₽ за каждый
    -- следующий. NULL в ставке = бонус за переходы не считается; источник
    -- кликов не подключён, а поля в тарифе есть.
    ADD COLUMN click_bonus_rate BIGINT,
    ADD COLUMN click_bonus_threshold INT NOT NULL DEFAULT 0,
    ADD COLUMN click_bonus_rate_over BIGINT,
    ADD CONSTRAINT terms_amounts_non_negative CHECK (
        salary_per_month >= 0 AND rate_per_1000_views >= 0
        AND rate_per_1000_views_over >= 0
        AND bonus_views_threshold >= 0 AND click_bonus_threshold >= 0
        AND (click_bonus_rate IS NULL OR click_bonus_rate >= 0)
        AND (click_bonus_rate_over IS NULL OR click_bonus_rate_over >= 0));

-- 2. Условия проекта — СНИМОК, а не ссылка на действующую версию.
-- Требование ЗК-БП10: изменение прайса не переписывает историю. Проект,
-- начатый по старым ставкам, по ним и досчитывается.
CREATE TABLE project_billing (
    project_id            UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    -- terms_version_id — откуда сняты числа. Только для истории:
    -- в самой версии их могут потом поправить.
    terms_version_id      UUID REFERENCES terms_versions(id),
    salary_per_month      BIGINT NOT NULL DEFAULT 0,
    rate_per_1000_views      BIGINT NOT NULL DEFAULT 0,
    bonus_views_threshold    BIGINT NOT NULL DEFAULT 0,
    rate_per_1000_views_over BIGINT NOT NULL DEFAULT 0,
    click_bonus_rate         BIGINT,
    click_bonus_threshold    INT NOT NULL DEFAULT 0,
    click_bonus_rate_over    BIGINT,
    updated_by            UUID REFERENCES users(id),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT project_billing_non_negative CHECK (
        salary_per_month >= 0 AND rate_per_1000_views >= 0
        AND rate_per_1000_views_over >= 0
        AND bonus_views_threshold >= 0 AND click_bonus_threshold >= 0
        AND (click_bonus_rate IS NULL OR click_bonus_rate >= 0)
        AND (click_bonus_rate_over IS NULL OR click_bonus_rate_over >= 0))
);

-- 3. Платежи заказчика. Половина вперёд, половина по завершении —
-- решение заказчика, поэтому видов ровно два и каждого по одному.
CREATE TYPE payment_kind AS ENUM ('prepayment', 'final');
CREATE TYPE payment_status AS ENUM ('awaiting', 'confirmed', 'cancelled');

CREATE TABLE project_payments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind         payment_kind NOT NULL,
    -- amount — сколько ждём, в копейках.
    amount       BIGINT NOT NULL CHECK (amount >= 0),
    status       payment_status NOT NULL DEFAULT 'awaiting',
    note         TEXT NOT NULL DEFAULT '',
    confirmed_by UUID REFERENCES users(id),
    confirmed_at TIMESTAMPTZ,
    created_by   UUID REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind),
    -- Подтвердивший и время подтверждения появляются вместе.
    CONSTRAINT project_payments_confirmed_together
        CHECK ((status = 'confirmed') = (confirmed_at IS NOT NULL)
               AND (status = 'confirmed') = (confirmed_by IS NOT NULL))
);

-- 4. Начисления креаторам, по месяцам.
--
-- Строка пересчитывается, пока не утверждена. Утверждение и выплата —
-- две отдельные кнопки менеджера: между «посчитали» и «отправили деньги»
-- проходит время, и путать их нельзя.
CREATE TYPE accrual_status AS ENUM ('draft', 'approved', 'paid');

CREATE TABLE creator_accruals (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    creator_user_id  UUID NOT NULL REFERENCES users(id),
    -- period_month — первое число месяца, как в creator_availability.
    period_month     DATE NOT NULL,
    salary           BIGINT NOT NULL DEFAULT 0,
    -- videos_planned/videos_delivered — сколько выкладок стояло и сколько
    -- закрыто. Разница — недостача: недосданное не оплачивается.
    videos_planned   INT NOT NULL DEFAULT 0,
    videos_delivered INT NOT NULL DEFAULT 0,
    deduction        BIGINT NOT NULL DEFAULT 0,
    -- Просмотры разложены по ступеням тарифа: views_base — то, что
    -- попало под полную ставку (до порога на каждом ролике),
    -- views_over — то, что сверх порога и считается по пониженной.
    -- Хранятся обе части, потому что по одному итогу потом не разобрать,
    -- почему бонус именно такой.
    views_total      BIGINT NOT NULL DEFAULT 0,
    views_base       BIGINT NOT NULL DEFAULT 0,
    views_over       BIGINT NOT NULL DEFAULT 0,
    views_bonus      BIGINT NOT NULL DEFAULT 0,
    clicks           INT    NOT NULL DEFAULT 0,
    click_bonus      BIGINT NOT NULL DEFAULT 0,
    total            BIGINT NOT NULL DEFAULT 0,
    status           accrual_status NOT NULL DEFAULT 'draft',
    approved_by      UUID REFERENCES users(id),
    approved_at      TIMESTAMPTZ,
    paid_by          UUID REFERENCES users(id),
    paid_at          TIMESTAMPTZ,
    calculated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, creator_user_id, period_month),
    CONSTRAINT creator_accruals_month_is_first
        CHECK (EXTRACT(DAY FROM period_month) = 1)
);

CREATE INDEX creator_accruals_creator_idx
    ON creator_accruals(creator_user_id, period_month DESC);

-- 5. UTM-метка креатора в проекте. Ставит менеджер.
-- clicks заполняется снаружи (из аналитики); бонус за переходы пока
-- выключен, поэтому на деньги это влияет только если включить ставку.
CREATE TABLE creator_utm_links (
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    clicks          INT  NOT NULL DEFAULT 0 CHECK (clicks >= 0),
    created_by      UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, creator_user_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS creator_utm_links;
DROP INDEX IF EXISTS creator_accruals_creator_idx;
DROP TABLE IF EXISTS creator_accruals;
DROP TYPE IF EXISTS accrual_status;
DROP TABLE IF EXISTS project_payments;
DROP TYPE IF EXISTS payment_status;
DROP TYPE IF EXISTS payment_kind;
DROP TABLE IF EXISTS project_billing;

ALTER TABLE terms_versions
    DROP CONSTRAINT IF EXISTS terms_amounts_non_negative,
    DROP COLUMN IF EXISTS click_bonus_rate_over,
    DROP COLUMN IF EXISTS click_bonus_threshold,
    DROP COLUMN IF EXISTS click_bonus_rate,
    DROP COLUMN IF EXISTS rate_per_1000_views_over,
    DROP COLUMN IF EXISTS bonus_views_threshold,
    DROP COLUMN IF EXISTS rate_per_1000_views,
    DROP COLUMN IF EXISTS salary_per_month;

-- +goose StatementEnd
