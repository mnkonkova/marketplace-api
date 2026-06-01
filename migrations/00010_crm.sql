-- +goose Up
-- +goose StatementBegin

-- CRM v5 — три кабинета (client/specialist/manager/admin), пайплайны как
-- редактируемые шаблоны (а не зашитые), проекты-инстансы с собственным
-- снэпшотом. Подробно — docs/CRM_V5_BRIEF.md.

-- 1. Роли и аппрув менеджеров.
-- users.kind (client|specialist|both) остаётся: он про маркетплейс-дихотомию
-- (поиск/выдача каталога). users.role — про CRM-разграничение кабинетов.
-- Старые пользователи получают role=client; client/specialist/admin авто-
-- одобрены, manager требует ручного аппрува.
ALTER TABLE users
    ADD COLUMN role        TEXT    NOT NULL DEFAULT 'client',
    ADD COLUMN is_approved BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE users
    ADD CONSTRAINT users_role_check
        CHECK (role IN ('client','specialist','manager','admin'));

-- 2. Продакшены (справочник, управляется админом).
CREATE TABLE productions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Уникальность имени по активным записям: можно деактивировать и завести
-- новое с тем же именем (как у specialist_categories.code, см. 00001).
CREATE UNIQUE INDEX productions_active_name_idx
    ON productions(LOWER(name)) WHERE is_active = TRUE;

-- 3. Выбор продакшена специалистом. XOR: либо production_id, либо
-- is_freelance=true. NULL+false = выбор не сделан (на профиле — попросим).
ALTER TABLE specialist_profiles
    ADD COLUMN production_id UUID NULL REFERENCES productions(id) ON DELETE SET NULL,
    ADD COLUMN is_freelance  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT specialist_profiles_freelance_xor_production
        CHECK (NOT (production_id IS NOT NULL AND is_freelance = TRUE));

