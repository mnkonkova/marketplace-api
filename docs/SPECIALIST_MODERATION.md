# Модерация специалистов админом (design doc)

**Статус:** не реализовано, спека под MVP.
**Связанные:** [docs/CRM_V5_BRIEF.md](./CRM_V5_BRIEF.md) (общая роль
админа), `internal/search/repo.go:LoadFeedVideoDocs`,
`internal/profiles/service.go:SetPublished`.

**Цель:** на старте маркетплейса (MVP) спец может «опубликовать»
свой профиль, но в OpenSearch (каталог `/search` + `/feed`) он
**не** попадает до тех пор, пока админ его не подтвердит. Защита
от спама, фейков, нерелевантного контента в самой раннее время
когда мы не можем доверять ML и репутации.

После выхода из MVP (>1k активных спецов) — модерацию можно ослабить
до «авто-модерация + reactive ban» или вынести в LLM-classifier.

---

## 1. Сценарий

### Сейчас

```
[Спец] жмёт "Опубликовать" в /me/profile
  → API: SetPublished(is_published=TRUE)
  → outbox: specialist.upserted
  → worker: reconcile OpenSearch
  → спец появляется в /search и /feed
```

Никакого админа в цепочке. Любой может опубликовать что угодно.

### После

```
[Спец] жмёт "Опубликовать"
  → API: SetPublished(is_published=TRUE, moderation_status='pending_review')
  → в /search и /feed ещё НЕ виден
  → в /me/profile банер: "На проверке. Появится в каталоге после одобрения."

[Админ] открывает /admin/moderation
  → видит очередь pending'ов с превью карточки
  → клик "Одобрить":
      moderation_status='approved'
      outbox: specialist.upserted
      worker → OpenSearch (спец появляется в /search и /feed)
      спец видит "Профиль одобрен" в /me/profile
  → клик "Отклонить" с обязательной причиной:
      moderation_status='rejected', moderation_reason='...'
      спец видит "Профиль отклонён: <причина>. Исправьте и опубликуйте снова."
      в OpenSearch не уходит (или удаляется если был там)
```

---

## 2. Modeli и states

### Поле `moderation_status` в `specialist_profiles`

| Значение | Что значит | Видим в каталоге? |
|---|---|---|
| `pending_review` | Только что опубликован, ждёт админа | ❌ нет |
| `approved` | Админ одобрил | ✅ да (если ещё и `is_published=TRUE`) |
| `rejected` | Админ отклонил | ❌ нет, спец видит причину |

`is_published` остаётся отдельным полем (спец сам решает быть в каталоге
или нет — например на отпуск выключает). Эффективная видимость:

```sql
is_published = TRUE AND moderation_status = 'approved'
```

### Переходы

```
                  ┌────────────────────────────────────┐
                  ↓                                    │
[спец публикует] pending_review ─[админ одобряет]→ approved ──┐
                  │                                            │
                  │              ┌─[спец редактирует]──────────┘
                  │              ↓
                  └─[админ отклоняет]→ rejected
                                          │
                                          └─[спец публикует снова]→ pending_review
```

**Важно:** любое изменение профиля у `approved`-спеца возвращает
обратно в `pending_review`. То есть спец опубликовал, админ одобрил,
спец поменял bio → снова в очередь. Для MVP — самый безопасный
паттерн (изменения видит админ перед попаданием в каталог).

Какие изменения триггерят перевод обратно в pending:
- `display_name` / `bio` / `avatar_url` (главные текстовые/визуальные)
- `production_id` / `is_freelance` (юридическое позиционирование)
- `categories` (специализация)
- Добавление нового portfolio_item.video или обновление существующего
- `rate_min`/`rate_max`/`city` — debatable, но для MVP тоже = re-review

Какие не триггерят:
- `rating_avg`/`reviews_count` (это снаружи приходит, не спец меняет)
- Удаление portfolio_item (наоборот, спец сам себя «зачистил»)
- `is_published` toggle (out of catalog ↔ back to catalog), если статус
  остался approved

### Дополнительные поля

```sql
moderation_status        TEXT NOT NULL DEFAULT 'pending_review'
moderation_reason        TEXT          -- причина reject'a от админа
moderation_reviewed_at   TIMESTAMPTZ   -- когда был verdict
moderation_reviewed_by   UUID REFERENCES users(id)  -- кто из админов
```

