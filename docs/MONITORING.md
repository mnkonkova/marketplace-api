# Мониторинг через Grafana Cloud

Метрики (RPS, latency, errors, CPU/RAM/диск VDS) и логи всех docker-контейнеров
уезжают в **Grafana Cloud free tier**. Self-hosted Grafana/Prometheus поднимать
не нужно. Free tier (10k метрик-серий, 50 GB логов, 14 дней retention)
покрывает MVP с многократным запасом.

Стек на VDS:
- **node-exporter** — раздаёт хост-метрики на `node-exporter:9100`.
- **api** — раздаёт HTTP-метрики (`http_requests_total`, `http_request_duration_seconds`)
  + runtime Go (GC, goroutines, alloc) на `api:8080/metrics`.
- **alloy** (Grafana Alloy) — скрейпит оба + тащит логи docker-сервисов,
  пушит в Grafana Cloud по remote_write/loki.api.

Всё уже в `docker-compose.prod.yml` и поднимется на `make redeploy`.
Осталось только зарегистрироваться в Grafana Cloud и вписать креды.

---

## 1. Регистрация в Grafana Cloud

1. Перейди на https://grafana.com/auth/sign-up/create-user — Sign up free
   (без карты, бесплатно).
2. Создай **Stack** (имя любое, регион — ближайший к Timeweb, например `eu-west-1`).
3. После создания откроется главная — там видно ссылку на твой grafana
   (https://**название**.grafana.net) и блок **Connections**.

## 2. Получить креды для Prometheus

1. В личке: **Connections** → **Add new connection** → **Hosted Prometheus metrics**.
2. На странице "Send Metrics" будут поля:
   - **Remote Write Endpoint** → это `GRAFANA_CLOUD_PROM_URL` (вида
     `https://prometheus-prod-XX.grafana.net/api/prom/push`).
   - **Username / Instance ID** → это `GRAFANA_CLOUD_PROM_USER` (число).
3. Сгенерируй API key:
   - Жми **Generate now** на той же странице (или в Account →
     **Access Policies** → **Add access policy**).
   - Permissions: `metrics:write` + `logs:write` (одним токеном для обоих).
   - Скопируй сгенерированный токен → это `GRAFANA_CLOUD_PROM_KEY`
     (и `GRAFANA_CLOUD_LOKI_KEY` — тот же).

## 3. Получить креды для Loki

1. **Connections** → **Hosted logs**.
2. **Push Endpoint** → `GRAFANA_CLOUD_LOKI_URL` (вида
   `https://logs-prod-XXX.grafana.net/loki/api/v1/push`).
3. **Username** → `GRAFANA_CLOUD_LOKI_USER` (число, отличается от
   prometheus user).
4. `GRAFANA_CLOUD_LOKI_KEY` — тот же токен из шага 2 (если делал с
   `logs:write` permission).

## 4. Вписать в `.env.prod`

```bash
ssh root@194.87.131.153
cd /opt/marketpclce/api
nano .env.prod
```

Заполни шесть `GRAFANA_CLOUD_*` полей. Применить:

```bash
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d alloy node-exporter
```

(или просто `make redeploy` — он переподнимет всё, включая alloy.)

Проверка что пишет в облако:
```bash
docker compose -f docker-compose.prod.yml --env-file .env.prod logs -f alloy
```
В логах ищи `level=info component=prometheus.remote_write` без `error`. Если
ошибки 401 — токен/username неправильные.

## 5. Импорт дашбордов в Grafana

В UI grafana (`https://твоя.grafana.net`) → **Dashboards** → **New** →
**Import** → вставь ID:

| ID         | Что показывает                                            |
|------------|-----------------------------------------------------------|
| **1860**   | Node Exporter Full — CPU/RAM/диск/сеть VDS                |
| **6671**   | Go Processes — runtime (GC, goroutines, allocs)           |
| **14584**  | Application HTTP overview (требует переменную datasource)  |

Для каждого после импорта проверь, что **datasource = твой prometheus stack**
(дефолтный) и `instance="marketpclce-prod"` отфильтровывается.

Свой дашборд для HTTP по нашим меткам собирается за 5 минут на основе:

- **RPS по эндпоинтам:**
  `sum by (route) (rate(http_requests_total{job="marketpclce-api"}[5m]))`
- **p95 latency:**
  `histogram_quantile(0.95, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))`
- **Error rate (5xx %):**
  `sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))`

## 6. Алерты в Telegram

1. **Contact points** → **Add contact point** → Type **Telegram**.
2. Создай бота через [@BotFather](https://t.me/BotFather), сохрани токен.
3. Узнай свой chat_id через [@userinfobot](https://t.me/userinfobot).
4. Вписать оба в contact point, **Test** — должно прийти "Test notification".

Рекомендуемый минимум алертов (Alerting → **Alert rules** → **New**):

| Имя              | Условие                                                              | Severity |
|------------------|----------------------------------------------------------------------|----------|
| API down         | `up{job="marketpclce-api"} == 0` > 1 минуты                          | critical |
| Disk near full   | `node_filesystem_avail_bytes{mountpoint="/rootfs"} / node_filesystem_size_bytes < 0.15` > 5 минут | warning  |
| OOM risk         | `node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes < 0.1` > 5 минут | warning  |
| 5xx burst        | `sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05` > 2 минут | warning  |
| OpenSearch hang  | `absent(rate(http_requests_total{route="/api/v1/search"}[5m]))` > 5 минут | warning  |

Каждое правило → **Add labels** → `severity=critical|warning` → **Notifications** →
your Telegram contact point.

## 7. Логи в Grafana

В UI: **Explore** → выбери datasource **Loki** → запросы:

```logql
{container="marketpclce-api-1"} | json
{container="marketpclce-api-1"} |~ "error"
{container=~"marketpclce-.*"} |~ "5\\d{2}"   # пятисотки во всех сервисах
```

Поля slog-логов (`method`, `path`, `status`, `req_id`, `dur_ms`) парсятся
автоматически через `| json`. Можно фильтровать как
`{container="marketpclce-api-1"} | json | status >= 500`.

---

## Раннбук: ESLatencyHigh (p95 поиска > 2s)

Разбор инцидента 2026-08-17 — сценарий типовой, повторится.

**Сначала посмотри на `op` в алерте.** `search` — лагает каталог у
пользователей; `bulk` — застревает outbox-индексер, выдача устаревает, но
страницы живые. Дальше диагностика общая.

Порт 9200 в проде наружу не проброшен, поэтому `curl` с хоста молча вернёт
пустоту (с `-s` ошибка съедается). Ходи изнутри контейнера:

```bash
docker exec api-opensearch-1 curl -s "localhost:9200/_cat/nodes?v&h=name,heap.percent,ram.percent,cpu,load_1m"
docker exec api-opensearch-1 curl -s localhost:9200/_nodes/stats/jvm | jq '.nodes[].jvm.gc.collectors'
docker exec api-opensearch-1 curl -s localhost:9200/_cat/allocation?v
docker stats --no-stream api-opensearch-1     # ← смотреть в первую очередь
df -h /var/lib/docker
free -h
```

Что искать, по убыванию вероятности:

1. **Контейнер у своего mem_limit** (`docker stats` показывает ~99%). Самый
   частый случай и самый неочевидный: на хосте при этом может быть 13 ГБ
   свободных, а `OOMRisk` смотрит именно на хост и молчит. Механика — heap
   помещается, а вот page cache для сегментов Lucene внутри cgroup жить
   негде, и поиск начинает читать с диска на каждом запросе (виден рост
   BLOCK I/O). Лечится подъёмом `mem_limit` **и** heap в
   `docker-compose.prod.yml`, правило: heap ≈ треть-половина лимита,
   остальное ядру под кэш. Поднимать только heap — сделать хуже.
2. **Диск под watermark.** `_cat/allocation` + `df -h`. При заполнении OS
   уходит в read-only (`cluster.blocks.read_only_allow_delete`), пишущие
   операции встают. Чистить место, потом снимать флаг вручную.
3. **Old-GC растёт** при живом лимите памяти — heap реально мал под объём
   данных. Тот же фикс, что в п.1.
4. **Всплеск soft-relax.** При `Total < 5` и активных фильтрах
   `internal/search/service.go` шлёт второй запрос без фильтров. Если
   выросла доля пустых выдач (сломался анализатор, уехал индекс) — нагрузка
   на кластер удваивается. Проверяется по доле запросов с `relaxed` в
   ответе.

Пересоздание контейнера (`docker compose -f docker-compose.prod.yml up -d
opensearch`) роняет поиск примерно на минуту — делать в тихое окно.

## Раннбук: ContainerMemoryNearLimit

Контейнер больше 15 минут держит `working_set` выше 90% своего `mem_limit`.
Это ранний сигнал того же класса проблем, что выше: сервису тесно в своём
cgroup, хост-алерты при этом молчат.

```bash
docker stats --no-stream                 # кто именно и насколько
docker inspect <name> --format '{{.HostConfig.Memory}}'
```

Дальше либо поднять лимит в `docker-compose.prod.yml` (для JVM-сервисов —
вместе с heap), либо искать утечку, если рост монотонный и без нагрузки.
Метрики приходят от cAdvisor-экспортера внутри Alloy — если алерт «No data»,
проверь моунты сервиса `alloy` в compose и `docker compose logs alloy`.

---

## Сколько сбор статистики стоит в обходах

Обход instacurl — единица расхода: один URL за один поход. Кредиты у
поставщика кончаются молча, поэтому полезно знать цифру заранее.

Правило расписания (`CollectInterval` в `internal/publications/collect.go`):
ролик 0–5 дней обходится ежедневно, 6–14 — раз в два дня, 15–28 — раз в
четыре, дальше раз в восемь. Ссылок на ролик — пять, по числу площадок.

Обход кончается подытогом периода, а не жизнью проекта. Период
подытоживается через `DefaultPeriodLockDelay` (14 дней) после своего
конца; в этот момент `billing.parkPeriodLinks` ставит ссылкам периода
`next_collect_at = 'infinity'`, и очередь их больше не видит. Смысл
именно в этих двух неделях: последние ролики периода успевают догнать
просмотры до расчёта, а после расчёта числа заморожены срезом, и новый
обход записывать некуда.

Отсюда главное свойство: расход проекта НЕ растёт со временем. Ролик
живёт в обходе от выхода до подытога своего периода — от 15 дней (вышел
в последний день периода) до 44 (вышел в первый), в среднем около
месяца. Это 11–17 обходов на ссылку, в среднем 14,7.

| роликов в месяц | обходов в сутки, на любом сроке проекта |
|---|---|
| 30 | ~73 |
| 60 | ~147 |

`CollectionRetention` (28 дней от закрытия проекта) остался страховкой
для схлопывания ряда: он решает, когда удалить поденную историю, а не
когда прекратить обход.

Снятая с обхода ссылка помечена `next_collect_at = 'infinity'`
(`publications.ParkedAt`), и ни одна запись этого поля её оттуда не
вынимает: сборщик, правка адреса и отсрочка пачки оборачивают новое
значение в `keepParked`. Иначе ссылка воскресала бы НАВСЕГДА — подытог
для её периода уже был и второй раз не случится.

Переоткрытие периода (`UnlockPeriod`, только админ) возвращает ссылки в
очередь: пересчитывать иначе было бы нечего.

HTTP-вызовов в десять раз меньше: пачка уходит одним запросом
(`STATS_COLLECT_BATCH`, по умолчанию 10 ссылок).

Потолок воркера — `STATS_COLLECT_BATCHES_PER_TICK × STATS_COLLECT_BATCH ×
тиков в сутки`, по умолчанию 20 × 10 × 24 = **4800 обходов в сутки на
всю площадку**. При расходе, который не растёт со временем, это около
65 проектов по 30 роликов в месяц или 33 по 60 — независимо от того,
сколько им лет.

Сколько ест конкретный проект — считается по базе:

```sql
SELECT pr.title,
       count(*) AS links,
       round(sum(1.0 / GREATEST(l.collect_interval_days, 1)), 1) AS обходов_в_сутки
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id AND p.status <> 'cancelled'
JOIN projects pr ON pr.id = p.project_id
WHERE pr.is_test = FALSE
  -- Снятые с обхода не считаем: их период уже подытожен.
  AND l.next_collect_at <> 'infinity'::timestamptz
GROUP BY pr.title ORDER BY 3 DESC;
```

Расход стерегут три правила в `grafana/alerts.yml`: `CollectVolumeSpike`
(вдвое больше вчерашнего часа — кто-то сбросил расписание),
`CollectNearDailyCapacity` (три четверти суточного потолка) и
`CollectQueueSaturated` (пачки заполняются целиком — очередь не
разгребается). Остальные правила той же группы ловят поломки, а не
расход.

### Сколько добавляет обход аккаунтов

Обход аккаунтов (`internal/publications/accountscan.go`) ищет ролики,
которые вышли, но не сданы ссылкой: раз в сутки ходит по адресам из
`project_accounts` и кладёт новое в «это ваш ролик?». Поход по адресу
аккаунта стоит ровно столько же, сколько поход по ссылке на ролик, —
один обход, один кредит.

Считается он не так, как сбор, и в этом вся разница:

| | сбор по ссылкам | обход аккаунтов |
|---|---|---|
| от чего растёт | от числа роликов в месяц | от числа аккаунтов |
| со временем | не меняется: ролик уходит из обхода с подытогом | не меняется, пока проект жив |
| за сутки | см. таблицу выше | **по одному на аккаунт** |

Аккаунтов у проекта — креаторы × площадки. Три креатора на пяти
площадках дают 15 обходов в сутки, и столько же они будут стоить на
шестой месяц проекта, когда сбор по архиву давно ушёл на интервал в
восемь дней. Для проекта из таблицы выше (30 роликов в месяц, полгода)
это +15 к 164, то есть примерно +9%; для проекта, который только
начался, — +15 к 71, то есть +21%.

Потолок воркера — `ACCOUNT_SCAN_BATCHES_PER_TICK × ACCOUNT_SCAN_BATCH ×
тиков в сутки`, по умолчанию 5 × 5 × 4 = **100 аккаунтов в сутки**, то
есть примерно шесть проектов по три креатора. Дальше очередь начнёт
отставать, и находки станут приходить не на следующий день, а через
два.

**Обход выключен по умолчанию.** Включается `ACCOUNT_SCAN_ENABLED=true`
в окружении воркера (нужен и настроенный `INSTACURL_URL`/
`INSTACURL_API_KEY` — сервис тот же). Включать стоит, посмотрев на
остаток кредитов у поставщика (`ScrapeCreatorsCreditsLow`): обход
тратит их каждый день независимо от того, вышло что-нибудь на аккаунте
или нет. Расход стережёт `AccountScanVolumeSpike` — отдельное правило,
потому что и счётчик отдельный (`crm_account_scan_total`): сложенные с
`crm_stat_collect_total`, эти два расхода нельзя было бы ни развести по
причинам, ни спланировать.

Сколько аккаунтов стоит в очереди — считается по базе:

```sql
SELECT pr.title, count(*) AS аккаунтов
FROM project_accounts a
JOIN projects pr ON pr.id = a.project_id
WHERE a.creator_user_id IS NOT NULL AND a.url <> '' AND a.platform <> 'other'
  AND pr.status <> 'cancelled'
  AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > now())
GROUP BY pr.title ORDER BY 2 DESC;
```

## Что отключить, если кончились лимиты Grafana Cloud

Free tier — 10k активных метрик-серий. Если упрёшься (увидишь в Grafana
Cloud → **Billing/Usage**):

- В `alloy/config.alloy` к `prometheus.remote_write` добавь
  `metric_relabel_configs`, отфильтруй ненужные `go_*` runtime-метрики:
  ```alloy
  metric_relabel_configs = [{
    source_labels = ["__name__"],
    regex         = "go_(memstats_.*|sched_.*|gc_.*)",
    action        = "drop",
  }]
  ```
- `scrape_interval` поднять с 30s до 60s (вдвое меньше серий).

Для логов лимит — 50 GB/месяц. У нас realistic ~200 MB/день при 1000
пользователях, так что предел далёкий. Если упрёмся — фильтр на уровне
loki.source.docker для исключения болтливых сервисов.
