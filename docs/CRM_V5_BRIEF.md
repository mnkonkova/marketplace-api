# CRM v5 — три кабинета, редактор пайплайнов, канбан (консолидированный бриф)

Это **единый финальный бриф** для CRM-надстройки над базовым discovery-маркетплейсом
(marketplace-api + marketplace-web). Замещает все предыдущие промпты по CRM. Гит откатан
до `main`, строим заново поверх базового состояния (auth / catalog / profiles / leads /
search / reviews).

Главный принцип: **не пересоздавать то, что есть на main**. Базовые домены работают —
встраиваемся.

Репозитории:
- `marketplace-api` — Go (chi, pgx, Postgres, OpenSearch, Redis, S3).
- `marketplace-web` — Angular 19 (ng-zorro, FSD, signals, standalone).

Инфраструктура: n8n self-hosted для нотификаций (Community = $0).

---

## 0. Что прочитать перед стартом

### marketplace-api
1. `CLAUDE.md`, `README.md`, `docs/DEPLOY.md`.
2. `internal/leads/` — **референсный домен** (repo/service/handlers/dto).
3. `internal/catalog/` — референс для справочников (productions по образцу).
4. `internal/profiles/` — расширим (выбор продакшена).
5. `internal/reviews/` — интеграция в финал воронки.
6. `internal/outbox/`, `cmd/worker/main.go` — события, расширим для n8n.
7. `internal/auth/` — JWT, middleware, роли.
8. `migrations/` — стиль, номер последней.

### marketplace-web
1. `README.md` — FSD, запуск.
2. `web/src/pages/cabinet/` — кабинет специалиста.
3. `web/src/entities/me/`, `entities/category/` — эталоны entity.
4. `web/src/features/auth/` — AuthSessionStore.
5. `web/src/widgets/app-header/` — навигация.
6. `web/src/app/app.routes.ts`.

Если что-то непонятно — спрашиваем, не угадываем.

---

## 1. Роли и кабинеты

Четыре роли в `users.role`: `client | specialist | manager | admin`.

| Кабинет | Роль | Что делает |
|---|---|---|
| **Клиентский** | client | Видит состояние своего проекта (стадии, прогресс, статусы). Апрувит сценарий и готовое видео. Оставляет отзыв. |
| **Специалистский** | specialist | Профиль + выбор продакшена. Видит назначенные проекты (read-only). |
| **Менеджерский** | manager | Берёт входящие проекты без ответственного. Ведёт их по канбану. Требует аппрува админа. |
| **Админский** | admin | Добавляет продакшены. Аппрувит менеджеров. Создаёт и редактирует пайплайны (с временем шагов). |

`manager` требует **аппрува**: регистрируется → `is_approved=false` → админ аппрувит →
`is_approved=true`. До аппрува в кабинет менеджера не пускаем.

---

## 2. Модель данных (одна миграция)

`migrations/00010_crm.sql`, стиль как `00001_init.sql`.

