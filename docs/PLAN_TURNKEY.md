# План: новая логика кнопки «под ключ»

Собрано 24 сентября 2026 по разбору обоих репозиториев
(`marketplace-api`, `marketplace-web`) и решениям владельца продукта.
Все пути и имена проверены по коду; где написано «нет» — значит в коде
этого действительно нет.

Статус: **план**, кроме одного пункта, который уже сделан (см. «Что
уже сделано»).

---

## Как выглядит сценарий

1. Заказчик жмёт «под ключ» и **заполняет бриф**.
2. **Выбирает креаторов списком**, галочками; кого-то отмечает «хочу
   особенно этих».
3. **Ползунком задаёт общее число роликов в месяц.** На экране —
   «меньше N ₽», без разбивки и уточнений.
4. Жмёт «Отправить». Видит «мы напишем по стоимости и креаторам».
5. Приглашение уходит **всем креаторам, у кого подключён бот**;
   отмеченным — с припиской «заказчик хочет особенно вас». Менеджерам в
   чат — «новая заявка».
6. Креатор отвечает одним из трёх способов: прикрепить файл в кабинете,
   прислать файл прямо в бот, отправить готовые ролики из своих.
7. Менеджер видит список «кто точно согласен», набирает из него состав,
   правит оплату (тариф дефолтный), отмечает обязательные пункты
   чеклиста, ставит даты выкладок и **финализирует** — создаётся проект.
8. Креаторам уходит уведомление, договор (из материалов проекта), ТЗ и
   даты начала съёмок.

Договор с клиентом и отбор креаторов — по телефону, это вне системы.

## Деньги

| Что | Сколько |
|---|---|
| Фикс заказчику | 1 000 ₽ за вышедший ролик |
| Фикс креатору | 500 ₽ за вышедший ролик |
| Ступени (у КАЖДОГО креатора свои) | 500 000 просмотров → 2 500 ₽ · 1 000 000 → 5 000 ₽ |
| Порог на ролик (`bonus_views_threshold`) | не участвует, 0 |
| Ставка сверх порога (`rate_per_1000_views_over`) | не участвует, 0 |

Правила, которые легко потерять:

- **Между порогами цена не растёт.** 700 000 просмотров — это ступень
  500 000, то есть 2 500 ₽, а не пропорция.
- **Ступень берёт человек, а не проект.** Трое по полмиллиона — три
  ступени по 2 500 ₽, а не одна за миллион на всех.
- **Верхняя ступень и есть потолок.** Отдельного числа
  («потолок бонуса») не нужно: бар считается по ней.
- **Бар = ролики × 1 000 + креаторы × цена верхней ступени.** Для двух
  креаторов и 30 роликов — «меньше 50 000 ₽». Округления нет.

---

## Что уже сделано

**Ступень берётся просмотрами каждого креатора** —
`internal/billing/calc.go`, функция `steppedAccruals`. Раньше период
считался от агрегата (`aggregateFacts` → `calcPeriod`) и раскладывался
по людям пропорционально вкладу; теперь строка человека считается из его
же фактов. Вместе с этим ушли раскладка по долям, остаток от деления и
вычитание утверждённых строк из цены периода — «сумма строк равна цене
периода» выполняется само собой.

Побочное правило, которое пришлось назвать явно: человек без единой
выкладки и без просмотров строки не получает, иначе нижняя ступень
лесенки «от 0 просмотров» доставалась бы каждому, кто числится в
составе.

Прогноз «что даст следующая ступень» (`nextStepForecast` в
`internal/billing/service.go`) считается там же и по его просмотрам.

---

## Ф0. Починить то, что уже сломано

Пять правок в написанном коде. Каждая иначе будет воспроизведена в новой
фиче.

1. **Материалы не доезжают до креатора вообще.**
   `internal/publications/materials.go:135` и `:170` сравнивают audience
   с `'creator'`/`'all'`, а легальные значения — `'creators'`/`'client'`
   (CHECK в `migrations/00036_materials_autoping.sql:17`). Поэтому
   `ReminderMaterialsUpdated` не отправляется никогда. Тот же рассинхрон
   в SQL внутри `brief.go:74` и `:147` — счётчик `BriefUpdate.Materials`
   всегда ноль.
