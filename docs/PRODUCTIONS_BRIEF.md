# Задача: добавить сущность «Продакшен» с управлением через Directus

Это бриф для тебя, Claude Code. Прочти **полностью** перед началом.
Главный принцип: **не пересоздавай существующее, копируй паттерны
домена `catalog` (specialty_categories)** — это самый близкий аналог.

Это **последняя** задача перед основным CRM-брифом. После прогона у нас
будет:
- Сущность `productions`, управляемая админом через Directus.
- Поле выбора продакшена в профиле специалиста (с опцией «Фрилансер»).
- Поднятый и настроенный Directus, готовый к расширению коллекциями
  для CRM.

Объём: 2.5–3 дня.

## 0. Что прочитать перед стартом

### В `marketplace-api`:
1. `CLAUDE.md` и `README.md`.
2. `internal/catalog/` целиком — **референсный домен** (это
   `specialty_categories`, ближайший аналог нашей `productions`).
3. `internal/profiles/dto.go`, `repo.go`, `service.go`, `handlers.go`.
4. `migrations/00001_init.sql` — определение `specialist_profiles`
   и `specialty_categories`.
5. Последняя миграция для стиля.
6. `docs/DEPLOY.md` — куда крутить Caddy и docker-compose.

### В `marketplace-web`:
1. `web/src/entities/category/` — эталон entity с автокомплитом /
   select из каталога.
2. `web/src/pages/cabinet/cabinet.page.ts` и `.html` — куда добавим
   поле. Поле `city` — твой эталон позиционирования.
3. `web/src/pages/specialist-profile/` — публичный профиль.

### Документация Directus
`docs.directus.io`, разделы: Quickstart Docker, Collections, Fields,
Relationships (Many-to-One), Roles & Permissions.

Если что-то непонятно — **спроси меня**, не угадывай.

---

## 1. Контекст

Сейчас в `specialist_profiles` есть простые текстовые поля типа
`city`, `contact_email`. Продакшен (студия, в которой работает
специалист) — другого класса: это **управляемая сущность**, которую
ведёт админ платформы. Специалист только **выбирает** свой продакшен
из утверждённого списка.

Зачем это нужно (контекст продукта):
- Группировка специалистов по продакшенам для будущей админки и
  отчётности.
- Возможность будущей привязки команд исполнителей к продакшенам.
- Различение трёх веток бизнес-модели (свой продакшен / фриланс /
  чужой продакшен) на уровне данных.
- Фильтр в поиске «специалист X из этого продакшена».

«Фрилансер» — **не продакшен**, а отдельное семантическое состояние.
В UI — пункт в том же выпадающем списке, в данных — отдельный флаг.

---

## 2. Что строим — backend

### 2.1 Модель данных

```sql
-- +goose Up
CREATE TABLE productions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Имя уникально среди активных (case-insensitive), чтобы не плодить
-- дубли «Studio Forge» и «studio forge». При деактивации можно
-- завести новую запись с тем же именем.
CREATE UNIQUE INDEX productions_active_name_idx
    ON productions(LOWER(name)) WHERE is_active = TRUE;

ALTER TABLE specialist_profiles
    ADD COLUMN production_id UUID NULL REFERENCES productions(id) ON DELETE SET NULL,
    ADD COLUMN is_freelance  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT specialist_profiles_freelance_xor_production
        CHECK (NOT (production_id IS NOT NULL AND is_freelance = TRUE));
CREATE INDEX specialist_profiles_production_idx
    ON specialist_profiles(production_id) WHERE production_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS specialist_profiles_production_idx;
ALTER TABLE specialist_profiles
    DROP CONSTRAINT IF EXISTS specialist_profiles_freelance_xor_production,
    DROP COLUMN IF EXISTS is_freelance,
    DROP COLUMN IF EXISTS production_id;
DROP INDEX IF EXISTS productions_active_name_idx;
DROP TABLE IF EXISTS productions;
```

`ON DELETE SET NULL` — если админ удалит продакшен (мы не должны это
делать, есть `is_active=false`, но на всякий случай), специалисты
не блокируются.

### 2.2 Новый домен `internal/productions/`

Полностью повтори стиль `internal/catalog/`. Файлы:

```
internal/productions/
├── dto.go         — Production, PublicProduction (без описания
│                    если решим), ListInput, CreateInput, UpdateInput
├── repo.go        — pgx запросы (ListActive, GetByID, Create,
│                    Update, Deactivate)
├── service.go     — валидация (name 2–120, description ≤ 1000,
│                    уникальность имени среди активных)
├── handlers.go    — публичный GET /productions
└── handlers_admin.go — admin CRUD (Phase out когда Directus возьмёт
                       управление, но оставляем для service-account /
                       fallback)
```

### 2.3 API эндпоинты

**Публичный** (для фронта специалиста — заполнить выпадающий список):
```
GET /api/v1/productions                — список активных, без auth
```

**Admin** (для service-account и потенциально других админ-инструментов):
```
GET    /api/v1/admin/productions       — все, включая неактивные
POST   /api/v1/admin/productions       — создать
PATCH  /api/v1/admin/productions/{id}  — изменить (optimistic lock)
DELETE /api/v1/admin/productions/{id}  — soft delete (is_active=false)
```

Middleware на `/admin/*` — `RequireAdmin`-или-`RequireModerator` (см. ниже).

**Optimistic lock на PATCH** — обязательно, т.к. модераторов будет
несколько и они могут править одну запись одновременно. Паттерн —
**ровно как в `internal/leads/` для recipient'а**:

- В ответе `GET` и `PATCH` всегда возвращается `updated_at` записи.
- Клиент (Directus / curl / любой консьюмер) присылает `updated_at`
  в теле PATCH запроса.
- Если присланный `updated_at` не совпадает с текущим в БД → возврат
  **409 Conflict** с понятным сообщением.
- На стороне Directus интерфейс по умолчанию это поддерживает — он
  посылает `If-Match` или эквивалент.

Без этого защита один-другому модератор затрёт чужие правки молча.

### 2.4 Расширение `internal/profiles/`