```sql
-- +goose Up

-- Роли и аппрув менеджеров
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'client',
  ADD COLUMN IF NOT EXISTS is_approved BOOLEAN NOT NULL DEFAULT FALSE;
-- role: client | specialist | manager | admin
-- is_approved: для manager — аппрув админом; остальные по умолчанию true при создании

-- Продакшены (справочник, управляется админом)
CREATE TABLE productions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX productions_active_name_idx
    ON productions(LOWER(name)) WHERE is_active = TRUE;

-- Выбор продакшена в профиле специалиста
ALTER TABLE specialist_profiles
    ADD COLUMN production_id UUID NULL REFERENCES productions(id) ON DELETE SET NULL,
    ADD COLUMN is_freelance  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT specialist_profiles_freelance_xor_production
        CHECK (NOT (production_id IS NOT NULL AND is_freelance = TRUE));

-- ПАЙПЛАЙНЫ (шаблоны воронки, управляются админом через UI — НЕ сид в коде)
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
    duration_days INT NOT NULL DEFAULT 1,       -- примерное время выполнения
    visible_to_client     BOOLEAN NOT NULL DEFAULT TRUE,
    visible_to_specialist BOOLEAN NOT NULL DEFAULT TRUE,
    weight        INT NOT NULL DEFAULT 1,
    sort_order    INT NOT NULL,
    is_review     BOOLEAN NOT NULL DEFAULT FALSE, -- помечает шаг «оставить отзыв» (авто-skip через 7 дней)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX pipeline_steps_stage_idx ON pipeline_steps(stage_id);

-- ПРОЕКТЫ (инстанс пайплайна, снэпшот на момент старта)
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
    assigned_to_user_id UUID REFERENCES users(id),     -- менеджер (NULL = во входящем пуле)
    pipeline_id         UUID NOT NULL REFERENCES pipelines(id),
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
    UNIQUE (lead_recipient_id)
);
CREATE INDEX projects_client_idx     ON projects(client_user_id);
CREATE INDEX projects_assigned_idx   ON projects(assigned_to_user_id);
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
    owner        TEXT NOT NULL,
    status       step_status NOT NULL DEFAULT 'pending',
    duration_days INT NOT NULL,
    visible_to_client     BOOLEAN NOT NULL,
    visible_to_specialist BOOLEAN NOT NULL,
    weight       INT NOT NULL,
    sort_order   INT NOT NULL,
    is_review    BOOLEAN NOT NULL DEFAULT FALSE,
    eta_date     DATE,
    review_deadline TIMESTAMPTZ,                 -- для review-шага: когда авто-skip (7 дней)
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_steps_project_idx ON project_steps(project_id);
CREATE INDEX project_steps_status_idx  ON project_steps(status);
CREATE INDEX project_steps_review_idx  ON project_steps(review_deadline)
    WHERE is_review = TRUE AND status = 'waiting_client';

-- Лента активности (источник для activity feed)
CREATE TABLE project_step_events (
    id            BIGSERIAL PRIMARY KEY,
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    step_id       UUID REFERENCES project_steps(id) ON DELETE CASCADE,
    actor_user_id UUID REFERENCES users(id),
    actor_type    TEXT NOT NULL DEFAULT 'human',  -- human | system
    event_kind    TEXT NOT NULL,                  -- step_transition | stage_advance | comment | assigned | created
    from_status   step_status,
    to_status     step_status,
    comment       TEXT,
    payload       JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_step_events_project_idx
    ON project_step_events(project_id, created_at DESC);

-- КОММЕНТАРИИ (задел под богатые карточки; в MVP — простой ввод, поле под TipTap)
CREATE TABLE project_comments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    author_id   UUID NOT NULL REFERENCES users(id),
    body        TEXT NOT NULL,                    -- plain text сейчас; TipTap отдаёт HTML/JSON позже
    body_format TEXT NOT NULL DEFAULT 'plain',    -- 'plain' | 'html' | 'tiptap_json' — задел
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ                       -- soft delete
);
CREATE INDEX project_comments_project_idx
    ON project_comments(project_id, created_at) WHERE deleted_at IS NULL;

-- Magic-link инвайты для клиентов, заведённых вручную
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

-- Привязка отзыва к проекту
ALTER TABLE reviews ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id);

-- НЕ сидим пайплайн. Админ создаёт первый пайплайн через UI.

-- +goose Down
-- обратные DROP'ы в правильном порядке
```

---

## 3. Редактор пайплайнов (новое — backend + admin UI)

Раньше воронка была захардкожена. Теперь **админ конструирует её через UI**.

### 3.1 Backend — домен `internal/pipelines/`

```
internal/pipelines/
├── dto.go      — Pipeline, Stage, Step, с вложенностью для редактора
├── repo.go     — CRUD, загрузка полного пайплайна с stages+steps
├── service.go  — валидация (имя, duration > 0, owner из enum, sort_order)
└── handlers_admin.go
```

API (только admin):
```
GET    /api/v1/admin/pipelines
GET    /api/v1/admin/pipelines/{id}
POST   /api/v1/admin/pipelines
PATCH  /api/v1/admin/pipelines/{id}
DELETE /api/v1/admin/pipelines/{id}
POST   /api/v1/admin/pipelines/{id}/stages
PATCH  /api/v1/admin/pipelines/stages/{id}
DELETE /api/v1/admin/pipelines/stages/{id}
POST   /api/v1/admin/pipelines/stages/{id}/steps
PATCH  /api/v1/admin/pipelines/steps/{id}
DELETE /api/v1/admin/pipelines/steps/{id}
PUT    /api/v1/admin/pipelines/{id}/reorder
```

**Версионирование:** редактирование пайплайна **не трогает** активные проекты (у них
снэпшот). Влияет только на новые проекты, стартующие после изменения. При значимом
изменении можно бампать `version`; на MVP достаточно того, что снэпшот защищает
активные проекты.

### 3.2 Admin UI — редактор пайплайнов

Страница `/admin/pipelines` — список пайплайнов (карточки) + кнопка «Создать».

