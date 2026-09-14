-- +goose Up
-- +goose StatementBegin

-- Клиент собирает пакет креаторов сам: смотрит выдачу с видео, расставляет
-- по приоритету, отправляет приглашения. Система доводит состав до нужного
-- числа без менеджера — он подключается там, где автоматика упёрлась.
-- Бизнес-требования — docs/CREATOR_ORDERS.md.
--
-- Почему отдельные таблицы, а не lead_recipients. Механика приглашений там
-- подходящая (статусы sent/viewed/accepted/declined), но первичный ключ
-- завязан на leads, а projects_lead_recipient_uniq (00010) предполагает
-- ОДИН проект на пару «заявка + специалист». Здесь наоборот: один проект
-- с несколькими креаторами. Переиспользуем приём, а не таблицу.

-- 1. Версии правил работы. Клиент соглашается с конкретной версией, и
-- старые заказы остаются на своей: иначе спор «мы такого не читали»
-- нечем закрыть.
CREATE TABLE terms_versions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version      INT  NOT NULL UNIQUE,
    body         TEXT NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE client_terms_consents (
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    terms_version_id UUID NOT NULL REFERENCES terms_versions(id),
    consented_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, terms_version_id)
);

-- 2. Заказ на подбор.
--
--   draft     — клиент собирает подборку, приглашения не отправлены
--   inviting  — приглашения ушли, ждём ответов
--   staffed   — согласилось столько, сколько нужно
--   paid      — оплачено; после этого создаётся проект
--   cancelled — клиент распустил состав
--
-- Платежей в системе нет (см. CLAUDE.md), поэтому paid_at проставляет
-- менеджер вручную. Отдельный статус, а не булево: он часть жизненного
-- цикла, и по нему считается «завершённый оплаченный месяц».
CREATE TYPE creator_order_status AS ENUM (
    'draft', 'inviting', 'staffed', 'paid', 'cancelled'
);

CREATE TABLE creator_orders (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- start_month — первое число месяца, на который берут креаторов.
    start_month    DATE NOT NULL,
    -- needed — сколько креаторов нужно. Ограничение объёма (1 в первый
    -- месяц, 2-3 дальше) проверяется при создании: здесь уже результат.
    needed         INT  NOT NULL,
    videos_count   INT  NOT NULL,
    status         creator_order_status NOT NULL DEFAULT 'draft',
    -- terms_version_id — с какой версией правил клиент согласился.
    -- Обязателен: без согласия заказ не создаётся.
    terms_version_id UUID NOT NULL REFERENCES terms_versions(id),
    -- project_id — проект, созданный после оплаты. NULL, пока не оплачен.
    project_id     UUID REFERENCES projects(id) ON DELETE SET NULL,
    paid_at        TIMESTAMPTZ,
    cancelled_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT creator_orders_needed_positive CHECK (needed BETWEEN 1 AND 3),
    CONSTRAINT creator_orders_videos_positive CHECK (videos_count > 0),
    -- Месяц — всегда первое число: иначе «сентябрь» и «сентябрь с 3-го»
    -- окажутся разными месяцами при сравнении занятости.
    CONSTRAINT creator_orders_month_is_first CHECK (EXTRACT(DAY FROM start_month) = 1)
);
CREATE INDEX creator_orders_client_idx ON creator_orders(client_user_id, created_at DESC);
-- Незакрытые заказы: их обходит фоновая задача (сгорание приглашений).
CREATE INDEX creator_orders_open_idx ON creator_orders(id)
    WHERE status IN ('draft', 'inviting');

-- 3. Подборка с приоритетом.
--
--   reserve  — в подборке, приглашение не отправлено
--   invited  — приглашение ушло, ждём ответа
--   accepted — согласился
--   declined — отказался
--   expired  — не ответил, приглашение сгорело
--
-- Приоритет обязателен и уникален внутри заказа: клиент не «выбирает N
-- человек», а расставляет собранных по порядку. Приглашения уходят первым
-- needed по списку, остальные ждут освободившегося места.
CREATE TYPE order_candidate_status AS ENUM (
    'reserve', 'invited', 'accepted', 'declined', 'expired'
);

CREATE TABLE order_candidates (
    order_id        UUID NOT NULL REFERENCES creator_orders(id) ON DELETE CASCADE,
    creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    priority        INT  NOT NULL,
    status          order_candidate_status NOT NULL DEFAULT 'reserve',
    invited_at      TIMESTAMPTZ,
    -- expires_at — когда приглашение сгорает и место освобождается.
    expires_at      TIMESTAMPTZ,
    responded_at    TIMESTAMPTZ,
    -- manager_pinged_at — менеджеру написали, что человек молчит сутки.
    -- Отметка нужна, чтобы не писать об этом каждый проход.
    manager_pinged_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, creator_user_id),
    CONSTRAINT order_candidates_priority_positive CHECK (priority > 0)
);
-- Один приоритет на заказ: два «первых» сделали бы порядок бессмысленным.
CREATE UNIQUE INDEX order_candidates_priority_uniq
    ON order_candidates(order_id, priority);
-- «Мои приглашения» у креатора.
CREATE INDEX order_candidates_creator_idx
    ON order_candidates(creator_user_id, status);
-- Узкий индекс под фоновую задачу сгорания.
CREATE INDEX order_candidates_expiring_idx ON order_candidates(expires_at)
    WHERE status = 'invited';

-- 4. Занятость креатора по месяцам. Ведёт сам креатор.
--
-- Клиент видит её ещё при расстановке приоритета — иначе первым в списке
-- окажется тот, кто взять не может, и заказ провисит трое суток впустую.
CREATE TABLE creator_availability (
    creator_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    month           DATE NOT NULL,
    is_available    BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (creator_user_id, month),
    CONSTRAINT creator_availability_month_is_first
        CHECK (EXTRACT(DAY FROM month) = 1)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS creator_availability;
DROP TABLE IF EXISTS order_candidates;
DROP TYPE IF EXISTS order_candidate_status;
DROP TABLE IF EXISTS creator_orders;
DROP TYPE IF EXISTS creator_order_status;
DROP TABLE IF EXISTS client_terms_consents;
DROP TABLE IF EXISTS terms_versions;

-- +goose StatementEnd