-- 4. Пайплайны (шаблоны воронки). Не сидим в коде: админ создаёт через UI.
-- version — задел; редактирование не трогает активные проекты (у них
-- собственный снэпшот в project_stages/project_steps).
CREATE TABLE pipelines (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    version            INT  NOT NULL DEFAULT 1,
    is_active          BOOLEAN NOT NULL DEFAULT TRUE,
    revisions_included INT  NOT NULL DEFAULT 2,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE pipeline_stages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_id UUID NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sort_order  INT  NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pipeline_stages_pipeline_idx ON pipeline_stages(pipeline_id);

CREATE TABLE pipeline_steps (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stage_id      UUID NOT NULL REFERENCES pipeline_stages(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    owner         TEXT NOT NULL CHECK (owner IN ('client','team','system')),
    duration_days INT  NOT NULL DEFAULT 1,
    visible_to_client     BOOLEAN NOT NULL DEFAULT TRUE,
    visible_to_specialist BOOLEAN NOT NULL DEFAULT TRUE,
    weight        INT  NOT NULL DEFAULT 1,
    sort_order    INT  NOT NULL,
    -- is_review помечает финальный шаг «оставить отзыв». При waiting_client
    -- worker ставит дедлайн 7 дней и авто-skip'ает по истечении.
    is_review     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (duration_days > 0),
    CHECK (weight > 0)
);
CREATE INDEX pipeline_steps_stage_idx ON pipeline_steps(stage_id);

-- 5. Проекты — инстансы пайплайна. status — общий жизненный цикл (включает
-- on_hold / dispute). assigned_to_user_id NULL = во входящем пуле менеджеров.
CREATE TYPE project_status AS ENUM (
    'draft','active','on_hold','done','cancelled','dispute'
);
CREATE TYPE step_status AS ENUM (
    'pending','in_progress','waiting_client','done','rejected','skipped'
);
CREATE TYPE project_source AS ENUM (
    'marketplace','manual','referral','returning_client'
);

CREATE TABLE projects (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lead_id             UUID REFERENCES leads(id),
    -- lead_recipient_specialist_id — кто был получателем-исполнителем для
    -- этого лида. lead_recipients имеет composite PK (lead_id, specialist),
    -- поэтому отдельной FK тут нет; уникальность защищаем индексом ниже.
    lead_recipient_specialist_id UUID REFERENCES users(id),
    client_user_id      UUID NOT NULL REFERENCES users(id),
    specialist_user_id  UUID REFERENCES users(id),
    assigned_to_user_id UUID REFERENCES users(id),
    pipeline_id         UUID NOT NULL REFERENCES pipelines(id),
    title               TEXT NOT NULL,
    source              project_source NOT NULL DEFAULT 'manual',
    status              project_status NOT NULL DEFAULT 'draft',
    revisions_included  INT  NOT NULL DEFAULT 2,
    revisions_used      INT  NOT NULL DEFAULT 0,
    budget              INT,
    notes               TEXT,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Один lead+specialist = ровно один проект (защита от двойного клика на
-- создании из принятого получателя лида). Уникальный индекс по обоим полям.
CREATE UNIQUE INDEX projects_lead_recipient_uniq
    ON projects(lead_id, lead_recipient_specialist_id)
    WHERE lead_id IS NOT NULL AND lead_recipient_specialist_id IS NOT NULL;
CREATE INDEX projects_client_idx     ON projects(client_user_id);
CREATE INDEX projects_assigned_idx   ON projects(assigned_to_user_id);
-- Inbox менеджера — частичный индекс по NULL assigned_to.
CREATE INDEX projects_unassigned_idx ON projects(id) WHERE assigned_to_user_id IS NULL;
CREATE INDEX projects_specialist_idx ON projects(specialist_user_id) WHERE specialist_user_id IS NOT NULL;
CREATE INDEX projects_status_idx     ON projects(status);

CREATE TABLE project_stages (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    sort_order   INT  NOT NULL,
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);
CREATE INDEX project_stages_project_idx ON project_stages(project_id);

CREATE TABLE project_steps (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    stage_id     UUID NOT NULL REFERENCES project_stages(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    owner        TEXT NOT NULL CHECK (owner IN ('client','team','system')),
    status       step_status NOT NULL DEFAULT 'pending',
    duration_days INT NOT NULL,
    visible_to_client     BOOLEAN NOT NULL,
    visible_to_specialist BOOLEAN NOT NULL,
    weight       INT NOT NULL,
    sort_order   INT NOT NULL,
    is_review    BOOLEAN NOT NULL DEFAULT FALSE,
    eta_date     DATE,
    -- review_deadline ставится в момент перехода в waiting_client (см. Ф5).
    -- До этого NULL.
    review_deadline TIMESTAMPTZ,
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_steps_project_idx ON project_steps(project_id);
CREATE INDEX project_steps_status_idx  ON project_steps(status);
-- Узкий частичный индекс под фоновый таск авто-skip review-шагов.
CREATE INDEX project_steps_review_idx  ON project_steps(review_deadline)
    WHERE is_review = TRUE AND status = 'waiting_client';

-- 6. Лента активности (источник для widgets/activity-feed). Пишется
-- при каждом переходе шага/стадии и при назначении менеджера.
CREATE TABLE project_step_events (
    id            BIGSERIAL PRIMARY KEY,
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    step_id       UUID REFERENCES project_steps(id) ON DELETE CASCADE,
    actor_user_id UUID REFERENCES users(id),
    actor_type    TEXT NOT NULL DEFAULT 'human'
        CHECK (actor_type IN ('human','system')),
    event_kind    TEXT NOT NULL,
    from_status   step_status,
    to_status     step_status,
    comment       TEXT,
    payload       JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_step_events_project_idx
    ON project_step_events(project_id, created_at DESC);

-- 7. Комментарии. body_format — задел под TipTap; в MVP — 'plain'.
CREATE TABLE project_comments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    author_id   UUID NOT NULL REFERENCES users(id),
    body        TEXT NOT NULL,
    body_format TEXT NOT NULL DEFAULT 'plain'
        CHECK (body_format IN ('plain','html','tiptap_json')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE INDEX project_comments_project_idx
    ON project_comments(project_id, created_at) WHERE deleted_at IS NULL;

-- 8. Magic-link инвайты — для клиентов, заведённых вручную админом
-- (manual / referral source). Храним sha256(token), не raw.
CREATE TABLE client_invites (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (token_hash)
);
CREATE INDEX client_invites_user_active_idx
    ON client_invites(user_id) WHERE used_at IS NULL;

-- 9. Привязка отзыва к проекту (используется в финальном шаге воронки).
ALTER TABLE reviews ADD COLUMN project_id UUID REFERENCES projects(id);
CREATE INDEX reviews_project_idx ON reviews(project_id) WHERE project_id IS NOT NULL;

-- НЕ сидим дефолтный пайплайн: админ создаёт через UI (см. Ф12).
-- Это намеренно — чтобы первый коммит CRM не диктовал воронку, а проверял
-- конструктор end-to-end.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE reviews DROP COLUMN IF EXISTS project_id;

DROP TABLE IF EXISTS client_invites;
DROP TABLE IF EXISTS project_comments;
DROP TABLE IF EXISTS project_step_events;
DROP TABLE IF EXISTS project_steps;
DROP TABLE IF EXISTS project_stages;
DROP TABLE IF EXISTS projects;

DROP TYPE IF EXISTS project_source;
DROP TYPE IF EXISTS step_status;
DROP TYPE IF EXISTS project_status;

DROP TABLE IF EXISTS pipeline_steps;
DROP TABLE IF EXISTS pipeline_stages;
DROP TABLE IF EXISTS pipelines;

ALTER TABLE specialist_profiles
    DROP CONSTRAINT IF EXISTS specialist_profiles_freelance_xor_production,
    DROP COLUMN IF EXISTS is_freelance,
    DROP COLUMN IF EXISTS production_id;

DROP TABLE IF EXISTS productions;

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_role_check,
    DROP COLUMN IF EXISTS is_approved,
    DROP COLUMN IF EXISTS role;

-- +goose StatementEnd