2. **`orderFacts` (`internal/billing/estimate.go:82`) не читает
   `fee_per_video`** из версии прайса, в отличие от `Repo.LatestTerms`.
   Смета до заказа считает по фиксу за ролик, смета по заказу — по
   окладу. Два разных числа одному человеку.
3. **`SideTerms` (`internal/billing/client_view.go`) не несёт
   `fee_per_video`/`creator_fee_per_video`** — ветка `@if
   (e.terms.fee_per_video)` в корзине воронки мёртвая.
4. **`TestDraftEstimateMatchesOrderEstimate`**
   (`tests/integration/billing_test.go:1349`) не выставляет
   `fee_per_video` и потому расхождения не ловит.
5. **`project_materials.url` не в `LoadReferencedMediaURLs`**
   (`internal/profiles/repo.go:1346`). Сегодня безопасно (там только
   внешние ссылки, `KeyFromURL` их отсекает), но как только менеджер
   сможет приложить файл — подметальщик снесёт его через
   `S3_ORPHAN_MIN_AGE`. Выкатить отдельным маленьким коммитом сейчас,
   пока правка бесплатна.

---

## Ф1. Деньги

**Ф1a — смета и данные, расчётное ядро не трогается.**

- Миграция `00066`: `terms_versions` и `project_billing` получают
  `fee_per_creator`-подобных колонок **не заводим** — потолок выражается
  верхней ступенью лесенки (`terms_steps`).
- Новая функция `upperBound(t Terms, creators, videos int) int64` рядом
  с `fixPart` в `internal/billing/estimate.go`:
  `videos × FeePerVideo + creators × (цена верхней ступени)`.
  `bonusForOneVideo` не зовётся, порог не участвует, округления нет.
- Поле `OrderEstimate.LessThan`; на фронте — одна строка «меньше N ₽»
  вместо блока `.btot` и разбивки корзины
  (`src/pages/me/order-funnel/order-funnel.page.html`).
- `Service.EnsureProjectTerms` — upsert снимка из `LatestTerms` при
  создании проекта (сегодня `refreshNotStartedProjects` делает только
  UPDATE и проекту без строки не даёт ничего, а `Repo.Terms` на
  отсутствие строки возвращает нули). Для `creators_turnkey`
  принудительно обнуляет `bonus_views_threshold` и
  `rate_per_1000_views_over`.
- Дефолтная версия прайса «под ключ» выпускается через `POST
  /admin/terms`: `fee_per_video = 100000`, `creator_fee_per_video =
  50000`, ступени `(500000 → 250000коп)` и `(1000000 → 500000коп)`,
  порог и ставка сверх порога — нули, оклад за месяц — ноль.

Ломает: `TestOrderEstimate` (`tests/integration/billing_test.go:693`),
`TestDraftEstimateMatchesOrderEstimate` (:1349),
`TestDraftEstimateWithoutRosterAndVolume` (:1483),
`tests/e2e/scenarios_test.go:122 TestE2EOrderToPaid` (строки 175-187
жёстко ждут `salaries == 6000000`), `web/e2e/specs/order-funnel.spec.ts`
(сверяет текст `.btot b`), `web/test-specs/project-tariff.spec.ts`,
`web/e2e/specs/admin-tariff.spec.ts`, `web/e2e/specs/billing-leaks.spec.ts:264`.

**Ф1b — запрет смешивания моделей.** В `Service.SaveTerms`/`checkRates`
и в `AdoptLatestTerms`: проекту «под ключ» нельзя протащить старую
модель (оклад за месяц, порог на ролик) обновлением из прайса.

---

## Ф2. Бриф, список креаторов, бар

Миграция `00065`:

```sql
CREATE TABLE order_briefs (
    order_id   UUID PRIMARY KEY REFERENCES creator_orders(id) ON DELETE CASCADE,
    goal       TEXT NOT NULL DEFAULT '',
    product    TEXT NOT NULL DEFAULT '',
    audience   TEXT NOT NULL DEFAULT '',
    tone       TEXT NOT NULL DEFAULT '',
    refs       TEXT NOT NULL DEFAULT '',
    platforms  TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE order_candidates
    ADD COLUMN is_preferred BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN broadcast_at TIMESTAMPTZ;

ALTER TYPE creator_order_status   ADD VALUE IF NOT EXISTS 'submitted';
ALTER TYPE order_candidate_status ADD VALUE IF NOT EXISTS 'responded';

-- Лимит 1..3 перестаёт быть обещанием клиента: состав утверждает менеджер.
ALTER TABLE creator_orders DROP CONSTRAINT creator_orders_needed_positive;
ALTER TABLE creator_orders ALTER COLUMN needed DROP NOT NULL;
ALTER TABLE creator_orders ADD CONSTRAINT creator_orders_needed_sane
    CHECK (needed IS NULL OR needed BETWEEN 1 AND 50);
```

