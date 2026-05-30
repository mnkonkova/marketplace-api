# Задача: основной CRM — проекты клиента, воронка, админка, нотификации

Это финальный бриф для тебя, Claude Code. Прочти **полностью** перед началом.
Главный принцип: **не пересоздавай существующее**. Это четвёртая задача
в серии, перед ней должны быть прогнаны:
1. Поле «Продакшен» — сущность `productions` и базовая настройка Directus
   с ролями `platform_admin` и `platform_moderator`.

Если первая задача ещё не прогнана — **остановись и спроси**.

Репозитории:
- `marketplace-api` — Go-бэкенд.
- `marketplace-web` — Angular 19 фронт.

Инфраструктура, которая **уже** должна быть поднята:
- Directus на `admin.{DOMAIN}` с двумя ролями.
- Service-account user в `users` с `role='admin'` для Directus.
- Минимум 1 admin + 2 moderator в Directus.

Новое в этом бриф:
- **n8n** на `automation.{DOMAIN}` для нотификаций (self-hosted Community = $0).

---

## 0. Что прочитать перед стартом

### В `marketplace-api`:
1. `CLAUDE.md`, `README.md`, `docs/DEPLOY.md`.
2. `internal/leads/` — целиком, **референсный домен**.
3. `internal/productions/` — сделано в предыдущей задаче.
4. `internal/reviews/` — для интеграции в финал воронки.
5. `internal/outbox/` — будем расширять.
6. `cmd/worker/main.go` — индексер outbox. **Расширяем webhook-диспатчером.**
7. `internal/auth/` — `RequireAdmin`, service-account токены.
8. `migrations/` — стиль и порядковый номер последней миграции.
9. `docker-compose.prod.yml` — текущая инфра (Directus уже там).

### В `marketplace-web`:
1. `web/src/pages/cabinet/` — кабинет специалиста с табами (Профиль/Продакшен).
   Раздел «Назначенные проекты» добавится во вкладку «Продакшен».
2. `web/src/entities/me/`, `entities/production/` — эталоны entity.
3. `web/src/app/app.routes.ts` — текущие маршруты.
4. `widgets/app-header/` — навигация.

### Внешние сервисы:
1. n8n docs: `docs.n8n.io` — Webhook trigger, HTTP Request node,
   Email/Telegram nodes, Workflow versioning.
2. Directus docs: Flows, HTTP Request operation, custom-buttons.

Если что-то непонятно — **спроси меня**, не угадывай.

---

## 1. Контекст продукта (важно для решений)

**Управляемый сервис с витриной**, не self-service маркетплейс:
менеджер ведёт каждый проект в Directus, клиент видит результат
в ЛК. Внешние команды подключатся к 3 месяцу.

**Ключевое архитектурное решение этого бриф:** CRM не привязана
к маркетплейсу. Она ведёт **все проекты продакшена**, включая
«свои» (прямые клиенты, реферралы, возвращающиеся клиенты). Это
снимает риск «CRM мёртвая первые 2–3 месяца, пока маркетплейс
не наберёт трафик» — система работает с дня 1.

Источники проектов помечаются полем `source`:
- `marketplace` — пришёл через лид с витрины
- `manual` — менеджер завёл вручную
- `referral` — пришёл по рекомендации
- `returning_client` — повторный клиент

Все идут по **одной воронке** видеопродакшена (на MVP — одна
воронка, один тип услуги). Различие только в:
- метриках (отдельный GMV для маркетплейса)
- комиссии (берётся только с `marketplace`-проектов; на MVP вообще
  без комиссий, биллинг позже)
- инвайт-флоу (для своих клиентов — magic-link, для маркетплейс-
  клиентов — обычная регистрация)

### Архитектурное разделение ответственности

С Directus и n8n у нас четыре точки доступа к данным проекта:

| Зона | Кто | Что делает |
|---|---|---|
| **Бизнес-логика мутаций** | Go API | Переходы стейт-машины, создание проекта, цикл правок, расчёт ETA, создание клиентов, инвайт-токены. **Единственный источник правды для мутаций.** |
| **Чтение и просмотр** | Directus | Списки, drill-in, фильтры, дашборды. Read-only на бизнес-таблицы. |
| **Ручные правки whitelist-полей** | Directus | `projects.title/budget/notes`, `projects.assigned_to_user_id`. **Только эти поля**, ничего другого. |
| **Опасные действия со стейт-машиной** | Directus → Go API | Кнопки в Directus вызывают наши endpoints через **Flows** (HTTP Request). Прямой write в `project_steps` запрещён permissions. |
| **Нотификации и инвайты** | Outbox → n8n | Worker слушает outbox, диспатчит webhook'и в n8n. n8n шлёт email/Telegram. |
| **Клиентский ЛК** | Angular `/me/projects/*` | Через Go API напрямую. Polling 30 сек. |
| **Кабинет специалиста (Назначенные проекты)** | Angular `/me`, таб «Продакшен» | Через Go API напрямую. Read-only по Варианту А. |

