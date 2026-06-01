# Directus админка платформы

Directus — внутренний инструмент для управления справочниками платформы
(`productions` и будущие CRM-сущности). Поднимается рядом с API в одном
compose-стеке, метаданные хранит в той же Postgres (таблицы `directus_*`),
бэкап БД покрывает и их.

## Доступ

- **Локально:** `http://localhost:8055`, bootstrap-учётка
  `admin@example.com / Admin12345` (значения зашиты в `docker-compose.yml`,
  не для прода).
- **Прод:** `https://admin.{DOMAIN}` (нужен caddy-блок и DNS-запись — см.
  «Развёртывание на прод» ниже).

## Роли и доступ к `productions`

| Действие | platform_admin | platform_moderator | unauthenticated |
| --- | --- | --- | --- |
| `GET /productions` (публичный API, только активные) | да | да | **да** |
| `GET /admin/productions` (наш API) | да | да | нет |
| `POST/PATCH/DELETE /admin/productions` (наш API) | да | **нет** | нет |
| Read в Directus UI | да | да | n/a |
| Create/Update/Delete в Directus UI | да | **нет** | n/a |

**Почему writes только у admin.** Productions — справочник на 5–20 записей,
изменения редкие. Concurrent-edit двух модераторов превратился бы в
last-write-wins (Directus 11 не делает If-Match / optimistic-lock для items
через REST). Чтобы исключить тихую потерю правок, writes ограничены ролью
`admin`. Моderator имеет read-доступ для observability/аудита и наблюдения
за справочником, но не правит.

> Это решение пересмотрим, когда будем добавлять CRM-сущности (лиды,
> сделки), где обновлений много и одного писателя мало. Для них правки
> «горячих» полей пойдут через наш Go API из кастомного UI в
> `marketplace-web` (со своими 409 stale_updated_at), а Directus останется
> для read-heavy панелей и dictionary-collections.

## Первый запуск локально

```bash
make up                                  # поднимет postgres/redis/opensearch
docker compose up -d directus            # поднимет Directus
# подождать ~10 сек, проверить:
curl -fsS http://localhost:8055/server/health
```

Зайти в UI: `http://localhost:8055`, креды выше.

При первом запуске Directus:
- Создал свои таблицы `directus_*` в БД `marketpclce`.
- Автоматически увидел наши таблицы как коллекции — `productions`,
  `users`, `specialist_profiles` и др. Импорт делать не нужно, всё уже
  доступно в Content.

## Настройка ролей и пользователей (первый раз)

Делается один раз после первого запуска. Можно через UI (Settings →
Access Control → Roles/Policies) или через REST API. Скрипт ниже
воспроизводимый — используем при инициализации стенда.

```bash
# 1. Логин bootstrap-админом
DT=$(curl -sS -X POST http://localhost:8055/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@example.com","password":"Admin12345"}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['access_token'])")
H="Authorization: Bearer $DT"

# 2. Роли
ADMIN_ROLE=$(curl -sS -X POST http://localhost:8055/roles -H "$H" \
  -H "Content-Type: application/json" \
  -d '{"name":"platform_admin","description":"Full CRUD на справочники"}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['id'])")
MOD_ROLE=$(curl -sS -X POST http://localhost:8055/roles -H "$H" \
  -H "Content-Type: application/json" \
  -d '{"name":"platform_moderator","description":"Read-only на productions"}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['id'])")

# 3. Policies + permissions
ADMIN_POLICY=$(curl -sS -X POST http://localhost:8055/policies -H "$H" \
  -H "Content-Type: application/json" \
  -d '{"name":"platform_admin_policy","app_access":true,"admin_access":false}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['id'])")
for ACTION in create read update delete; do
  curl -sS -X POST http://localhost:8055/permissions -H "$H" \
    -H "Content-Type: application/json" \
    -d "{\"policy\":\"$ADMIN_POLICY\",\"collection\":\"productions\",\"action\":\"$ACTION\",\"fields\":[\"*\"]}"
done

MOD_POLICY=$(curl -sS -X POST http://localhost:8055/policies -H "$H" \
  -H "Content-Type: application/json" \
  -d '{"name":"platform_moderator_policy","app_access":true,"admin_access":false}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['id'])")
curl -sS -X POST http://localhost:8055/permissions -H "$H" \
  -H "Content-Type: application/json" \
  -d "{\"policy\":\"$MOD_POLICY\",\"collection\":\"productions\",\"action\":\"read\",\"fields\":[\"*\"]}"

# 4. Связь role ↔ policy через directus_access
curl -sS -X POST http://localhost:8055/access -H "$H" \
  -H "Content-Type: application/json" \
  -d "{\"role\":\"$ADMIN_ROLE\",\"policy\":\"$ADMIN_POLICY\"}"
curl -sS -X POST http://localhost:8055/access -H "$H" \
  -H "Content-Type: application/json" \
  -d "{\"role\":\"$MOD_ROLE\",\"policy\":\"$MOD_POLICY\"}"

# 5. Пользователи
curl -sS -X POST http://localhost:8055/users -H "$H" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"platform-admin@example.com\",\"password\":\"PlatAdmin12345\",\"role\":\"$ADMIN_ROLE\",\"first_name\":\"Platform\",\"last_name\":\"Admin\"}"
curl -sS -X POST http://localhost:8055/users -H "$H" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"mod-alice@example.com\",\"password\":\"ModAlice12345\",\"role\":\"$MOD_ROLE\",\"first_name\":\"Alice\",\"last_name\":\"Mod\"}"
curl -sS -X POST http://localhost:8055/users -H "$H" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"mod-bob@example.com\",\"password\":\"ModBob12345\",\"role\":\"$MOD_ROLE\",\"first_name\":\"Bob\",\"last_name\":\"Mod\"}"
```

