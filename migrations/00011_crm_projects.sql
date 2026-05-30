-- +goose Up
-- +goose StatementBegin

-- ─── Enum'ы ──────────────────────────────────────────────────────────
-- project_status — состояние проекта в целом.
-- step_status — состояние шага. waiting_client — отдельный статус, не
-- подвид in_progress (по решению из брифа); skipped для опциональных
-- шагов с тайм-аутом (например, review через 7 дней без действия).
-- project_source — откуда пришёл проект; используется для отдельного
-- расчёта GMV маркетплейса и комиссии (на MVP без комиссии).
CREATE TYPE project_status AS ENUM (
    'draft','active','on_hold','done','cancelled','dispute'
);
CREATE TYPE step_status AS ENUM (
    'pending','in_progress','waiting_client','done','rejected','skipped'
);
CREATE TYPE project_source AS ENUM (
    'marketplace','manual','referral','returning_client'
);

-- ─── Шаблон услуги ───────────────────────────────────────────────────
-- service_templates версионируются. При StartProject копируем стадии
-- и шаги в project_stages/_steps (snapshot) — дальнейшие правки шаблона
-- не задевают живые проекты. На MVP активна одна версия video_production.
CREATE TABLE service_templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code        TEXT NOT NULL,
    version     INT  NOT NULL,
    title       TEXT NOT NULL,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    revisions_included INT NOT NULL DEFAULT 2,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (code, version)
);

CREATE TABLE service_template_stages (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES service_templates(id) ON DELETE CASCADE,
    code        TEXT NOT NULL,
    title       TEXT NOT NULL,
    sort_order  INT  NOT NULL,
    UNIQUE (template_id, code)
);

CREATE TABLE service_template_steps (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stage_id     UUID NOT NULL REFERENCES service_template_stages(id) ON DELETE CASCADE,
    code         TEXT NOT NULL,
    title        TEXT NOT NULL,
    owner        TEXT NOT NULL CHECK (owner IN ('client','team','system')),
    duration_days INT NOT NULL DEFAULT 1,
    visible_to_client     BOOLEAN NOT NULL DEFAULT TRUE,
    visible_to_specialist BOOLEAN NOT NULL DEFAULT TRUE,
    weight       INT NOT NULL DEFAULT 1,
    sort_order   INT NOT NULL,
    UNIQUE (stage_id, code)
);

-- ─── Проекты ─────────────────────────────────────────────────────────
-- Один проект = один договор продакшен ↔ клиент по одной услуге.
-- lead_id заполнен, если проект пришёл из маркетплейса (source=marketplace);
-- ссылка на конкретный recipient выражается через composite FK
-- (lead_id, specialist_user_id) → lead_recipients (см. ниже).
-- assigned_to_user_id — менеджер платформы (admin/moderator).
CREATE TABLE projects (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    lead_id             UUID REFERENCES leads(id),
    client_user_id      UUID NOT NULL REFERENCES users(id),
    specialist_user_id  UUID REFERENCES users(id),
    assigned_to_user_id UUID REFERENCES users(id),
    template_id         UUID NOT NULL REFERENCES service_templates(id),
    title               TEXT NOT NULL,
    source              project_source NOT NULL DEFAULT 'manual',
    status              project_status NOT NULL DEFAULT 'draft',
    revisions_included  INT NOT NULL DEFAULT 2,
    revisions_used      INT NOT NULL DEFAULT 0,
    budget              INT,
    notes               TEXT,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Composite FK на recipient. lead_recipients имеет PK
    -- (lead_id, specialist_user_id) — это естественный ключ маркетплейса.
    -- Если recipient удалится (CASCADE из leads), у проекта зануляются
    -- оба столбца — проект остаётся как «свой клиент», аудит-связь
    -- теряется. На практике recipient'ы не удаляют.
    CONSTRAINT projects_lead_recipient_fk
        FOREIGN KEY (lead_id, specialist_user_id)
        REFERENCES lead_recipients(lead_id, specialist_user_id)
        ON DELETE SET NULL
        DEFERRABLE INITIALLY DEFERRED
);