---

(Полный бриф зафиксирован в репо как источник истины. Тело документа
повторяет тело сообщения пользователя; см. сообщение от 2026-05-30,
которое инициировало эту задачу. Здесь сохранены секции 1, 2 и
содержание; остальные секции — модель данных, воронка, стейт-машина,
backend-домен, API, outbox/n8n, frontend, Directus, n8n, фазы, DoD,
точки риска — приведены ниже в исходном виде, без сокращений.)

---

## 2. Модель данных

### 2.1 Миграция

```sql
-- +goose Up

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
    visible_to_client BOOLEAN NOT NULL DEFAULT TRUE,
    visible_to_specialist BOOLEAN NOT NULL DEFAULT TRUE,
    weight       INT NOT NULL DEFAULT 1,
    sort_order   INT NOT NULL,
    UNIQUE (stage_id, code)
);

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
    lead_recipient_id   UUID REFERENCES lead_recipients(id),
    client_user_id      UUID NOT NULL REFERENCES users(id),
    specialist_user_id  UUID REFERENCES users(id),
    assigned_to_user_id UUID REFERENCES users(id),         -- менеджер платформы (admin/moderator)
    template_id         UUID NOT NULL REFERENCES service_templates(id),
    title               TEXT NOT NULL,
    source              project_source NOT NULL DEFAULT 'manual',
    status              project_status NOT NULL DEFAULT 'draft',
    revisions_included  INT NOT NULL DEFAULT 2,
    revisions_used      INT NOT NULL DEFAULT 0,
    budget              INT,
    notes               TEXT,                              -- для Directus ручных правок
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Один проект на один recipient (recipient уникально маппится в проект)
    UNIQUE (lead_recipient_id)
);
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

CREATE TABLE project_step_events (
    id            BIGSERIAL PRIMARY KEY,
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    step_id       UUID NOT NULL REFERENCES project_steps(id) ON DELETE CASCADE,
    actor_user_id UUID REFERENCES users(id),
    actor_type    TEXT NOT NULL DEFAULT 'human',           -- 'human' | 'service' | 'system'
    from_status   step_status,
    to_status     step_status NOT NULL,
    comment       TEXT,
    payload       JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_step_events_project_idx
    ON project_step_events(project_id, created_at DESC);

-- Magic-link инвайты для клиентов (созданных вручную)
CREATE TABLE client_invites (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,                           -- bcrypt от токена
    expires_at    TIMESTAMPTZ NOT NULL,
    used_at       TIMESTAMPTZ,
    created_by    UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (token_hash)
);
CREATE INDEX client_invites_user_idx ON client_invites(user_id);
CREATE INDEX client_invites_expires_idx ON client_invites(expires_at) WHERE used_at IS NULL;

-- Привязка ревью к проекту (опционально, для удобства фильтрации в админке)
ALTER TABLE reviews ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id);
CREATE INDEX IF NOT EXISTS reviews_project_idx ON reviews(project_id) WHERE project_id IS NOT NULL;

-- SEED шаблона video_production v1 — все стадии и шаги по таблице ниже
INSERT INTO service_templates (code, version, title, revisions_included)
VALUES ('video_production', 1, 'Видеопродакшен', 2);
-- ... INSERT'ы стадий и шагов, см. раздел 3 ...

-- +goose Down
-- (обратные DROP'ы в правильном порядке)
```

### 2.2 Замечание про `lead_recipients`

Используем существующую таблицу `lead_recipients` из домена leads.
**Не дублируй** её и **не меняй** статус-машину recipient'а — она
работает. Только добавляем:
- Admin-endpoint для смены статуса recipient'а от лица менеджера
  (для «акцептить за свой продакшен»).
- Хук «recipient → accepted → проект может быть создан».

---

## 3. Воронка видеопродакшена (зафиксирована — не менять)