Страница `/admin/pipelines/:id`:
- Вертикальный список **стадий**, внутри каждой — список **шагов**.
- На каждом шаге: имя, owner (селект client/team/system), время (duration_days),
  флажки видимости, чекбокс «это шаг отзыва» (`is_review`).
- Drag-drop для **переупорядочивания** стадий и шагов (CDK drag-drop).
- Кнопки «+ стадия», «+ шаг», удаление.

---

## 4. Проекты, стейт-машина, продвижение стадий

### 4.1 Снэпшот пайплайна при старте

`StartProject` копирует `pipeline_stages`/`pipeline_steps` в `project_stages`/
`project_steps`. Считает `eta_date` от `started_at` + накопленные `duration_days`.
Для `is_review`-шага НЕ ставит deadline сразу (ставится когда шаг становится
`waiting_client`).

### 4.2 Стейт-машина шагов

```
pending → in_progress → done
                     → waiting_client → done
                                     → rejected → in_progress
                     → skipped
```

`waiting_client` — отдельный статус. `skipped` — для review по тайм-ауту.

### 4.3 Продвижение стадии (для канбана менеджера)

Когда менеджер тащит проект-карточку в следующую колонку-стадию:
`POST /api/v1/manager/projects/{id}/advance_stage`
- Завершает (`done`) все незавершённые шаги **с owner='team'** текущей стадии.
- Активирует первый шаг следующей стадии.
- Если есть незавершённый шаг с `owner='client'` — **запрещает** продвижение (409
  «нельзя пропустить ожидание клиента»). Карточка отскакивает назад.
- Пишет событие `stage_advance` в `project_step_events` + outbox.

### 4.4 Цикл правок

`revisions_included` (из пайплайна), `revisions_used`. Клиент жмёт «Правки» на
client_review → rejected → возврат в монтажный шаг, `revisions_used += 1`. Лимит
исчерпан → `dispute` + нотификация менеджеру.

### 4.5 Review-шаг (7 дней)

Когда review-шаг становится `waiting_client` — ставим `review_deadline = now() + 7 days`.
Фоновая задача в worker (каждый час) переводит просроченные review-шаги в `skipped`,
проект → `done`.

### 4.6 display_status (для всех кабинетов)

Computed-поля в `internal/projects/display_status.go`:
- `DeriveProjectDisplayStatus` → not_started | in_progress | waiting_action |
  completed | on_hold | cancelled
- `DeriveStageDisplayStatus` → not_started | active | completed (+ done/total)
- `DeriveCurrentStep` → текущий шаг (приоритет: waiting_client(client) → in_progress
  → первый pending)

Таксономия (для UI бейджей):
| display_status | Бейдж | Цвет |
|---|---|---|
| not_started | «Впереди» | серый |
| in_progress | «В работе» | синий |
| waiting_action | «Ждёт вас» | gold |
| completed | «Готово» | green |
| on_hold | «На паузе» | orange |

---

## 5. API по кабинетам

### 5.1 Клиент (role=client)
```
GET  /api/v1/me/projects
GET  /api/v1/me/projects/{id}/funnel
POST /api/v1/me/projects/{id}/steps/{step_id}/approve
POST /api/v1/me/projects/{id}/steps/{step_id}/request_revision  {comment}
POST /api/v1/me/projects/{id}/steps/{step_id}/submit_review     {rating, text}
GET  /api/v1/me/projects/{id}/comments
```

### 5.2 Специалист (role=specialist)
```
GET   /api/v1/productions
PATCH /api/v1/me/profile                        — production_id | is_freelance
GET   /api/v1/me/specialist/projects
GET   /api/v1/me/specialist/projects/{id}/funnel
```

### 5.3 Менеджер (role=manager, is_approved=true)
```
GET   /api/v1/manager/projects/inbox
POST  /api/v1/manager/projects/{id}/claim
GET   /api/v1/manager/projects
GET   /api/v1/manager/projects/{id}
POST  /api/v1/manager/projects/{id}/advance_stage
POST  /api/v1/manager/projects/{id}/steps/{step_id}/start
POST  /api/v1/manager/projects/{id}/steps/{step_id}/complete
POST  /api/v1/manager/projects/{id}/steps/{step_id}/skip   {comment}
PATCH /api/v1/manager/projects/{id}
GET   /api/v1/manager/projects/{id}/events
GET   /api/v1/manager/projects/{id}/comments
POST  /api/v1/manager/projects/{id}/comments    {body}
```