-- Один проект на один recipient — partial unique по (lead_id, specialist_user_id)
-- только когда оба заполнены (manual-проекты их NULL имеют легально).
CREATE UNIQUE INDEX projects_recipient_unique_idx
    ON projects(lead_id, specialist_user_id)
    WHERE lead_id IS NOT NULL AND specialist_user_id IS NOT NULL;

CREATE INDEX projects_client_idx     ON projects(client_user_id);
CREATE INDEX projects_specialist_idx ON projects(specialist_user_id) WHERE specialist_user_id IS NOT NULL;
CREATE INDEX projects_assigned_idx   ON projects(assigned_to_user_id) WHERE assigned_to_user_id IS NOT NULL;
CREATE INDEX projects_lead_idx       ON projects(lead_id) WHERE lead_id IS NOT NULL;
CREATE INDEX projects_status_idx     ON projects(status);
CREATE INDEX projects_source_idx     ON projects(source);

CREATE TABLE project_stages (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    code         TEXT NOT NULL,
    title        TEXT NOT NULL,
    sort_order   INT  NOT NULL,
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (project_id, code)
);
CREATE INDEX project_stages_project_idx ON project_stages(project_id);

-- project_steps — snapshot из шаблона. eta_date считается в Go синхронно
-- при переходе шага в in_progress (бриф §11: «не делай ETA как cron»).
-- cta_payload — JSON-данные для кнопки в кабинете клиента (контекст для
-- UI; на бэк не влияет).
CREATE TABLE project_steps (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    stage_id     UUID NOT NULL REFERENCES project_stages(id) ON DELETE CASCADE,
    code         TEXT NOT NULL,
    title        TEXT NOT NULL,
    owner        TEXT NOT NULL,
    status       step_status NOT NULL DEFAULT 'pending',
    duration_days INT NOT NULL,
    visible_to_client     BOOLEAN NOT NULL,
    visible_to_specialist BOOLEAN NOT NULL,
    weight       INT NOT NULL,
    sort_order   INT NOT NULL,
    eta_date     DATE,
    cta_payload  JSONB,
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (stage_id, code)
);
CREATE INDEX project_steps_project_idx ON project_steps(project_id);
CREATE INDEX project_steps_status_idx  ON project_steps(status);

-- Аудит переходов. Источник истины для «кто, когда, что менял».
-- actor_type разделяет человек/service-account/system (auto-skip).
CREATE TABLE project_step_events (
    id            BIGSERIAL PRIMARY KEY,
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    step_id       UUID NOT NULL REFERENCES project_steps(id) ON DELETE CASCADE,
    actor_user_id UUID REFERENCES users(id),
    actor_type    TEXT NOT NULL DEFAULT 'human'
                    CHECK (actor_type IN ('human','service','system')),
    from_status   step_status,
    to_status     step_status NOT NULL,
    comment       TEXT,
    payload       JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_step_events_project_idx
    ON project_step_events(project_id, created_at DESC);

-- Magic-link инвайты для клиентов, заведённых менеджером вручную.
-- token_hash — bcrypt от plaintext (raw уезжает один раз в email).
-- TTL 7 дней — клиент должен успеть открыть письмо.
CREATE TABLE client_invites (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    used_at       TIMESTAMPTZ,
    created_by    UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (token_hash)
);
CREATE INDEX client_invites_user_idx ON client_invites(user_id);
CREATE INDEX client_invites_expires_idx
    ON client_invites(expires_at) WHERE used_at IS NULL;

-- Привязка ревью к проекту (для фильтрации «отзыв по этому проекту»
-- в админке и для финального шага review).
ALTER TABLE reviews ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id);
CREATE INDEX IF NOT EXISTS reviews_project_idx
    ON reviews(project_id) WHERE project_id IS NOT NULL;

-- ─── SEED шаблона video_production v1 ────────────────────────────────
-- Воронка зафиксирована брифом §3 (17 шагов в 6 стадиях). ON CONFLICT
-- DO NOTHING — идемпотентно на случай повторного up.

INSERT INTO service_templates (code, version, title, revisions_included)
VALUES ('video_production', 1, 'Видеопродакшен', 2)
ON CONFLICT (code, version) DO NOTHING;

