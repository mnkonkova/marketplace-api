# Grafana: обновление дашборда и алертов

Этот файл — практический how-to для двух задач:
1. Обновить дашборд `marketpclce — HTTP + Outbox + LLM + CRM + OS`.
2. Залить/обновить алерт-правила из `grafana/alerts.yml`.

Конкретный Grafana Cloud стек уже настроен — см. `docs/MONITORING.md`
(там описано как получить креды Prometheus/Loki и подключить Alloy).

Все файлы которые тут упоминаются лежат в репо:
- `grafana/http-dashboard.json` — дашборд (uid: `marketpclce-http`)
- `grafana/alerts.yml` — Prometheus rule groups

---

## A. Обновить дашборд

### Способ 1: UI (быстро, разово)

1. В Grafana Cloud: **Dashboards** → **New** → **Import**.
2. Жми **Upload JSON file** и выбери `grafana/http-dashboard.json` ИЛИ
   скопируй содержимое и вставь в **Import via panel json**.
3. На странице импорта:
   - **Name**: оставь как есть (берётся из JSON).
   - **Folder**: выбери `marketpclce` (создай если нет).
   - **DS_PROMETHEUS**: укажи свой Prometheus datasource (обычно
     `grafanacloud-<stack>-prom`).
4. Жми **Import**. Если дашборд с тем же uid (`marketpclce-http`) уже
   есть — Grafana **перезапишет** его новой версией (`version` в JSON
   увеличился до 3). Изменения в старом дашборде (если их вносили
   через UI и не коммитили в git) — потеряются.

### Способ 2: API (для CI / повтор-друг-другу)

```bash
GRAFANA_URL="https://<stack>.grafana.net"
GRAFANA_TOKEN="<service account token с правом Dashboards: Write>"

# Оборачиваем JSON в формат API:
jq '{ dashboard: ., overwrite: true, folderUid: "marketpclce", message: "sync from git" }' \
  grafana/http-dashboard.json \
  | curl -sS -X POST "$GRAFANA_URL/api/dashboards/db" \
      -H "Authorization: Bearer $GRAFANA_TOKEN" \
      -H "Content-Type: application/json" \
      -d @-
```

Service account token: **Administration → Service accounts → Add** →
роль `Editor` достаточно. Сохрани в `1Password` или `.envrc.local`.

### Проверка после импорта

- Открой дашборд, переключи диапазон на `Last 1 hour`.
- В разделе **Async / Outbox**: `Outbox pending` и `Outbox dead-lettered`
  должны показывать число (а не `No data`). Если No data — Alloy ещё
  не доскрейпил воркер, подожди 1-2 минуты.
- В разделе **OpenSearch backend**: панели начнут заполняться после
  первого деплоя нового бинаря с `es_requests_total` / `es_request_duration_seconds`
  метриками. Тоже до 2 мин лага.
- В разделе **Outbox lease (P3)**: серии `pending` / `lockable` /
  `scheduled (lease + backoff)`. Если воркер свежий, `scheduled` = 0
  большую часть времени.

---

## B. Залить/обновить алерты

`grafana/alerts.yml` — стандартный Prometheus rule group format
(совместим с Mimir, Prometheus, VictoriaMetrics). В Grafana Cloud
правила хранятся в **Mimir**, и есть два пути их синхронизировать.

### Способ 1: UI (paste YAML, ручной mode)

1. **Alerts & IRM** → **Alert rules** → **New rule** → выбери
   **Mimir or Loki managed alert rule** (НЕ Grafana-managed).
2. На странице создания нажми **Edit YAML** (правый верхний угол).
3. Для каждой группы из `alerts.yml`:
   - **Namespace**: `marketpclce` (создаст автоматически).
   - **Group name**: значение из `name:` (например `marketpclce-http`).
   - В YAML-редактор вставь блок `rules:` целиком из соответствующей
     группы. **Без обёртки `groups:` и без `name:` сверху** — только
     поля внутри group'ы (interval, rules).
4. Жми **Save**. Повтори для каждой из 6 групп.

Это нудно, но рабоче если правил мало. Для 17 правил лучше CLI.

### Способ 2: mimirtool (для IaC, recommended)

`mimirtool` умеет `rules load` — синхронит весь файл одной командой.

