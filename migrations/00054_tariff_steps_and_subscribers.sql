-- +goose Up
-- +goose StatementBegin

-- Произвольное число ступеней вместо двух зашитых.
--
-- До сих пор лесенка была двухступенчатой и жила колонками: цена ступени
-- до порога (step_fee) и после (step_fee_over). Живой клиент просит
-- другое — «оклад плюс KPI на 300 000 просмотров», и следом четвёртую и
-- пятую точку. Колонками это не выражается: каждая новая ступень
-- означала бы миграцию и правку всех запросов.
--
-- Поэтому ступени переезжают в строки. Ступень — это ПОРОГ ОБЪЁМА и цена
-- периода на нём: набрал за период столько-то просмотров — период стоит
-- столько-то. Не «цена за каждые сто тысяч», а именно цена периода: так
-- звучит коммерческое предложение, и так его читает клиент.
--
-- У каждой ступени своя пара чисел: сколько платит заказчик и сколько из
-- этого получает креатор. Креаторское пустое означает «как у заказчика»
-- — то же правило, что у всех остальных креаторских полей тарифа.
--
-- Старые двухступенчатые колонки НЕ трогаем и не переносим: на них стоят
-- действующие проекты со снятыми снимками, и пересчитать их задним
-- числом нельзя. Версия без строк-ступеней считается как раньше.
CREATE TABLE terms_steps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Ступени принадлежат либо версии прайса, либо снимку проекта.
    -- Одна таблица на оба случая намеренно: правило «ступень — это порог
    -- и две цены» одно, и второй такой же таблицей оно разъехалось бы.
    terms_version_id UUID REFERENCES terms_versions (id) ON DELETE CASCADE,
    project_id UUID REFERENCES projects (id) ON DELETE CASCADE,

    -- FromViews — с какого объёма просмотров периода действует ступень.
    -- Нижняя ступень обычно с нуля: это и есть голый оклад без KPI.
    from_views BIGINT NOT NULL,
    -- ClientFee — сколько стоит период заказчику на этой ступени.
    client_fee BIGINT NOT NULL,
    -- CreatorFee — оклад креатору на этой ступени. NULL = как у
    -- заказчика: до заполнения стороны совпадают и маржи нет.
    creator_fee BIGINT,

    CONSTRAINT terms_steps_one_owner CHECK (
        (terms_version_id IS NULL) <> (project_id IS NULL)
    ),
    CONSTRAINT terms_steps_non_negative CHECK (
        from_views >= 0 AND client_fee >= 0 AND (creator_fee IS NULL OR creator_fee >= 0)
    )
);

-- Два порога с одной суммой — не «уточнение», а неразрешимая
-- неоднозначность: какую цену брать, решал бы порядок строк.
CREATE UNIQUE INDEX terms_steps_version_from_idx
    ON terms_steps (terms_version_id, from_views) WHERE terms_version_id IS NOT NULL;
CREATE UNIQUE INDEX terms_steps_project_from_idx
    ON terms_steps (project_id, from_views) WHERE project_id IS NOT NULL;

-- ---- KPI по подписчикам ----
--
-- Подписчиков НИКТО НЕ СОБИРАЕТ: ни один сборщик по ним не ходит, и
-- выдумывать сбор под ставку нельзя. Поэтому здесь объявляется только
-- цена, а само число вводит менеджер руками — ровно так же, как уже
-- сделано с переходами по UTM (creator_utm_links.clicks).
ALTER TABLE terms_versions
    -- SubscriberRate — сколько заказчик платит за одного подписчика,
    -- набранного за период, копейки. NULL = KPI по подписчикам не
    -- считается вовсе.
    ADD COLUMN subscriber_rate BIGINT,
    -- CreatorSubscriberRate — сколько из этого получает креатор.
    -- NULL = столько же, сколько платит заказчик.
    ADD COLUMN creator_subscriber_rate BIGINT,
    ADD CONSTRAINT terms_subscriber_rate_non_negative CHECK (
        (subscriber_rate IS NULL OR subscriber_rate >= 0)
        AND (creator_subscriber_rate IS NULL OR creator_subscriber_rate >= 0)
    );

ALTER TABLE project_billing
    ADD COLUMN subscriber_rate BIGINT,
    ADD COLUMN creator_subscriber_rate BIGINT,
    ADD CONSTRAINT project_billing_subscriber_rate_non_negative CHECK (
        (subscriber_rate IS NULL OR subscriber_rate >= 0)
        AND (creator_subscriber_rate IS NULL OR creator_subscriber_rate >= 0)
    );

-- Само число подписчиков — по креатору и по периоду.
--
-- По ПЕРИОДУ, а не одной строкой на проект: KPI считается за период, и
-- одно поле «сколько всего подписчиков» пришлось бы каждый месяц
-- перезаписывать, теряя то, за что уже заплатили. Заведено так же, как
-- переходы: строка появляется, когда менеджер вписал число.
CREATE TABLE creator_period_subscribers (
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- PeriodStart — начало периода, к которому отнесено число. Тот же
    -- ключ, что у начислений.
    period_start DATE NOT NULL,
    -- Subscribers — сколько подписчиков прибавилось за период. Вводит
    -- менеджер руками: автоматического источника нет.
    subscribers BIGINT NOT NULL DEFAULT 0 CHECK (subscribers >= 0),
    updated_by UUID REFERENCES users (id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, creator_user_id, period_start)
);

-- Подписчики в начислении — отдельной парой чисел, а не внутри бонуса за
-- просмотры. Начисление объясняет, ПОЧЕМУ вышла такая сумма, и слитые в
-- одно число просмотры с подписчиками этого больше не объясняют.
ALTER TABLE creator_accruals
    ADD COLUMN subscribers BIGINT NOT NULL DEFAULT 0 CHECK (subscribers >= 0),
    ADD COLUMN subscriber_bonus BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN payout_subscriber_bonus BIGINT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE creator_accruals
    DROP COLUMN IF EXISTS payout_subscriber_bonus,
    DROP COLUMN IF EXISTS subscriber_bonus,
    DROP COLUMN IF EXISTS subscribers;

DROP TABLE IF EXISTS creator_period_subscribers;

ALTER TABLE project_billing
    DROP CONSTRAINT IF EXISTS project_billing_subscriber_rate_non_negative;
ALTER TABLE project_billing
    DROP COLUMN IF EXISTS creator_subscriber_rate,
    DROP COLUMN IF EXISTS subscriber_rate;

ALTER TABLE terms_versions
    DROP CONSTRAINT IF EXISTS terms_subscriber_rate_non_negative;
ALTER TABLE terms_versions
    DROP COLUMN IF EXISTS creator_subscriber_rate,
    DROP COLUMN IF EXISTS subscriber_rate;

DROP TABLE IF EXISTS terms_steps;

-- +goose StatementEnd