После настройки **выключить bootstrap-учётку** `admin@example.com`
(в проде — обязательно): Settings → Users → admin@example.com → Status
→ `suspended`.

## Управление продакшенами

В Directus UI: Content → Productions → `+` (создать) / клик по строке (правка).

Поля:
- `name` (string, NOT NULL, 2–120 символов, case-insensitive уникален среди
  активных).
- `description` (text, до 1000 символов).
- `is_active` (boolean, default true). Снять галку = soft-delete: запись
  останется в БД, привязанные специалисты не сломаются, в публичном
  списке `GET /productions` исчезнет.
- `id` / `created_at` / `updated_at` — автоматически, не трогать.

**Не удалять записи через «×»** — только снимать `is_active`. Удаление
безопасно (FK на `specialist_profiles.production_id` стоит
`ON DELETE SET NULL`), но теряется история.

## Service-token для Directus Flows → Go API

Когда Directus Flows понадобится дёргать наш Go API (например, для
CRM-операций), он должен ходить под service-токеном с `is_service=true`
и `role=admin` или `role=moderator`. Выпуск:

```bash
# с доступом к .env (JWT_SECRET) и БД (нужен service-account user
# directus-service@platform.internal, его кладёт миграция 00010):
go run ./cmd/service-token > /tmp/dst.txt   # дефолт role=admin
# Прод-вариант:
docker compose -f docker-compose.prod.yml exec api /usr/local/bin/api  # ...
```

Положить в `DIRECTUS_SERVICE_TOKEN` в `.env` (или `.env.prod`),
перезапустить контейнер directus. Внутри Flow токен используется в
`Request URL` хедере как `Authorization: Bearer ${DIRECTUS_SERVICE_TOKEN}`
(Directus 11 умеет подставлять env-переменные в Flow operations).

Ротация — выпустить новый, заменить env, перезапустить. Старый токен
автоматически инвалидироваться не будет (нет revocation list), но
без подписи на актуальном JWT_SECRET он работать перестанет, если
поменять и JWT_SECRET.

## Развёртывание на прод

1. В кабинете DNS → A-запись `admin.{DOMAIN}` → IP VDS.
   Если планируешь поднимать n8n (CRM-нотификации) — добавь ещё
   `automation.{DOMAIN}` → тот же IP.