| stage_code | step_code | Шаг | Срок (д) | owner | visible_to_client | visible_to_specialist | weight |
|---|---|---|---|---|---|---|---|
| `start` | `payment` | Оплата | 1 | client | true | false | 1 |
| `start` | `escrow_hold` | Резерв средств | 0 | system | true | false | 1 |
| `prep` | `strategy` | Медиастратегия | 2 | team | true | true | 2 |
| `prep` | `script` | Сценарий | 2 | team | true | true | 2 |
| `prep` | `script_approval` | Согласование сценария | 1 | client | true | true | 1 |
| `prep` | `social_setup` | Настройка соцаккаунтов | 2 | team | **false** | false | 2 |
| `shooting` | `shoot_prep` | Подготовка к съёмке | 5 | team | true | true | 5 |
| `shooting` | `shoot_day` | Съёмочный день | 1 | team | true | true | 1 |
| `editing` | `rough_cut` | Черновой монтаж | 7 | team | true | true | 7 |
| `editing` | `final_cut` | Чистовой монтаж | 3 | team | true | true | 3 |
| `editing` | `internal_approval` | Внутренний апрув | 1 | team | **false** | true | 1 |
| `acceptance` | `client_review` | Видео на согласование | 2 | client | true | true | 2 |
| `acceptance` | `revision_round` | Раунд правок | 0 | system | true | false | 0 |
| `delivery` | `publishing` | Публикация | 1 | team | true | true | 1 |
| `delivery` | `report` | Отчёт | 2 | team | true | true | 2 |
| `delivery` | `nps` | Оценка NPS | 1 | client | true | false | 1 |
| `delivery` | `review` | Отзыв на исполнителя | 1 | client | true | **false** | 1 |

**Новое:** добавлена колонка `visible_to_specialist` и шаг `review`
в финале. Шаг `review`:
- `owner=client`, `visible_to_specialist=false` (специалист не должен
  видеть процесс заполнения отзыва о себе)
- Использует существующий `internal/reviews/`
- При завершении пишет запись в `reviews` с `project_id=current`
- Опциональный — если клиент не оставляет отзыв в течение 7 дней,
  шаг авто-`skipped`, проект → `done`

---

## 4. Стейт-машина и цикл правок

### 4.1 Статусы

```
pending → in_progress → done
                     → waiting_client → done
                                     → rejected → in_progress
                     → skipped (для не-required шагов с тайм-аутом)
```

`waiting_client` — отдельный статус, **не подвид `in_progress`**.

`skipped` — новый статус для опциональных шагов с тайм-аутом
(например, `review` не заполнен за 7 дней).

### 4.2 Цикл правок (без изменений от v3)

`revisions_included=2` (default), `revisions_used`.

Клиент на `client_review` → «Правки»:
1. `client_review` → `rejected`
2. Если лимит не исчерпан → `final_cut` → `in_progress`, `revisions_used += 1`
3. После доработки → `client_review` → `waiting_client`
4. Лимит исчерпан → проект → `dispute`, нотификация менеджеру

### 4.3 Snapshot шаблона

При старте проекта (`StartProject`):
- Из `service_template_stages` и `_steps` копируются стадии и шаги
  в `project_stages` и `project_steps`.
- Шаблон в проекте больше не меняется.
- Защита от рассинхронизации при правке шаблона админом.

---

## 5. Структура backend-домена

```
internal/projects/
├── dto.go                — ProjectClientView, ProjectAdminView,
│                           ProjectSpecialistView (новое), enum'ы,
│                           CreateManualInput, CreateFromRecipientInput
├── repo.go               — pgx SQL, транзакции
├── service.go            — StartProjectManual, StartFromRecipient,
│                           StartFromAllAcceptedRecipients (bulk),
│                           AdvanceStep, RequestRevision, ApproveStep,
│                           GetClientView, GetSpecialistView,
│                           GetAdminView
├── handlers.go           — HTTP роуты клиента
├── handlers_specialist.go — HTTP роуты специалиста (read-only)
├── handlers_admin.go     — HTTP роуты админа (Directus вызывает)
├── template.go           — Snapshot
├── progress.go           — расчёт по весам
├── source.go             — auto-detect source по lead_id
└── events.go             — outbox + типизированные события

internal/invites/
├── dto.go                — Invite, GenerateInput
├── repo.go
├── service.go            — Generate, Redeem, Cleanup expired
└── handlers_admin.go     — /admin/users/{id}/generate_invite
                            /auth/redeem_invite/{token}

internal/notifications/   — расширение worker'а
├── dispatcher.go         — outbox → webhook n8n
├── config.go             — мэппинг event_type → webhook URL
└── retry.go              — exponential backoff
```

Зарегистрировать в `cmd/api/main.go` и `internal/httpapi/router.go`.

---

## 6. API контракты

### 6.1 ЛК клиента (auth + soft-gate email_verified + role=client)