### 5.4 Админ (role=admin)
```
# Продакшены
GET/POST/PATCH/DELETE /api/v1/admin/productions
# Аппрув менеджеров
GET  /api/v1/admin/managers
POST /api/v1/admin/managers/{id}/approve
POST /api/v1/admin/managers/{id}/revoke
# Пайплайны (раздел 3)
# Проекты (обзор всех)
GET  /api/v1/admin/projects
POST /api/v1/admin/projects
POST /api/v1/admin/projects/from_recipient/{id}
POST /api/v1/admin/projects/from_lead/{id}/bulk
POST /api/v1/admin/users
POST /api/v1/admin/users/{id}/generate_invite
```

### 5.5 Публичный
```
POST /api/v1/auth/redeem_invite/{token}
```

### Middleware
`RequireClient`, `RequireSpecialist`, `RequireManager` (role=manager AND is_approved),
`RequireAdmin`. Все на своих префиксах.

---

## 6. Frontend — три кабинета (Angular FSD)

### 6.1 Клиентский кабинет — раздел «Мои проекты» на сайте МП

**Размещение:** это НЕ отдельное приложение и НЕ отдельный layout. Это раздел
**«Мои проекты»** внутри основного сайта маркетплейса (`marketplace-web`) — там же,
где клиент ищет специалистов и создаёт брифы. Клиент — обычный пользователь МП, и
свои проекты смотрит в том же месте, без переключения в «другой кабинет».

- Пункт **«Мои проекты»** в основной навигации сайта (`app-header`), виден залогиненному
  пользователю с `role=client`.
- Маршрут `/me/projects` и `/me/projects/:id` в основном `app.routes.ts`.
- Использует тот же глобальный layout/шапку, что и остальной сайт МП.

Эталон UI детальной страницы — присланный скриншот. Реализовать:
- `pages/me/projects-list/` — карточки проектов, статус-бейдж из display_status,
  прогресс, ETA.
- `pages/me/project-detail/` — общий прогресс + ожидаемое завершение, стадии
  (таймлайн с кружками-статусами слева), внутри шаги с бейджами (Готово/В работе/
  Ждёт вас/Впереди) и подписью owner (вы/команда). Текущая стадия выделена синей
  рамкой. Polling 30с.
- Действия на waiting_client шагах: Принять / Запросить правки / Оставить отзыв.
- Легенда статусов сверху.

### 6.2 Специалистский кабинет

- Вкладка «Профиль» (существующее).
- Вкладка «Продакшен»: выбор production_id XOR is_freelance + назначенные проекты
  read-only (polling 60с).

### 6.3 Менеджерский кабинет

Layout `/manager/*` с sidebar.

- `pages/manager/inbox/` — входящие проекты без ответственного. Кнопка «Взять на себя».
- `pages/manager/board/` — **КАНБАН его проектов**:
  - Колонки = стадии пайплайна.
  - CDK drag-drop: тащишь проект → `advance_stage`.
  - При 409 — карточка отскакивает + тост.
- `pages/manager/project-detail/` — все шаги, кнопки Старт/Завершить/Пропустить,
  лента активности, комментарии, inline-edit полей (optimistic lock).

### 6.4 Админский кабинет

Layout `/admin/*` с sidebar.

- `pages/admin/productions/` — CRUD.
- `pages/admin/managers/` — список + аппрув/отозвать.
- `pages/admin/pipelines/` — список + редактор (CDK reorder).
- `pages/admin/projects/` — обзор всех.

### 6.5 Guards, роутинг и разграничение

**Два контура UI в одном `marketplace-web`:**

1. **Основной сайт МП** (общий layout/шапка): поиск, брифы, профили, плюс разделы
   по роли:
   - `client` → пункт «Мои проекты» (`/me/projects`)
   - `specialist` → кабинет специалиста (`/me`, вкладки Профиль/Продакшен)
2. **Операционные кабинеты** (отдельные layout с sidebar, не общая шапка сайта):
   - `manager` → `/manager/*`
   - `admin` → `/admin/*`

`shared/guards/`: `client.guard`, `specialist.guard`, `manager.guard` (role +
is_approved), `admin.guard`.

---

## 7. CDK канбан + комментарии + активность

### 7.1 Канбан на @angular/cdk/drag-drop

`cdkDropList` + `cdkDrag`. Оптимистичное перемещение, откат при 409.
**Тот же CDK используется в редакторе пайплайнов** для переупорядочивания стадий/шагов.

### 7.2 Комментарии — задел под TipTap

MVP: простой `textarea` + список. Структура готова к TipTap:
- `entities/comment/` — типы, repository (GET/POST).
- `widgets/comment-list/` — рендер по `body_format` (`plain` → текст; `html` /
  `tiptap_json` → задел).