```bash
brew install grafana/grafana/mimirtool   # или скачать с github releases

export MIMIR_ADDRESS="https://prometheus-prod-XX.grafana.net"   # из шагов docs/MONITORING.md
export MIMIR_TENANT_ID="<GRAFANA_CLOUD_PROM_USER из .env.prod>"
export MIMIR_API_KEY="<GRAFANA_CLOUD_PROM_KEY>"

# Sync файла с правилами:
mimirtool rules load grafana/alerts.yml

# Проверка что залилось:
mimirtool rules list
mimirtool rules print --rule-files grafana/alerts.yml
```

`mimirtool rules load` — idempotent. Запускать после каждого изменения
`grafana/alerts.yml`. Можно повесить в CI на push в main.

### Способ 3: Alloy (декларативно)

Если хочешь чтобы alloy сам деплоил правила при перезапуске —
добавь в `alloy/config.alloy`:

```alloy
mimir.rules.kubernetes "default" {
  // … либо для local-файлов:
}

local.file "alerts" {
  filename = "/etc/alloy/alerts.yml"
}

mimir.rules.cloud "default" {
  basic_auth {
    username = sys.env("GRAFANA_CLOUD_PROM_USER")
    password = sys.env("GRAFANA_CLOUD_PROM_KEY")
  }
  address = sys.env("GRAFANA_CLOUD_PROM_URL")
  rules = [local.file.alerts.content]
}
```

И смонтировать `grafana/alerts.yml` → `/etc/alloy/alerts.yml` в
`docker-compose.prod.yml`. Минус: alloy перезагружает rules при
рестарте, а не при изменении файла.

---

## C. Подключить уведомления

Правила без routing — это просто счётчики `ALERTS{}` метрики.
Чтобы доходило до телеги/email:

1. **Alerts & IRM** → **Contact points** → **New contact point**.
   - **Integration**: Telegram / Email / Slack / Webhook.
   - Для Telegram: вписать `bot_token` и `chat_id` группы (см.
     инструкции по бот-токену в Grafana docs).
2. **Notification policies** → корневой policy → **Edit** →
   - **Default contact point** → выбери только что созданный.
   - **Group by**: `alertname`, `severity` (для меньшего spam'a).
3. (Опционально) Добавь **nested policy** для `severity = page` →
   отдельная Telegram-группа `marketpclce-pages` (чтобы warn'ы и
   page'и были в разных чатах).

---

## D. Что должно работать после обновления

Чек-лист — пробежаться через час после деплоя:

- [ ] Дашборд видит outbox-метрики (`outbox_pending`, `outbox_dead`,
      `outbox_lockable`, `outbox_handler_success_total` и т.д.).
- [ ] Дашборд видит OS-метрики (`es_requests_total`, `es_request_duration_seconds`).
      Эти появятся **только** после ребилда воркера+api с новым кодом.
- [ ] В **Alerts & IRM** → **Alert rules** видно 6 групп (`marketpclce-http`,
      `marketpclce-crm-cabinets`, `marketpclce-outbox`, `marketpclce-llm`,
      `marketpclce-ratelimit`, `marketpclce-opensearch`).
- [ ] Все 17 правил — в состоянии **Normal** (зелёные). Если что-то
      в **Firing** — открой и почитай аннотацию, либо это реальная
      проблема, либо порог в `alerts.yml` нужно поправить.
- [ ] Тестовый алерт сработал → пришло уведомление в Telegram.
      (Можно spec'ом снизить `for: 1m` у `APIp95LatencyHigh` и поиграть
      нагрузкой `hey -z 30s ...`, потом откатить.)

---

## E. Куда копнуть если что-то сломалось

| Симптом | Где искать |
|---|---|
| Дашборд: `No data` во всех панелях | Datasource не выбран при импорте. Settings → Variables → `datasource` → проверь. |
| Дашборд: пустые `es_*` панели | Воркер/api с новым кодом ещё не задеплоен. `git log` по `internal/platform/es/metrics.go`. |
| Дашборд: пустой `outbox_lockable` | То же — нужен ребилд воркера. |
| Алерт постоянно firing на пустом стеке | Возможно метрика отсутствует (например, `outbox_lockable` = no series при холодном старте). Правило `OutboxLeaseLeak` требует `outbox_pending > 50` — на нулевом проде не сработает. |
| `mimirtool rules load` → 403 | Token истёк или у него нет `metrics:write`. Сгенери новый в Grafana Cloud → Access Policies. |
| Уведомления не приходят | Notification policies → проверь что точка контакта привязана. Bot должен быть admin'ом группы для отправки. |