`moderation_status` имеет CHECK constraint:
`status IN ('pending_review','approved','rejected')`.

### Backfill при миграции

Существующие спецы на проде уже в каталоге. Чтобы не оставить их за
бортом:

```sql
UPDATE specialist_profiles
SET moderation_status = 'approved',
    moderation_reviewed_at = now()
WHERE is_published = TRUE;
```

Спецы которые published=FALSE остаются в pending_review (когда
опубликуются — пойдут в очередь).

---

## 3. Backend

### Миграция (00021)

- Колонки выше + CHECK + partial index
  `WHERE moderation_status = 'pending_review'` для быстрого `ORDER BY
  updated_at` в админской очереди.

### Repo / Service

**`profiles.SetPublished`** (`internal/profiles/service.go:292`):
- При `published=true` сбрасывать `moderation_status='pending_review'`
  (если был approved — в очередь обратно).
- НЕ эмитить `specialist.upserted` если статус не approved
  (иначе worker положит в OS).

**`profiles.Patch*InTx`** (любая мутация полей которые «триггерят
re-review»):
- Если `moderation_status='approved'` И юзер дернул bio/avatar/…:
  `moderation_status='pending_review'`
- Эмитить `specialist.retracted` чтобы worker убрал из OS пока
  не одобрят заново.

**`search.LoadFeedVideoDocs`** / **`search.Indexer.Reconcile`**:
- В WHERE добавить `AND moderation_status = 'approved'`.

**`admin/repo.go`** (новые методы):
```go
ListPendingSpecialists(ctx, limit, offset) → []SpecialistProfile
ApproveSpecialist(ctx, userID, actorID) error
RejectSpecialist(ctx, userID, actorID, reason string) error
```

ApproveSpecialist:
- UPDATE moderation_status='approved', reviewed_at, reviewed_by
- outbox: `specialist.upserted` → worker индексит
- эмитить `moderation.approved` для аналитики (опц)

RejectSpecialist:
- UPDATE moderation_status='rejected', reason
- outbox: `specialist.retracted` (если был в OS)
- эмитить `moderation.rejected`

### API endpoints

```
GET    /admin/moderation/specialists?status=pending&limit=20&cursor=…
       → список в очереди
GET    /admin/moderation/specialists/:id
       → детали + ссылка на полный профиль
POST   /admin/moderation/specialists/:id/approve
       → 200, событие одобрения
POST   /admin/moderation/specialists/:id/reject
       body: { "reason": "string ≤500 chars" }
       → 200, причину сохраняем в moderation_reason
```

Защита: `RequireRoles("admin")`. Менеджеру модерация не доступна —
это политическое решение «каталог решает только админ».

### DTO

`profiles.Profile` (отдаваемое в `/me/profile`):
- Добавить поля `moderation_status: string`,
  `moderation_reason: string|null`,
  `moderation_reviewed_at: time.Time|null`.

`PublicProfile` (отдаваемое в `/specialists/:id`):
- Эти поля НЕ отдавать — публично знать что профиль был rejected
  никому не нужно.

### Outbox events

Без изменений в схеме outbox. Используем существующие:
- `specialist.upserted` — после approve (или edit approved-спеца, но
  без upsert до следующего approve)
- `specialist.retracted` — после reject

---

## 4. Frontend

### Спец-кабинет `/me/profile`

Добавить **banner** наверху страницы в зависимости от
`moderation_status`:

```
┌─────────────────────────────────────────────────────────────┐
│ ⏳ На проверке у админа                                      │
│                                                              │
│ Ваш профиль в очереди модерации. Обычно занимает до 24      │
│ часов. После одобрения вы появитесь в каталоге.             │
└─────────────────────────────────────────────────────────────┘
```

или

```
┌─────────────────────────────────────────────────────────────┐
│ ⚠ Профиль отклонён                                          │
│                                                              │
│ Причина: <moderation_reason>                                 │
│                                                              │
│ [Внести правки и опубликовать снова]                        │
└─────────────────────────────────────────────────────────────┘
```

`approved` — баннер не показываем, всё в порядке.

В `me.types.ts`:
```ts
export interface MyProfile {
  // ... уже существующие поля
  moderation_status: 'pending_review' | 'approved' | 'rejected';
  moderation_reason?: string;
  moderation_reviewed_at?: string;
}
```