```
GET    /api/v1/me/projects
GET    /api/v1/me/projects/{id}
GET    /api/v1/me/projects/{id}/funnel
GET    /api/v1/me/projects/by_lead/{lead_id}   — все проекты по одному брифу
POST   /api/v1/me/projects/{id}/steps/{step_id}/approve
POST   /api/v1/me/projects/{id}/steps/{step_id}/request_revision
POST   /api/v1/me/projects/{id}/steps/{step_id}/submit_review
       body: { rating: 1..5, text: string }
```

Фильтрация по `visible_to_client=true` в SQL. Проверка
`client_user_id == current_user_id` обязательна.

### 6.2 Кабинет специалиста (auth + role=specialist)

```
GET    /api/v1/me/specialist/projects
GET    /api/v1/me/specialist/projects/{id}
GET    /api/v1/me/specialist/projects/{id}/funnel
```

Фильтрация по `visible_to_specialist=true`. Проверка
`specialist_user_id == current_user_id` обязательна.

**Read-only** на стейт-машину. Никаких мутаций (Вариант А).

### 6.3 Admin API (для Directus Flows и моderator-UI)

```
GET    /api/v1/admin/projects
       ?status=&client=&specialist=&source=&overdue=true&assigned=
GET    /api/v1/admin/projects/{id}
POST   /api/v1/admin/projects                     — создать вручную
                                                    (source = manual|referral|returning_client)
POST   /api/v1/admin/projects/from_recipient/{recipient_id}
                                                    (source = marketplace)
POST   /api/v1/admin/projects/from_lead/{lead_id}/bulk
                                                    — для всех accepted recipients
                                                    разом
PATCH  /api/v1/admin/projects/{id}                 — title, budget, notes,
                                                    assigned_to_user_id, status
                                                    (optimistic lock через updated_at)
POST   /api/v1/admin/projects/{id}/steps/{step_id}/start
POST   /api/v1/admin/projects/{id}/steps/{step_id}/complete
POST   /api/v1/admin/projects/{id}/steps/{step_id}/skip   (с комментарием)
GET    /api/v1/admin/projects/{id}/events

POST   /api/v1/admin/lead_recipients/{id}/accept   — менеджер «акцептит за
                                                    свой продакшен»
PATCH  /api/v1/admin/lead_recipients/{id}          — общее изменение статуса
                                                    recipient'а

POST   /api/v1/admin/users                         — создать клиента вручную
       body: { email, name, send_invite: bool }
POST   /api/v1/admin/users/{id}/generate_invite    — magic-link для существующего
```

### 6.4 Magic-link redemption (публичный, no auth)

```
POST   /api/v1/auth/redeem_invite/{token}
       — проверяет токен, выставляет user.email_verified=true,
         выдаёт JWT
```

### 6.5 Optimistic lock

Все PATCH-эндпоинты на `projects` и `project_steps` требуют
`updated_at` в теле запроса. Если в БД свежее — 409 Conflict.

Паттерн — **ровно как в `internal/leads/`** для recipient'ов.

---

## 7. Outbox события и n8n диспатч

(см. полное тело брифа в исходном сообщении пользователя; ключевые
правила сохранены ниже)

### 7.1 События

`event_type`: `step.transitioned`, `project.created`,
`project.completed`, `project.disputed`, `revision.requested`,
`client_invite.generated`.

`payload`: project_id, project_source, lead_id, step_id, step_code,
step_owner, from, to, actor_user_id, actor_type, client_user_id,
client_email, client_name, specialist_user_id, specialist_email,
assigned_to_user_id, manager_email, occurred_at.

### 7.2 Расширение worker'а

В `cmd/worker/main.go` добавить **параллельный consumer** для
webhook'ов n8n. Правила диспатча — см. таблицу в брифе.

URL — `N8N_WEBHOOK_BASE_URL` env. n8n маршрутизирует по `event_type`
внутри одного workflow.

### 7.3 Идемпотентность

В webhook передаём `event_id` (id записи в outbox). В n8n flow:
Redis GET по `event_id` → exit если есть, иначе SET с TTL 24ч.

---

## 8. Frontend Angular

### 8.1 ЛК клиента (новое)
- `entities/project/` (Repository + types)
- `widgets/project-card/`, `project-progress-bar/`, `funnel-stage/`
- `features/project-funnel/`, `features/project-actions/`
- `pages/me/projects-list/`, `pages/me/project-detail/`
- `shared/guards/client-role.guard.ts`
- Маршруты: `/me/projects`, `/me/projects/:id`
- Polling 30 сек. Группировка по `lead_id` на списке.

### 8.2 Кабинет специалиста — расширение вкладки «Продакшен»
- Новая секция `assigned-projects/` в существующей вкладке.
- Read-only карточки + переход на detail-страницу.
- `pages/me/specialist-project-detail/` — read-only.
- Polling 60 сек.
- Маршрут: `/me/specialist/projects/:id`.