Бэкенд: `OrderBrief` в `internal/orders/dto.go`, `saveBrief`/`loadBrief`
в `repo.go`, `brief` и `preferred_ids` в `createOrderReq`, ручка `PATCH
/me/orders/{id}/brief`. В `Service.Create` снимаются `ErrTooManyCreators`
и `ErrNotEnoughCandidates`; занятость (`BusyCreators`) становится
предупреждением, а не отказом.

Фронт: `src/pages/me/order-funnel/` — шаги `['Бриф','Креаторы','Объём',
'Заявка принята','Проект']`, плоский список креаторов с чекбоксом «хочу
особенно», бар вместо разбивки. Разметка сразу для десктопа и тача
(`_sotka.scss`, `_sotka-touch.scss`, `widgets/sotka-tabbar`).

Ломает: `tests/integration/orders_test.go` (`TestFirstMonthAllowsOneCreator`
:94, `TestReserveExhaustedThenTopUp` :254), весь
`tests/integration/priority_test.go`, `web/e2e/specs/order-funnel.spec.ts`,
`web/e2e/specs/order-roster.spec.ts`.

---

## Ф3. Рассылка

**Ф3a — не зависит от бота:** статус `submitted`, событие
`order.created` в общий чат менеджеров (строка в `chatRouting`, ветка в
`deploy/n8n/workflows/crmTgEventsV1.json`, типы в `dynamicEmitSites`).
Сузить `Repo.ExpireAndAdvance` до `status = 'inviting'`, иначе заявки
протухнут через 72 часа и завалят чат пингами «креатор не отвечает».

**Ф3b — зависит от бота:** `Repo.KnownCreators` и
`Repo.BroadcastInvitations`, событие `order.broadcast_sent`.

«Известный креатор» — **у кого подключён бот** и профиль одобрен либо на
модерации:

```sql
SELECT b.user_id
FROM telegram_links b            -- см. план по ботам
JOIN users u ON u.id = b.user_id
JOIN specialist_profiles sp ON sp.user_id = b.user_id
WHERE u.is_active AND NOT u.is_manager AND NOT u.is_admin
  AND u.kind IN ('specialist', 'both')
  AND sp.moderation_status IN ('approved', 'pending_review')
  AND b.blocked_at IS NULL
  AND (SELECT count(*) FROM notification_log n
        WHERE n.user_id = b.user_id AND n.sent_date = CURRENT_DATE) < 30
```

Потолок — **30 сообщений в сутки на человека**, считается по
`notification_log`, новой таблицы не нужно. Категории `blogger`/`ugc` и
`is_published` в рассылке **не** участвуют.

Обязательно: лог «рассылка ушла N получателям». Ноль должен быть виден —
пока никто не нажал `/start`, получателей нет, и это не ошибка.

Тексты — в плане по ботам.

---

## Ф4. Отклик креатора и файлы

Миграция `00067`:

```sql
CREATE TABLE order_candidate_responses (
    order_id        UUID NOT NULL,
    creator_user_id UUID NOT NULL,
    mode            TEXT NOT NULL
        CHECK (mode IN ('attach', 'upload', 'from_portfolio', 'decline')),
    -- file_url — объект в бакете под префиксом orders/. ВАЖНО: колонка
    -- обязана быть в Repo.LoadReferencedMediaURLs, иначе SweepOrphanMedia
    -- снесёт живой файл через S3_ORPHAN_MIN_AGE; префикс "orders/"
    -- обязан быть в prefixes там же, иначе удалённые отклики оставят
    -- мусор в бакете навсегда.
    file_url        TEXT,
    note            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, creator_user_id),
    FOREIGN KEY (order_id, creator_user_id)
        REFERENCES order_candidates(order_id, creator_user_id) ON DELETE CASCADE
);

CREATE TABLE order_response_portfolio_items (
    order_id        UUID NOT NULL,
    creator_user_id UUID NOT NULL,
    item_id         UUID NOT NULL REFERENCES portfolio_items(id) ON DELETE CASCADE,
    PRIMARY KEY (order_id, creator_user_id, item_id),
    FOREIGN KEY (order_id, creator_user_id)
        REFERENCES order_candidate_responses(order_id, creator_user_id) ON DELETE CASCADE
);
```