-- Стадии через WITH-CTE: подставляем template_id из только что INSERT'a.
WITH tmpl AS (
    SELECT id FROM service_templates WHERE code='video_production' AND version=1
)
INSERT INTO service_template_stages (template_id, code, title, sort_order)
SELECT tmpl.id, v.code, v.title, v.sort_order
FROM tmpl, (VALUES
    ('start',      'Старт',          1),
    ('prep',       'Подготовка',     2),
    ('shooting',   'Съёмка',         3),
    ('editing',    'Монтаж',         4),
    ('acceptance', 'Приёмка',        5),
    ('delivery',   'Доставка',       6)
) AS v(code, title, sort_order)
ON CONFLICT (template_id, code) DO NOTHING;

-- Шаги. Source-of-truth таблица — раздел 3 брифа: stage_code, step_code,
-- title, duration_days, owner, visible_to_client, visible_to_specialist,
-- weight, sort_order.
WITH tmpl AS (
    SELECT id FROM service_templates WHERE code='video_production' AND version=1
),
stages AS (
    SELECT s.id AS stage_id, s.code AS stage_code
    FROM service_template_stages s
    JOIN tmpl ON s.template_id = tmpl.id
)
INSERT INTO service_template_steps (
    stage_id, code, title, owner, duration_days,
    visible_to_client, visible_to_specialist, weight, sort_order
)
SELECT stages.stage_id, v.step_code, v.title, v.owner, v.duration_days,
       v.vis_client, v.vis_spec, v.weight, v.sort_order
FROM stages
JOIN (VALUES
    ('start',      'payment',           'Оплата',                       1, 'client', TRUE,  FALSE, 1, 1),
    ('start',      'escrow_hold',       'Резерв средств',               0, 'system', TRUE,  FALSE, 1, 2),
    ('prep',       'strategy',          'Медиастратегия',               2, 'team',   TRUE,  TRUE,  2, 1),
    ('prep',       'script',            'Сценарий',                     2, 'team',   TRUE,  TRUE,  2, 2),
    ('prep',       'script_approval',   'Согласование сценария',        1, 'client', TRUE,  TRUE,  1, 3),
    ('prep',       'social_setup',      'Настройка соцаккаунтов',       2, 'team',   FALSE, FALSE, 2, 4),
    ('shooting',   'shoot_prep',        'Подготовка к съёмке',          5, 'team',   TRUE,  TRUE,  5, 1),
    ('shooting',   'shoot_day',         'Съёмочный день',               1, 'team',   TRUE,  TRUE,  1, 2),
    ('editing',    'rough_cut',         'Черновой монтаж',              7, 'team',   TRUE,  TRUE,  7, 1),
    ('editing',    'final_cut',         'Чистовой монтаж',              3, 'team',   TRUE,  TRUE,  3, 2),
    ('editing',    'internal_approval', 'Внутренний апрув',             1, 'team',   FALSE, TRUE,  1, 3),
    ('acceptance', 'client_review',     'Видео на согласование',        2, 'client', TRUE,  TRUE,  2, 1),
    ('acceptance', 'revision_round',    'Раунд правок',                 0, 'system', TRUE,  FALSE, 0, 2),
    ('delivery',   'publishing',        'Публикация',                   1, 'team',   TRUE,  TRUE,  1, 1),
    ('delivery',   'report',            'Отчёт',                        2, 'team',   TRUE,  TRUE,  2, 2),
    ('delivery',   'nps',               'Оценка NPS',                   1, 'client', TRUE,  FALSE, 1, 3),
    ('delivery',   'review',            'Отзыв на исполнителя',         1, 'client', TRUE,  FALSE, 1, 4)
) AS v(stage_code, step_code, title, duration_days, owner, vis_client, vis_spec, weight, sort_order)
  ON stages.stage_code = v.stage_code
ON CONFLICT (stage_id, code) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS reviews_project_idx;
ALTER TABLE reviews DROP COLUMN IF EXISTS project_id;

DROP TABLE IF EXISTS client_invites;
DROP TABLE IF EXISTS project_step_events;
DROP TABLE IF EXISTS project_steps;
DROP TABLE IF EXISTS project_stages;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS service_template_steps;
DROP TABLE IF EXISTS service_template_stages;
DROP TABLE IF EXISTS service_templates;

DROP TYPE IF EXISTS project_source;
DROP TYPE IF EXISTS step_status;
DROP TYPE IF EXISTS project_status;

-- +goose StatementEnd