### 8.3 Кнопки в `app-header`
- `role=client` → «Мои проекты».
- `role=specialist` → «Назначенные проекты».
- `role=admin|moderator` → ссылка на Directus.

### 8.4 Что НЕ строить
- Никакой админки на Angular. Админка — Directus.
- Никаких форм создания проектов вручную на Angular — в Directus.

---

## 9. Directus — расширение существующей установки

### 9.1 Новые коллекции
projects, project_stages, project_steps, project_step_events (RO),
service_templates*, client_invites.

### 9.2 Permissions
- `platform_admin`: Read all, Write whitelist
  (`projects.title/budget/notes/assigned_to_user_id/status` +
  `service_template_steps.duration_days/weight`).
- `platform_moderator`: то же, кроме `service_template_*` (RO).
- Никакого write на `project_steps.status`, `project_stages.*`,
  `project_step_events.*`, `*.created_at`, `*.updated_at`.

### 9.3 Flows (9 кнопок)
Accept recipient, Create project from recipient, Bulk create from
lead, Create manual project, Start/Complete/Skip step, Send client
invite, Change project status. Все через HTTP Request → Go API.

### 9.4 Filter presets
projects: «Все активные», «Маркетплейс», «Свои», «На приёмке у
клиента», «Просроченные», «Мои проекты».

### 9.5 Dashboards
- CRM Overview: статусы (отдельно marketplace и свои), просроченные,
  NPS, время в стадиях, воронка по `source`.
- Marketplace KPI: только `source=marketplace`, GMV, конверсия,
  средний чек.

---

## 10. n8n

### 10.1 Docker compose
Сервис `n8n` (image: n8nio/n8n:latest) на :5678, basic-auth.
Caddyfile блок `automation.{$DOMAIN}`. DNS A-запись.

### 10.2 Базовые workflows
- `project-client-notification` — webhook + switch по event_type +
  idempotency через Redis + Email SMTP Yandex.
- `client-invite` — magic-link письмо.
- `manager-notification` — Telegram на `TG_MANAGER_CHAT_ID`.

### 10.3 Бэкап workflows
Экспорт в `ops/n8n/workflows/`. Makefile-таска `make export-n8n`.

---

## 11. Что НЕ делать

- Не дублировать `leads`, `reviews`, `productions`, `auth`.
- Не поднимать Directus заново — уже стоит.
- Не пересоздавать роли — расширять permissions.
- Не WebSocket. Polling.
- Не свой workflow engine.
- Не cron для ETA — синхронно при переходе.
- Не давать Directus write на `project_steps.status` — только flow.
- Не SMTP/Telegram в Go — это n8n.
- Не админка на Angular.
- Не NgModule / ngIf / constructor injection.
- Не плодить UI-киты — только ng-zorro.
- Не открытым в БД инвайт-токен — `bcrypt(token)`.
- TTL инвайта — 7 дней.

---

## 12. Фазы

1. Backend: миграция, шаблон, репо (1.5–2д)
2. Backend: создание проекта + клиентское API (2д)
3. Backend: специалист API (0.5д)
4. Backend: переходы + admin API (2д)
5. Backend: invites и manual-флоу (1д)
6. Backend: outbox диспатчер для n8n (1д)
7. n8n: поднятие и workflows (1.5д)
8. Directus: коллекции, права, flows (1.5д)
9. Frontend клиент (3д)
10. Frontend специалист (1д)
11. Прогресс, метрики, swagger (0.5д)

После каждой — `make lint && make test`, `npm run format:check && ng build`,
ручная проверка, ревью.

---

## 13. Definition of Done

См. полный список в брифе. Ключевое: 5 end-to-end сценариев
приёмки — маркетплейс, свой клиент, цикл правок, специалист,
концурент-edit модераторов.

---

## 14. Точки риска

1. RAM VDS под Directus + n8n (~+500 МБ).
2. DNS `automation.{DOMAIN}`.
3. SMTP-кредитки Yandex Mail.
4. Telegram-бот (`TG_BOT_TOKEN`, `TG_MANAGER_CHAT_ID`).
5. `users.role` — уже есть из productions-задачи.
6. `DIRECTUS_SERVICE_TOKEN` в `.env.prod`.

---

## 15. Первый коммит

1. В обоих репо ветка `feature/crm-projects-and-notifications`.
2. Бриф в `docs/CRM_PROJECTS_BRIEF.md`.
3. Пустые коммиты.
4. До Фазы 1 — 7-10 строк анализа.