Ручка пресайна `POST /me/creator/uploads/work-sample` — копия
`profiles.Service.CreatePortfolioUploadURL`
(`internal/profiles/service.go:1026`), ключ
`orders/{order_id}/{creator_user_id}/{uuid}.ext`, те же ограничения
(mp4/mov, 50 МБ одним PUT, 200 МБ multipart), та же rate-limit группа
`uploads` (`internal/httpapi/router.go:271`).

**Две обязательные правки в `internal/profiles`, без которых ручку
выпускать нельзя:**

1. `order_candidate_responses.file_url` — в UNION
   `Repo.LoadReferencedMediaURLs` (`repo.go:1346`);
2. `"orders/"` — в `prefixes` в `SweepOrphanMedia` (`cleanup.go:55`).

Забыть первое — терять живые файлы через сутки; забыть второе — копить
мусор навсегда.

«Отправить из моих» новой ручки не требует: `GET /me/portfolio`
(`profiles.Handler.PortfolioList`) уже отдаёт нужный список.

Фронт: новая страница `src/pages/me/creator-invitations/` — сегодня
`OrderApi.invitations()` и `respondInvitation()` объявлены, но их не
вызывает никто.

---

## Ф5. Финализация менеджером

Новая ручка `POST /manager/orders/{id}/finalize` → создаёт проект
`kind='creators_turnkey'`, вызывает `LinkProject` (**метод есть, но не
смонтирован ни на один HTTP-маршрут — в проде заказ и проект не связаны
вообще**), кладёт бриф в `projects.notes`, `videos_count` в
`projects.monthly_plan`, добавляет креаторов, зовёт
`EnsureProjectTerms`. Чеклист прицепится сам:
`projects.Service.attachChecklist`
(`internal/projects/checklist_attach.go:39`) уже делает это для
`creators_turnkey`.

Новая ручка `PATCH /manager/projects/{id}/checklist/items/{itemId}` с
`{is_required}` — сегодня есть только добавление и удаление, переключить
обязательность нельзя.

Новая ручка `GET /manager/orders/{id}/responses` +
`Repo.ListResponses` — список «кто точно согласен»: аватар, имя, ⭐ если
отмечен клиентом, медиана просмотров, вложение (файл или превью из
портфолио), кнопка «Взять в проект».

Фронт: раздел «Команда» в
`src/widgets/manager-turnkey-project/` — блок «Согласны на заявку (N)»
над списком состава и кнопкой «Добавить креатора».

Ломает: `tests/integration/orders_test.go:540
TestManagerProjectOrderAndInvite` и `:647 TestOrderChatEventsCarryProjectID`
(оба сейчас линкуют проект напрямую через `repo.LinkProject`).

---

## Ф6. Договор и ТЗ креатору

Миграция `00068`: `project_materials.kind` получает значение
`'contract'`; договор отдаётся первым в `ListMaterials` и выделяется в
кабинете креатора отдельной плашкой. Целиком опирается на Ф0.1 — без
починки audience материалы до креатора уведомлением не доезжают.

---

## Открытые вопросы

1. **Старая очередь приглашений.** При рассылке всем не нужны
   `InviteTTL` (72 ч), `inviteNext`, `ClientReorderPriority`,
   `ExpireAndAdvance`. Удалять или оставить вторым путём для доборов? За
   этим 4 из 10 тестов `orders_test.go`, весь `priority_test.go` и
   `order-roster.spec.ts`.
2. **Договор один на проект или свой у каждого?** В
   `project_materials` нет `creator_user_id`; если свой — нужна колонка
   или отдельная таблица.
3. **Заявки на канбане.** У `creators_turnkey` воронки нет по дизайну
   (CHECK `projects_pipeline_required` в
   `migrations/00040_kind_on_create.sql:12`), поэтому заявка не встанет
   ни в одну колонку `/admin/board`. Нужен ли отдельный экран «Заявки»?
4. **Креатор согласился, но не взят.** `RemoveCreator`
   (`internal/publications/repo.go:116`) не шлёт ничего. Молчим,
   пишем «в этот раз не подошло», или ставим в очередь на следующие
   заявки?