### Админ-кабинет `/admin/moderation`

Новая страница (роут `/admin/moderation`):

#### Список очереди

```
┌─────────────────────────────────────────────────────────────┐
│ Модерация специалистов                                       │
│                                                              │
│ В очереди: 12                                                │
│                                                              │
│ ┌─────────┐ Иван Петров                       2ч назад      │
│ │ avatar  │ UGC-креатор · фриланс             [Открыть →]   │
│ │         │ Москва · 2000-5000 ₽/мин                        │
│ └─────────┘                                                  │
│                                                              │
│ ┌─────────┐ Анна Сидорова                    5ч назад       │
│ │ avatar  │ Видеограф · фриланс              [Открыть →]    │
│ ...                                                          │
└─────────────────────────────────────────────────────────────┘
```

Сортировка: FIFO (старые сверху — никого не «забыли»).
Пагинация: cursor-based как у `/admin/projects`.

#### Детальная страница `/admin/moderation/:id`

Embedded полный публичный preview профиля + кнопки внизу:

```
┌─────────────────────────────────────────────────────────────┐
│ [полный preview как на /specialist/<id>]                    │
│                                                              │
│ ───────────────────────────────────────────                  │
│ Решение                                                      │
│                                                              │
│ [✓ Одобрить и опубликовать]    [✗ Отклонить с причиной]    │
└─────────────────────────────────────────────────────────────┘
```

Reject — модалка с обязательной textarea (минимум 10 символов).

### Бейдж в сайдбаре админки

В навигации `/admin/*` рядом с пунктом «Модерация» показывать число
pending'ов:

```
Модерация [12]
```

Источник: тот же `GET /admin/moderation/specialists?status=pending`
с `count`-параметром (или отдельный лёгкий endpoint только COUNT'а
если потребуется).

---

## 5. Edge cases

### A. Спец публикуется → админ долго (>24ч) не реагирует

Юзер ждёт. Можно добавить email-нотификацию админу через outbox →
n8n → telegram-чат «moderation backlog» (отложим до пост-MVP).

Sanity-check: alert в Grafana `pending_specialists_too_old` если есть
записи >48 часов в очереди (см. metrics ниже).

### B. Спец одобрен, потом меняет profile

См. §2. Любое значимое изменение → обратно в pending. spec видит
banner «На повторной проверке». Из OS убирается (через
`specialist.retracted`).

### C. Несколько админов работают параллельно

Race: оба открыли одного спеца, оба нажали Approve. Решение:
optimistic lock через `updated_at` (existing паттерн в проектах).
Второй получает 409, видит уведомление «Уже обработан коллегой».

### D. Reject → спец исправил → новая итерация

Спец публикуется заново. История reject'ов **не хранится** в БД для
MVP (только последний). Если нужно — отдельная таблица
`moderation_history`, отложим.

### E. Admin сам себе создаёт фейковый профиль и одобряет

Не баг, а фича — для тестирования флоу. Альтернатива (защита от
«сам себя одобрил»): админу запретить approve собственный профиль.
Для MVP можно опустить.

### F. Backfill старых спецов

См. §2: `UPDATE … SET moderation_status='approved' WHERE
is_published=TRUE`. Если потом захотим пере-промодерировать —
отдельная админская команда (например в seed).

---

## 5a. n8n: уведомление админу в Telegram

При переходе спеца в `pending_review` backend эмитит outbox-событие
`moderation.specialist_pending` с payload `{user_id, email, display_name,
reason}`. Worker через `N8N_WEBHOOK_URL` (тот же CRM webhook, что у
`project.*`) шлёт его в n8n.

Версионированный workflow — `deploy/n8n/workflows/crmTgEventsV1.json`,
case добавлен в существующий Code-node `Format message`. Деплой на проде:

```bash
ssh root@<vds>
cd /opt/marketpclce/api
git pull
make n8n-import   # подтянет crmTgEventsV1.json в работающий n8n
```

В n8n UI workflow остаётся **Active** — пере-привязки credentials не
нужно, т.к. меняется только jsCode внутри ноды.

Формат сообщения в Telegram:
```
🔍 Новая заявка на модерацию
<display_name>
<email>
Причина: первая публикация / повторная попытка
[Открыть карточку] → {app_base_url}/admin/moderation/{user_id}
```