Добавляем поля `production_id` и `is_freelance` в:
- `dto.go` — структура профиля.
- `repo.go` — SELECT/UPDATE.
- `service.go` — валидация:
  - Если `production_id != nil`, проверь, что продакшен существует
    и активен (один SELECT в `productions`).
  - Если `is_freelance == true`, обнули `production_id`.
  - Если `production_id != nil`, обнули `is_freelance`. (Двойная
    защита поверх БД-constraint'а — даём понятный 400 вместо 500.)

В `MeProfile`-ответе добавь:
```go
ProductionID    *uuid.UUID `json:"production_id,omitempty"`
ProductionName  string     `json:"production_name,omitempty"`  // join из productions для удобства фронта
IsFreelance     bool       `json:"is_freelance"`
```

`ProductionName` — денормализация для фронта, чтобы не делать второй
запрос. В SELECT — `LEFT JOIN productions p ON p.id = sp.production_id`.

### 2.5 Service-account и роль admin

Подготовим почву под Directus. В миграции:

```sql
-- Добавить роль в users (если уже не сделано в предыдущих задачах)
ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'client';
-- допустимые: client | specialist | admin | moderator

-- Создать service-account пользователя для Directus
INSERT INTO users (email, password_hash, role, is_email_verified, created_at)
VALUES (
    'directus-service@platform.internal',
    '$2a$10$DUMMY_NEVER_LOGIN_DIRECTLY',  -- хэш заведомо несовместим, логин невозможен
    'admin',
    true,
    now()
) ON CONFLICT (email) DO NOTHING;
```

В `internal/auth/`:
- `RequireAdmin` middleware — JWT с `role='admin'` ИЛИ service-account токен.
- Service-account токен — long-lived JWT без exp, выдаётся через
  ENV-переменную `DIRECTUS_SERVICE_TOKEN` обоих сервисов. В коде
  валидируется по signature + claim `is_service=true`.

В аудит-логе (если есть) actor от service-account отмечаем меткой
`actor_type='service'`, чтобы потом отличать «человек-админ»
от «Directus автоматика».

### 2.6 Сидинг

В миграции (или отдельной seed-команде) — **не сидим** продакшены.
Админ заведёт их через Directus после первого запуска. Это даст
честную проверку флоу.

---

## 3. Что строим — frontend (`marketplace-web`)

### 3.1 entity productions

```
entities/production/
├── api/productions.repository.ts   — GET /productions
├── model/production.types.ts        — Production interface
└── index.ts
```

`ProductionsRepository.list()` возвращает `Observable<Production[]>`.
Кэш на 5 минут через `shareReplay({ refCount: true, windowTime: 300_000 })`
или Angular `inject(DestroyRef)` + signal-based кэш — как удобнее.

### 3.2 Расширение entity me

В `entities/me/` добавь в тип профиля:
```typescript
production_id: string | null;
production_name: string | null;
is_freelance: boolean;
```

### 3.3 Виджет выбора в форме профиля

В `cabinet.page.html` рядом с полем `city` добавь поле «Продакшен»
через `nz-select`:

```html
<nz-form-item>
  <nz-form-label>Продакшен</nz-form-label>
  <nz-form-control>
    <nz-select
      [ngModel]="selectedProductionValue()"
      (ngModelChange)="onProductionChange($event)"
      nzPlaceHolder="Выберите продакшен или фриланс"
      nzShowSearch
      nzAllowClear>
      <nz-option
        *ngFor="let p of productions()"
        [nzValue]="p.id"
        [nzLabel]="p.name">
      </nz-option>
      <nz-option nzValue="__freelance__" nzLabel="Фрилансер">
      </nz-option>
    </nz-select>
  </nz-form-control>
</nz-form-item>
```

(Если в кодстайле используется `@for` — переведи на него, ngFor
здесь для краткости.)

Логика `onProductionChange`:
- `null` → `production_id=null, is_freelance=false`
- `'__freelance__'` → `production_id=null, is_freelance=true`
- `<uuid>` → `production_id=<uuid>, is_freelance=false`

`selectedProductionValue()` — computed signal, преобразует обратно:
- `is_freelance=true` → `'__freelance__'`
- `production_id` → этот id
- иначе `null`

### 3.4 Публичный профиль

В `pages/specialist-profile/`:
- Найди где показывается `city`.
- Добавь блок «Продакшен» по правилам:
  - `is_freelance=true` → «Фрилансер»
  - `production_name` → название
  - оба пустые → ничего не показывать
- Формат шапки: `{display_name} · {production_label} · {city}`
  с разделителями только при наличии полей.

### 3.5 Карточка в ленте (если есть)

Если в `widgets/specialist-card/` (или похожем) показывается `city`
или специальность — добавь `production_label` по тем же правилам.

---

## 4. Directus — поднятие и настройка

### 4.1 Docker compose

В `docker-compose.prod.yml`:

```yaml
  directus:
    image: directus/directus:11
    container_name: directus
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_started
    environment:
      KEY: ${DIRECTUS_KEY}
      SECRET: ${DIRECTUS_SECRET}
      ADMIN_EMAIL: ${DIRECTUS_ADMIN_EMAIL}
      ADMIN_PASSWORD: ${DIRECTUS_ADMIN_PASSWORD}
      DB_CLIENT: pg
      DB_HOST: postgres
      DB_PORT: 5432
      DB_DATABASE: ${POSTGRES_DB}
      DB_USER: ${POSTGRES_USER}
      DB_PASSWORD: ${POSTGRES_PASSWORD}
      CACHE_ENABLED: 'true'
      CACHE_STORE: redis
      REDIS: redis://redis:6379
      PUBLIC_URL: https://admin.${DOMAIN}
      CORS_ENABLED: 'true'
      CORS_ORIGIN: https://admin.${DOMAIN}
    volumes:
      - directus-uploads:/directus/uploads
      - directus-extensions:/directus/extensions
    mem_limit: 512m

volumes:
  directus-uploads:
  directus-extensions:
```

Добавь в `.env.prod.example`:
```
DIRECTUS_KEY=
DIRECTUS_SECRET=
DIRECTUS_ADMIN_EMAIL=
DIRECTUS_ADMIN_PASSWORD=
DIRECTUS_SERVICE_TOKEN=
```

KEY и SECRET генерируются `openssl rand -hex 32`.

### 4.2 Caddyfile

Добавь блок:
```
admin.{$DOMAIN} {
    reverse_proxy directus:8055
}
```

DNS: A-запись `admin.{DOMAIN}` на тот же IP VDS. Caddy сам выпишет
TLS через Let's Encrypt.

### 4.3 Первый запуск и конфигурация

После `make deploy`:

1. Зайти на `admin.{DOMAIN}` под `DIRECTUS_ADMIN_EMAIL/PASSWORD`.
2. **Directus автоматически увидит таблицы** в БД и предложит
   импортировать их как коллекции. Импортировать:
   - `productions`
   - `specialist_profiles` (для отображения, не редактирования
     production_id/is_freelance — это делают сами специалисты)
   - `users` (read-only для админов в этой задаче)
3. Настроить **роли** (важно: модераторов будет несколько, права
   проектируем под это):
   - `platform_admin` — full CRUD на `productions`, управление
     пользователями, read-only на `specialist_profiles` и `users`.
     **1–2 человека на платформе.**
   - `platform_moderator` — read + update на `productions` (поля
     `name`, `description`, `is_active`), без create/delete.
     **Произвольное количество — 3, 5, 10 человек.**
     Не имеет доступа к пользователям и админ-настройкам Directus.
4. Создать первого админа через Settings → Users в Directus,
   привязать к роли `platform_admin`.
5. Создать **минимум 2 модераторов** (для проверки multi-user
   сценария и concurrent edits) с ролью `platform_moderator`.
6. Заполнить **3–5 тестовых продакшенов** через Directus UI,
   чтобы фронт мог их подгрузить.

### 4.4 Service-account токен в Directus

В Directus → Settings → Access Tokens создать постоянный токен
для роли `platform_admin`, скопировать значение в `DIRECTUS_SERVICE_TOKEN`
в `.env.prod`. Этот токен Directus Flows будут использовать для
вызовов нашего Go API (потребуется в основном CRM-промпте).

### 4.5 Бэкап Directus-конфига

Directus метаданные хранятся в той же Postgres (таблицы `directus_*`) —
существующий бэкап БД их покрывает. Volume `directus-uploads`
добавить в `scripts/backup.sh` (если есть).

---

## 5. Что НЕ делать

- **Не сидим** продакшены автоматически. Админ создаёт их через Directus.
- **Не делаем** автокомплит с возможностью «создать на лету».
  Только выбор из утверждённого списка. Это сознательно — иначе
  все начнут писать «Фриланс», «фриланс», «freelancer» руками.
- **Не показываем** в выпадающем списке неактивные продакшены.
- **Не удаляем** продакшен из БД через DELETE. Только soft (is_active=false).
- **Не даём** Directus прямой write-доступ к `specialist_profiles.production_id`.
  Эта связь устанавливается специалистом через наш API, не админом.
  (Админ ведёт **справочник** продакшенов, не привязывает к ним людей.)
- **Не плодим** новые UI-киты. Только ng-zorro.
- **Не используем** NgModule, ngIf/ngFor (только @if/@for),
  constructor injection.

---

## 6. Фазы

После каждой — `make lint && make test` (backend),
`npm run format:check && ng build` (frontend), ручная проверка,
ревью.

### Фаза 1 — Backend: миграция и домен productions (1 день)
1. Миграция (productions + расширение specialist_profiles + role
   в users + service-account user).
2. `internal/productions/` (dto, repo, service, handlers,
   handlers_admin).
3. Юнит-тесты на валидацию и repo.
4. Подключить в `cmd/api/main.go` и роутер.
5. `make migrate-up && make test && make lint` зелёные.
6. Ручная проверка: `curl GET /productions` (пустой массив, ок),
   `curl POST /admin/productions` с service-token создаёт запись.

### Фаза 2 — Backend: расширение profiles (0.5 дня)
1. Поля в DTO, SELECT с JOIN, UPDATE с валидацией.
2. Service: проверка existence продакшена, взаимоисключение с
   freelance.
3. Тест на CHECK-constraint (попытка `production_id + is_freelance`
   должна падать 400).
4. Swagger обновлён.

### Фаза 3 — Directus (1 день)
1. Добавить сервис в `docker-compose.prod.yml`.
2. Caddyfile с маршрутом.
3. `.env.prod.example` обновить.
4. Сделать DNS-запись для `admin.{DOMAIN}` (тебе сделать в DNS
   кабинете — напомни мне).
5. Деплой, первый запуск.
6. Настроить роли `platform_admin` и `platform_moderator`.
7. Создать второго пользователя для проверки multi-admin.
8. Завести 3–5 тестовых продакшенов.
9. Сделать постоянный access-token, сохранить в env.
10. Записать инструкцию для админов в `docs/ADMIN_DIRECTUS.md`
    (как заходить, как добавлять продакшены).

### Фаза 4 — Frontend (0.5–1 день)
1. `entities/production/` с Repository.
2. Расширение типов в `entities/me/`.
3. Поле `nz-select` в `cabinet.page` с правильной логикой.
4. Отображение на публичном профиле.
5. Карточка в ленте (если есть).
6. Ручная проверка end-to-end:
   - Зайти под специалистом → выбрать продакшен → сохранить →
     перезагрузить → видно.
   - Переключить на «Фрилансер» → сохранить → перезагрузить.
   - Открыть публичный профиль → видно лейбл.
   - Зайти в Directus → деактивировать продакшен → у специалистов
     production_id остался, но в выпадающем списке для других
     специалистов он не появляется.

---

## 7. Definition of Done

- [ ] Миграции up/down чисто.
- [ ] `GET /productions` возвращает только активные.
- [ ] Admin endpoints работают со service-token.
- [ ] `PATCH /me/profile` принимает `production_id` ИЛИ
      `is_freelance`, но не оба.
- [ ] CHECK-constraint в БД и валидация в Go на одной и той же логике.
- [ ] `make lint && make test` зелёные.
- [ ] Swagger описывает новые ручки.
- [ ] Directus поднят на `admin.{DOMAIN}` с HTTPS.
- [ ] Роли `platform_admin` и `platform_moderator` настроены
      с правильными permissions (admin = full CRUD; moderator =
      только read+update полей `name/description/is_active`).
- [ ] Минимум **1 admin + 2 moderator** созданы в Directus.
- [ ] **Concurrent-edit тест пройден:** два модератора открывают
      одну запись `productions`, оба правят, второй PATCH возвращает
      **409 Conflict** (а не молча затирает).
- [ ] Optimistic lock на PATCH `/admin/productions/{id}` через
      `updated_at` работает (юнит-тест + ручная проверка).
- [ ] 3–5 тестовых продакшенов созданы.
- [ ] Service-token сохранён в env, готов к использованию в CRM.
- [ ] Документация в `docs/ADMIN_DIRECTUS.md` написана.
- [ ] Frontend `format:check` и `build` зелёные.
- [ ] `nz-select` в кабинете работает, переключение между «студия»
      и «фрилансер» корректно сохраняется.
- [ ] Публичный профиль показывает лейбл с правильным форматом.
- [ ] Standalone, signals, inject, OnPush, @if/@for везде.

---

## 8. Точки риска

Проверь и подтверди мне до начала:

1. Как сейчас называется DTO профиля и есть ли уже поле `role`
   в `users`?
2. Стиль миграций — `+goose Up / Down` или просто SQL?
   Снапшоты сидинга через goose или отдельный seed-инструмент?
3. Есть ли в `internal/auth/` готовый паттерн service-account
   токенов, или придётся писать с нуля?
4. Хватает ли RAM на VDS (Directus просит ~300–500 MB). Если
   сейчас занято > 3 GB из 4 — апгрейд до 6 GB до старта Фазы 3.
5. Свободен ли поддомен `admin` (DNS / SSL не блокирует)?
6. Виджет карточки специалиста в ленте — есть, нет, какой?

---

## 9. Первый коммит

1. Ветка `feature/productions-entity-and-directus`.
2. Положить файл в `docs/PRODUCTIONS_BRIEF.md`.
3. Пустой коммит.
4. До Фазы 1 — напиши мне 5–7 строк ответа на точки риска
   и план Фазы 1.

Удачи. После прогона у нас будет рабочий Directus с двумя админами
и работающее поле «Продакшен» в профиле — идеальный плацдарм для
основного CRM-промпта (проекты клиента + воронка).