- `features/add-comment/` — сейчас textarea + кнопка. **TODO: заменить на ngx-tiptap**.

Не подключаем TipTap сейчас — только чистая точка расширения.

### 7.3 Лента активности из project_step_events

- `entities/project-event/` — типы, repository.
- `widgets/activity-feed/` — вертикальный таймлайн, иконки по `event_kind`,
  relative-время через date-fns.

---

## 8. n8n (нотификации)

n8n self-hosted, `automation.{DOMAIN}`, basic-auth.

Worker → webhook: для событий из outbox шлёт webhook в n8n с `event_id`
(идемпотентность).

| Событие | Кому |
|---|---|
| шаг → waiting_client (owner=client) | клиенту: «требуется действие» |
| project.created (есть email) | клиенту + менеджеру |
| project.disputed | менеджеру |
| review_deadline approaching | клиенту: «остался день» |
| client_invite.generated | клиенту: magic-link |

Workflows: `project-client-notification` (email), `client-invite` (magic-link email),
`manager-notification` (Telegram). Экспорт в `ops/n8n/workflows/`.

---

## 9. Фазы

После каждой — `make lint && make test` (backend),
`npm run format:check && ng build` (frontend), ревью.

### Backend
- **Ф1** — миграция + домены productions, pipelines (2 дня)
- **Ф2** — профиль специалиста (выбор продакшена) (0.5 дня)
- **Ф3** — проекты: создание, снэпшот, клиентский API (2 дня)
- **Ф4** — менеджер: inbox, claim, канбан-API, ведение (2 дня)
- **Ф5** — стейт-машина review + worker-таска (0.5 дня)
- **Ф6** — админ: аппрув менеджеров, проекты, инвайты (1 день)
- **Ф7** — комментарии + активность API (0.5 дня)
- **Ф8** — outbox + n8n (1 день)

### Frontend
- **Ф9** — клиентский кабинет (2.5 дня)
- **Ф10** — специалистский кабинет (1 день)
- **Ф11** — менеджерский кабинет (3 дня)
- **Ф12** — админский кабинет (3 дня)
- **Ф13** — n8n + финализация (1 день)

---

## 10. Definition of Done (ключевое)

### Backend
- [ ] Миграции up/down чисто.
- [ ] Админ создаёт пайплайн через API, проект инстансит его снэпшотом.
- [ ] Редактирование пайплайна не ломает активные проекты.
- [ ] Менеджер видит inbox без ответственного, claim работает.
- [ ] advance_stage запрещает пропуск клиентского апрува (409).
- [ ] review-шаг авто-skip через 7 дней.
- [ ] display_status корректен (проект 77% = «В работе», не «Впереди»).
- [ ] Аппрув менеджера: до аппрува нет доступа в кабинет.
- [ ] Optimistic lock на PATCH проектов.
- [ ] Комментарии и события пишутся/читаются.
- [ ] outbox + n8n: нотификация при waiting_client.
- [ ] `make lint && make test` зелёные. Swagger.

### Frontend
- [ ] Клиентский кабинет (статусы, прогресс, цвета).
- [ ] Специалист выбирает продакшен (dropdown + Фрилансер).
- [ ] Менеджер: inbox → claim → канбан, drag продвигает стадию, невалидный
      drag отскакивает.
- [ ] Менеджер: лента активности + комментарии (textarea) в карточке.
- [ ] Админ: редактор пайплайнов (стадии/шаги/время, CDK reorder).
- [ ] Админ: аппрув менеджеров, CRUD продакшенов.
- [ ] Guards по ролям, manager-guard учитывает is_approved.
- [ ] Standalone, signals, inject, OnPush, @if/@for, только ng-zorro.
- [ ] Комментарии: точка расширения под TipTap помечена TODO.

### Приёмочные сценарии
- [ ] Админ создаёт пайплайн «бриф→оплата→...→отзыв» с временем шагов.
- [ ] Менеджер регистрируется → админ аппрувит → менеджер заходит.
- [ ] Проект появляется в inbox → менеджер claim → ведёт в канбане → двигает по
      стадиям → клиент видит прогресс в своём кабинете.
- [ ] Клиент апрувит сценарий → стадия продвигается.
- [ ] Проект завершён → review-шаг → клиент оставляет отзыв (или через 7 дней
      авто-skip).

---

## 11. Точки риска (проверить до старта)

См. отчёт по рекогносцировке в первом коммите ветки.