2. В `marketplace-web/Caddyfile` добавить блоки:

   ```caddy
   admin.{$DOMAIN} {
       encode zstd gzip
       reverse_proxy directus:8055 {
           lb_try_duration 15s
           lb_try_interval 250ms
           fail_duration   1s
       }
   }

   # n8n (опционально, если поднят CRM-стек)
   automation.{$DOMAIN} {
       encode zstd gzip
       reverse_proxy n8n:5678 {
           lb_try_duration 15s
           lb_try_interval 250ms
           fail_duration   1s
       }
   }
   ```

   (Caddy сам выпишет TLS через Let's Encrypt при первом запросе.)

3. В `.env.prod`:

   ```
   DIRECTUS_KEY=$(openssl rand -hex 32)
   DIRECTUS_SECRET=$(openssl rand -hex 32)
   DIRECTUS_ADMIN_EMAIL=...        # сильный email
   DIRECTUS_ADMIN_PASSWORD=...     # сильный пароль
   DIRECTUS_SERVICE_TOKEN=         # заполнить ПОСЛЕ первого запуска
   ```

4. `make deploy` → `docker compose -f docker-compose.prod.yml up -d directus`.
5. Подождать прогрев (10–30 сек), зайти на `https://admin.{DOMAIN}`.
6. Прогнать скрипт ролей выше (адаптировав URL).
7. Сгенерировать service-token, прописать в `.env.prod`, перезапустить
   directus.
8. Выключить bootstrap-учётку `DIRECTUS_ADMIN_EMAIL` (Settings → Users
   → Status → `suspended`).
9. Завести 3–5 реальных продакшенов вручную через UI или повторить
   API-команды.

## Воронки (Funnel templates)

В Directus справа доступны три коллекции (иконки `movie_filter`/`layers`/
`check_circle_outline`):

- **service_templates** — сами шаблоны воронок (заголовок воронки,
  версия, флаг активности).
- **service_template_stages** — стадии внутри одной воронки.
- **service_template_steps** — шаги внутри стадии.

Модель: шаблон-чертёж и проект-снапшот. При создании проекта (через
Directus Flow или `POST /admin/projects`) текущее дерево воронки
копируется в `project_stages` + `project_steps`. **Правки шаблона на
запущенные проекты не влияют.**

### Добавить новую воронку

1. Content → **service_templates** → `+`.
2. Заполнить:
   - **code**: snake_case, например `corporate_video`. Должен быть
     уникален в паре с version.
   - **version**: 1 (для первой версии). Если правишь существующую
     воронку с уже запущенными проектами — лучше создать новую запись
     с `version=2`, не править v1, чтобы был чистый аудит.
   - **title**: «Корпоративное видео». Это видно в дропдауне выбора
     при создании проекта.
   - **is_active**: ✓ (если выключить — воронка исчезнет из дропдауна,
     запущенные проекты не задеты).
   - **revisions_included**: 2 по умолчанию (сколько раундов правок
     включено в стоимость).
3. Сохранить.
4. Открыть запись → справа в related-полях добавлять стадии в
   **service_template_stages** (template_id уже подставлен):
   - **code**: snake_case, уникален в рамках воронки. Пример: `prep`.
   - **title**: «Подготовка».
   - **sort_order**: 1, 2, 3…
5. В каждой стадии добавлять шаги в **service_template_steps**:
   - **code**: уникален в рамках стадии. Если код `client_review` —
     бэк включит approve-кнопку клиенту; `final_cut` — будет
     переоткрыт при revision-цикле. См. `internal/projects/client_actions.go`.
   - **title**: «Согласование сценария».
   - **owner**: `client` (требует действия клиента) / `team`
     (команда исполнителя) / `system` (автоматический шаг —
     escrow_hold, revision_round).
   - **duration_days**: сколько дней по плану. eta_date проставится
     автоматически при переходе шага в in_progress (today + duration).
   - **visible_to_client** / **visible_to_specialist**: скрывает шаг
     от ЛК соответствующей роли. Примеры скрытий из шаблона
     video_production: `social_setup` и `internal_approval` скрыты
     от клиента; `payment`, `nps`, `review` — от специалиста.
   - **weight**: вес в расчёте прогресса. Большие шаги (черновой
     монтаж, 7 дней) делают тяжелее (weight=7).
   - **sort_order**: порядок внутри стадии.
6. Проверка: `curl -H "Authorization: Bearer $SVC" /api/v1/admin/funnel-templates`
   вернёт новую воронку в списке. Дропдаун в Directus Flow «Create
   manual project» подхватит её автоматически.

### Что НЕ делать с воронками

- **Не редактировать живой шаблон при наличии активных проектов**, если
  изменения логические (другой owner у шага, удаление шага). Snapshot
  в проектах уже скопирован, но менеджер увидит расхождение между
  шаблоном и снапшотом, аудит будет хромать. Лучше — version+1.
- **Не удалять шаги вручную** на которых уже сидят активные проекты:
  FK CASCADE снесёт пометки в snapshot. Сначала переведи active
  проекты в done/cancelled, потом удаляй.
- **Не давать unauthenticated** (`Public`) доступ ни к одной из трёх
  коллекций — это внутренний справочник.

## Бэкап

- БД (`pg_dump`) покрывает и Directus-метаданные (`directus_*`).
- Volume `directus-uploads` — отдельно (внутри Directus при текущей
  конфигурации не используем, но если появятся аплоады, добавить
  в `scripts/backup.sh`).

## Что НЕ делать

- Не давать модератору write-доступ на `productions` через Directus
  UI — это нарушает single-editor дизайн и вернёт lost-update.
- Не выпускать static-токены Directus (Settings → Access Tokens) для
  внутренних автоматизаций — там нет `is_service`, наш Go API их
  отвергнет. Использовать `cmd/service-token`.
- Не редактировать `directus_*` таблицы напрямую в Postgres — Directus
  держит invariants на уровне приложения.
- Не давать unauthenticated-роли (`Public`) никаких прав на `productions`
  через Directus — публичный доступ идёт через наш Go API
  `/api/v1/productions` с урезанными полями (id + name только).