`reason` приходит из backend:
- `publish_requested` — спец впервые нажал «Опубликовать» или повторно
  после reject'a (→ «первая публикация / повторная попытка»);
- `content_changed` — был approved, поменял профиль (→ «изменения
  после одобрения»).

Если нужен отдельный чат (не туда, где CRM `project.*`) — заведи
второй workflow + Webhook и новый env `N8N_MODERATION_WEBHOOK_URL`
по аналогии с `N8N_SUPPORT_WEBHOOK_URL` (см. cmd/worker/main.go).

---

## 6. Метрики (Grafana)

Добавить в `internal/profiles/metrics.go` (новый):
- `moderation_pending_count` (gauge) — сколько в очереди
- `moderation_pending_oldest_minutes` (gauge) — возраст самой старой записи
- `moderation_approved_total{actor_id}` (counter)
- `moderation_rejected_total{actor_id}` (counter)
- `moderation_decision_latency_seconds` (histogram) — от submit до verdict

Алерты в `grafana/alerts.yml`:
- `ModerationBacklogHigh`: `moderation_pending_count > 50 for 2h` (warn)
- `ModerationStale`: `moderation_pending_oldest_minutes > 2880` (page) —
  что-то висит >48 часов

Дашборд: новая секция «Moderation» с этими метриками.

---

## 7. Out of scope (MVP)

- **LLM-предмодерация** — пайплайн отдаёт reason от модели, админ
  только подтверждает. Когда дойдём до 100+ заявок в день.
- **Шкала качества** (`good/ok/bad`) вместо binary approve/reject —
  для тонкого ранжирования. Сейчас бинарный.
- **Multi-reviewer** flow (нужно ≥2 одобрений) — оверкилл.
- **История модерации** (см. §5.D) — отдельная таблица.
- **Auto-rejection** по простым правилам (display_name матчит спам-list,
  bio < 50 символов) — добавим если будет много откровенно мусорных.

---

## 8. План реализации

Discrete spurts, можно катить отдельными PR'ами:

### Бэк

1. **Migration 00021** — поля + index + backfill `WHERE is_published`.
2. **`profiles.service`** — SetPublished пробивает moderation_status;
   Patch* возвращает approved в pending; репо-фильтры в search.
3. **Admin endpoints** + handlers + routing в `httpapi/router.go`.
4. **Тесты** на state-машину (pending→approved→re-publish→pending и т.д.).
5. **Метрики** + алерты в `grafana/alerts.yml`.

### Фронт

6. **Banner в `/me/profile`** — статусы pending/rejected.
7. **Страница `/admin/moderation`** — список + детальная + reject модалка.
8. **Бейдж в админ-навигации** — счётчик pending.

### Деплой

9. **Миграция на проде** → backfill уже-published'нутых → деплой бэка.
10. **Деплой фронта** — баннер + админка.
11. **Verify**: создать тестового спеца, проверить весь флоу.

Каждый шаг проверяется тестами; деплой после §5 — серым флагом
(`MODERATION_ENABLED=true` в env), чтобы в случае проблем быстро
откатить через env-флаг без миграций.

---

## 9. Альтернативы (рассмотрены)

| Вариант | Почему нет |
|---|---|
| **Без модерации** (сейчас) | На MVP высокий риск спама / фейков → бренд страдает с первых дней |
| **LLM-классификатор** в pipeline | Не готовы доверять модели на старте, нужны labeled data сначала |
| **Self-verify через ID/документы** | Серьёзный onboarding-trade off, лучше попозже |
| **Reactive moderation** (видим в каталоге, по жалобам убираем) | Низкое качество каталога с первого дня = плохой первое впечатление |
| **Ручная апрув через прямой SQL** | Не масштабируется, нет audit-trail кто и когда одобрил |

---

## 10. Связанные

- **CRM v5 admin кабинет** — уже есть `/admin/board`, `/admin/projects`.
  Добавляем `/admin/moderation` как ещё одну секцию.
- **Auth flow** — спец уже verified (email + опубликован). Moderation
  — следующий слой gate'a.
- **Outbox + reindex** — используем existing `specialist.upserted` и
  `specialist.retracted` события, новый код для них не нужен.
