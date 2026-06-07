# Поднятие VDS с нуля

Шпаргалка для двух сценариев:

- **A. Чистый старт** — новая VDS, пустая БД, никаких данных. Запустить
  новый instance продукта (staging, demo, второй регион).
- **B. Переезд** — новая VDS, данные восстанавливаются из бекапа со
  старой машины (Postgres + n8n volume). Используется при смене провайдера
  или после катастрофы.

Стек на VDS: api / worker / web / postgres / opensearch / redis / n8n
+ observability (node-exporter + alloy). Всё под одним доменом, Caddy
проксирует.

См. также `docs/MONITORING.md` (Grafana Cloud).

---

## 0. Что должно быть на руках

| | Где взять |
|---|---|
| Домен | Любой регистратор; нужен поддомен `<домен>` под VDS IP |
| Yandex Cloud Service Account | console.cloud.yandex.ru — нужен ключ к S3 для медиа |
| Anthropic API key (опционально) | console.anthropic.com — LLM-фичи (summarize/clarify) |
| SMTP-аккаунт `noreply@<домен>` | reg.ru hosting — отправка verify/reset писем (через n8n) |
| SMTP-аккаунт `info@<домен>` | reg.ru hosting — приём обращений из футера UI |
| Telegram bot token (опционально) | @BotFather — n8n-нотификации |
| Grafana Cloud free tier (опционально) | grafana.com — метрики + логи |
| **Для сценария B**: tar-бекапы со старой VDS | `backups/marketpclce-*.sql.gz` и `backups/n8n/n8n-*.tar.gz` |

---

## 1. Заказать VDS

Любой провайдер с Ubuntu 22.04. ОС: только Ubuntu 22.04 LTS (на 24.04
тоже работает, но не тестировалось; на Debian — drop-in, но
`docker-compose-v2` ставится через docker-репо, не из apt).

### Тиры

| | Минимум (MVP/staging) | **Рекомендация (prod)** | Под нагрузкой (1k+ DAU) |
|---|---|---|---|
| RAM | 3 GB | **4 GB** | 8 GB |
| vCPU | 2 | 2 | 4 |
| Диск SSD | 40 GB | **60 GB** | 100 GB |
| Сеть | 100 Мбит/с | 100 Мбит/с | 1 Гбит/с |
| Локация | Москва (ru-1) для рос. аудитории |

> Минимум 3 GB требует ужать OpenSearch:
> `OPENSEARCH_JAVA_OPTS=-Xms384m -Xmx384m` в `docker-compose.prod.yml`.
> На 4 GB и выше не трогать.

### RAM-бюджет (4 GB конфигурация)

| Сервис | RAM idle | RAM peak | Примечание |
|---|---|---|---|
| postgres 16 | ~150 MB | ~600 MB | shared_buffers + кэш чтения |
| opensearch | ~700 MB | ~1.0 GB | Java heap 512m + off-heap |
| redis | ~30 MB | ~100 MB | rate-limit окна + кэш summarize |
| api (Go) | ~50 MB | ~200 MB | пулы pgx/redis/es |
| worker (Go) | ~50 MB | ~250 MB | outbox loop + s3 sweep + ffmpeg-транскод |
| web (Caddy + статика) | ~30 MB | ~50 MB | проксирует /api |
| n8n (Node.js) | ~250 MB | ~500 MB | + workflows под нагрузкой |
| node-exporter + alloy | ~80 MB | ~150 MB | observability агент |
| OS + Docker | ~250 MB | ~400 MB | |
| **Итого** | **~1.6 GB** | **~3.2 GB** | свободно ~800 MB на пики |

Если планируешь крутить **много** LLM-запросов (summarize/clarify) или
**частые** n8n-workflow'ы — бери 8 GB.

### Disk-бюджет

| Что | Старт | Через год (~100 проектов) | Через 3 года (~1000 проектов) |
|---|---|---|---|
| Docker-images (api/web/postgres/opensearch/redis/n8n) | ~4.2 GB | ~4.2 GB | ~5.2 GB |
| Postgres data | ~50 MB | ~500 MB | ~3 GB |
| OpenSearch index | ~20 MB | ~150 MB | ~1 GB |
| n8n volume (workflows + execution log) | ~10 MB | ~200 MB | ~1 GB |
| Бекапы (`backups/`, retention=3 дня) | ~5 MB | ~500 MB | ~2 GB |
| Логи Docker + system | ~50 MB | ~1 GB | ~3 GB |
| **Итого** | **~5 GB** | **~7 GB** | **~15 GB** |

40 GB хватает на MVP и год работы. 60 GB рекомендуется чтобы спокойно
держать долгую историю outbox/audit + локальные бекапы. Расти можно
ресайзом диска у провайдера (без остановки на большинстве VDS-хостов).

**Медиа (видео портфолио) на VDS НЕ хранится** — заливается напрямую
в Yandex Object Storage (S3) клиентом, читается тоже напрямую. VDS
держит только метаданные/URL. Поэтому 1 ТБ видео не повлияет на размер
диска VDS.

### Сеть

- Трафик через VDS: API-ответы (JSON), статика фронта, n8n webhook'и.
- Видео и фото — **минуют** VDS (S3 ↔ клиент напрямую).
- Реалистичный объём: ~5–20 ГБ/месяц на ранней стадии, ~100 ГБ/месяц
  при 1k DAU. У большинства VDS-провайдеров трафик безлимитный или
  лимит 1 ТБ/месяц — хватает с запасом.

Запиши IP и root-пароль.

---

## 2. Базовая подготовка

```bash
ssh root@<IP>

apt update && apt -y upgrade
apt -y install docker.io docker-compose-v2 git make ufw curl

# Firewall: только 22 / 80 / 443
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

systemctl enable --now docker

# OpenSearch требует sysctl
echo 'vm.max_map_count=262144' >> /etc/sysctl.conf
sysctl -p
```

---

## 3. Поддомен и DNS

A-запись `<домен>` → `<IP VDS>`, TTL 300.

```bash
dig <домен> +short    # должен показать IP VDS
```

**Без работающего DNS Caddy не получит TLS-сертификат** (Let's Encrypt
HTTP-01 challenge). Подожди 1–5 минут, проверь `dig`, и только потом
двигайся дальше.

---

## 4. Раскладка репо

```
/opt/marketpclce/
├── api/   ← marketplace-api (.env.prod и compose тут)
└── web/   ← marketplace-web (фронт)
```

```bash
mkdir -p /opt/marketpclce && cd /opt/marketpclce
git clone https://github.com/mnkonkova/marketplace-api  api
git clone https://github.com/mnkonkova/marketplace-web  web

cd api
cp .env.prod.example .env.prod
nano .env.prod
```

Обязательные поля `.env.prod`:

| Поле | Как получить / задать |
|---|---|
| `DOMAIN` | `<домен>` |
| `POSTGRES_PASSWORD` | `openssl rand -hex 16` |
| `DATABASE_URL` | `postgres://marketpclce:<password>@postgres:5432/marketpclce?sslmode=disable` |
| `OPENSEARCH_PASSWORD` | `openssl rand -base64 24` (≥10 символов, спец-символы) |
| `JWT_SECRET` | `openssl rand -hex 32` |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` / `S3_BUCKET` | Yandex Cloud → Service Account → Static access key |
| `LLM_API_KEY` | console.anthropic.com (опционально) |
| `N8N_WEBHOOK_URL` / `N8N_EMAIL_WEBHOOK_URL` / `N8N_SUPPORT_WEBHOOK_URL` | пока пустое (заполним в шаге 7) |

`CORS_ORIGINS` оставь пустым — фронт и API под одним доменом, CORS не нужен.

---

## 5. Развилка: A или B

### Сценарий A — чистый старт

```bash
cd /opt/marketpclce/api
make deploy
```

`make deploy` сам:
1. Сделает `git pull` в обоих репо.
2. Соберёт api + web образы.
3. Поднимет postgres / opensearch / redis.
4. Сделает **pre-migrate backup** (пустой, в `backups/` — ок).
5. Прогонит миграции через goose.
6. Запустит api / worker / web.
7. Caddy выпишет TLS-сертификат (10–60 сек).

Проверь:
```bash
make prod-ps              # все сервисы healthy
curl -fsSL https://<домен>/healthz
curl -fsSL https://<домен>/
```

(Опционально) заливка демо-данных:
```bash
make prod-seed              # 5 спецов + портфолио
make prod-seed-videos       # mp4 в S3
```

Перейди к шагу **7. n8n + Telegram**.

### Сценарий B — переезд со старой VDS

1. **На старой VDS** — снять свежие бекапы:
   ```bash
   cd /opt/marketpclce/api
   make prod-backup-db       # → backups/marketpclce-YYYY-MM-DD_HHMMSS.sql.gz
   make backup-n8n           # → backups/n8n/n8n-YYYY-MM-DD_HHMMSS.tar.gz
   ```

2. **Перенести бекапы** на новую VDS:
   ```bash
   # с локальной машины:
   scp old-vds:/opt/marketpclce/api/backups/marketpclce-*.sql.gz  /tmp/
   scp old-vds:/opt/marketpclce/api/backups/n8n/n8n-*.tar.gz      /tmp/
   scp /tmp/marketpclce-*.sql.gz  new-vds:/opt/marketpclce/api/backups/
   scp /tmp/n8n-*.tar.gz          new-vds:/opt/marketpclce/api/backups/n8n/
   ```

   Либо напрямую с VDS → VDS если между ними настроен SSH:
   ```bash
   ssh old-vds 'cat /opt/marketpclce/api/backups/marketpclce-*.sql.gz' \
     | ssh new-vds 'cat > /opt/marketpclce/api/backups/marketpclce-restore.sql.gz'
   ```

3. **На новой VDS** — поднять только инфраструктуру (без миграций):
   ```bash
   cd /opt/marketpclce/api
   SKIP_MIGRATE=1 SKIP_BACKUP=1 make deploy
   ```
   Postgres поднимется с **пустой** БД, миграции **не** применяются —
   restore сам создаст таблицы из дампа.

4. **Восстановить Postgres**:
   ```bash
   mkdir -p backups
   cp /opt/marketpclce/api/backups/marketpclce-*.sql.gz backups/
   make prod-restore-db
   # если нужно конкретный файл:
   #   make prod-restore-db BACKUP_FILE=backups/marketpclce-2026-06-04.sql.gz
   ```

5. **Восстановить n8n**:
   ```bash
   mkdir -p backups/n8n
   cp /opt/marketpclce/api/backups/n8n/n8n-*.tar.gz backups/n8n/
   make restore-n8n
   ```

6. **Перезапустить app-сервисы** (чтобы api/worker подхватили данные):
   ```bash
   docker compose -f docker-compose.prod.yml --env-file .env.prod \
     restart api worker
   ```

7. **Переиндексировать OpenSearch** (его в бекапе нет — данные индексирует
   worker из outbox):
   ```bash
   # см. кастомный seed-индексер или принудительный re-emit outbox
   # самое простое: SQL-нагон UPDATE с no-op для триггера переиндексации,
   # либо CLI tool если есть. Если ничего — каталог будет пустой на
   # поиске первые 5–10 секунд после первого UPDATE.
   ```

Проверь:
```bash
make prod-ps                                  # все healthy
curl -fsSL https://<домен>/healthz
curl -fsSL https://<домен>/api/v1/specialists | head -c 200
```

Если в ответе есть данные со старой VDS — миграция прошла.

---

## 6. Первый админ (только сценарий A)

В сценарии B админ перенёсся вместе с дампом. В A — создать вручную:

```bash
# Зарегистрируйся через UI: https://<домен>/registration
# Подтверди email (письмо от noreply@<домен>, через n8n SMTP).
# Затем в Postgres:
docker compose -f docker-compose.prod.yml --env-file .env.prod \
  exec postgres psql -U marketpclce -d marketpclce \
  -c "UPDATE users SET is_admin = TRUE, is_approved = TRUE WHERE email = '<твой email>';"
```

Теперь `https://<домен>/admin` доступен.

---

## 7. n8n (нотификации: Telegram, Email через reg.ru SMTP)

n8n живёт в отдельном контейнере (вне нашего compose'а — управляется
руками или через отдельный compose, см. `docker-compose.n8n.yml` если
есть).

### Поднять n8n

⚠️ Имя сети зависит от названия директории, из которой ты делала
`make deploy`. Стандартно для раскладки `/opt/marketpclce/api/` оно
будет `api_default`. Проверь:
```bash
docker network ls | grep default
```

```bash
docker run -d \
  --name marketplace-api-n8n-1 \
  --network api_default \
  --restart unless-stopped \
  -p 5678:5678 \
  -v marketplace-api_n8n-data:/home/node/.n8n \
  -e N8N_HOST=n8n.<домен> \
  -e N8N_PORT=5678 \
  -e WEBHOOK_URL=https://n8n.<домен>/ \
  -e N8N_PROXY_HOPS=1 \
  n8nio/n8n
```

`--network api_default` (или твой реальный имя сети из `docker network ls`)
— обязательно, иначе Caddy в `web` контейнере не достучится до n8n.
Имя контейнера `marketplace-api-n8n-1` — DNS-имя в этой сети
(см. `Caddyfile`: `reverse_proxy marketplace-api-n8n-1:5678`).

Заведи A-запись `n8n.<домен>` → IP VDS до запуска `make deploy`,
чтобы Caddy выписал TLS на этот поддомен через HTTP-01 challenge.

### Сценарий A — настройка с нуля

1. Открой `https://n8n.<домен>` — экран **Owner Setup**.
2. Заведи email/пароль (минимум 8 символов, 1 заглавная, 1 цифра).
3. **Импортируй версионированные workflows из git** (4 штуки лежат
   в `deploy/n8n/workflows/`):
   ```bash
   cd /opt/marketpclce/api
   make n8n-import
   ```
   Появятся 4 деактивированных workflow:
   - `CRM project events → Telegram` (новые брифы, назначения, споры)
   - `CRM email notifications (verify + reset)` (письма от noreply@)
   - `CRM weekly digest → Telegram` (дайджест в понедельник 9:00)
   - `CRM support → Telegram + Email` (футер /support)
4. **Создай credentials в n8n UI** (левый сайдбар → **Credentials** → `Create
   Credential`; в n8n 2.x они не в Settings, а отдельным пунктом):

   | Имя | Тип | Поля |
   |---|---|---|
   | `Telegram CRM bot` | Telegram API | Access Token от @BotFather |
   | `reg.ru SMTP noreply` | SMTP | Host=`mail.hosting.reg.ru`, Port=`465` (SSL) или `587` (TLS), User=`noreply@wayprmarket.ru`, Password=из reg.ru |
   | `Postgres CRM` | Postgres | Host=`postgres`, Port=`5432`, DB=`marketpclce`, User/Password из `.env.prod` |

   ⚠️ **Привязать credentials к нодам вручную.** `make n8n-import` кладёт
   в workflow ссылку на credential по **имени** (`Telegram CRM bot`), а
   n8n матчит их по **внутреннему id**. У credential'а, который ты только
   что создала, id новый, поэтому workflow жалуется *«Credential not
   configured»* при попытке активации. Нужно открыть каждую такую ноду и
   выбрать credential из выпадашки `Credential to connect with` → **Save**.
   Привязка сохраняется в БД n8n.

   Список нод, которые надо перепривязать:

   | Workflow | Нода | Credential |
   |---|---|---|
   | crmTgEventsV1 | Telegram | Telegram CRM bot |
   | crmSupport | Telegram | Telegram CRM bot |
   | crmSupport | Email → info@ | reg.ru SMTP noreply |
   | crmEmailNotify | Send via reg.ru SMTP | reg.ru SMTP noreply |
   | crmWeeklyDigest | Telegram | Telegram CRM bot |
   | crmWeeklyDigest | Postgres stats | Postgres CRM |

   После повторного `make n8n-import` (например, после изменений
   workflow'а) — связки слетят, и нужно будет перепривязать заново.
   Для disaster-recovery без этого ручного шага — использовать
   `make restore-n8n` из tarball'а (там credentials с теми же id уже
   есть, n8n восстановит связки автоматически).
5. Скопируй **Production URL** Webhook-нод и вставь в `.env.prod`:
   ```
   N8N_WEBHOOK_URL=https://n8n.<домен>/webhook/project-events
   N8N_EMAIL_WEBHOOK_URL=https://n8n.<домен>/webhook/email-events
   N8N_SUPPORT_WEBHOOK_URL=https://n8n.<домен>/webhook/support-events
   ```
6. `make deploy` → перезапустит worker с включёнными диспатчерами.
   В логах должно быть три строки `n8n ... webhook ready`.
7. **Активируй каждый workflow** в n8n (тумблер `Active` справа сверху).
8. Проверь email:
   - `mail.hosting.reg.ru` MX-запись на твой домен (через reg.ru cabinet)
   - SPF/DKIM на домен для noreply@wayprmarket.ru — иначе письма
     уедут в спам у клиентов.

### Изменение workflows (dev → git → prod)

1. Открой n8n локально (`http://localhost:5678` или `http://n8n.<домен>`),
   правь workflow в UI, **Save**.
2. `make n8n-export` — выгрузит изменения в `deploy/n8n/workflows/`
   (без credentials и runtime-метаданных).
3. `git diff` → закоммитить.
4. На VDS: `git pull && make n8n-import` → подтянет новую версию.

### Сценарий B — restore

```bash
make restore-n8n
```

→ восстановит все workflows + credentials + encryption key + аккаунт
owner'a со старой VDS. Login/пароль те же. URL webhook'а уже зашит в
`N8N_WEBHOOK_URL`, ничего настраивать не надо.

---

## 8. Cron бекапов

```bash
crontab -e
```

Добавить:

```cron
# Postgres — ежесуточно в 03:00
0 3 * * * cd /opt/marketpclce/api && /usr/bin/make prod-backup-db >> /var/log/marketpclce-backup.log 2>&1
# n8n — ежесуточно в 03:30
30 3 * * * cd /opt/marketpclce/api && /usr/bin/make backup-n8n >> /var/log/marketpclce-backup.log 2>&1
```

Хранение — `BACKUP_KEEP_DAYS=3` (см. Makefile). Удаление старше 3 дней
автоматическое внутри make-таргетов.

Для долгого хранения — добавь sync в S3:
```bash
# Пример (требует aws cli + IAM-юзера на Yandex Cloud):
aws --endpoint-url=https://storage.yandexcloud.net s3 sync \
  /opt/marketpclce/api/backups/ s3://marketpclce-backups/
```

---

## 9. Observability

Если нужны метрики и логи в Grafana Cloud — заполни в `.env.prod`:

```
GRAFANA_CLOUD_PROM_URL=https://prometheus-prod-XX.grafana.net/api/prom/push
GRAFANA_CLOUD_PROM_USER=<numeric>
GRAFANA_CLOUD_PROM_KEY=<api-token>
GRAFANA_CLOUD_LOKI_URL=https://logs-prod-XXX.grafana.net/loki/api/v1/push
GRAFANA_CLOUD_LOKI_USER=<numeric>
GRAFANA_CLOUD_LOKI_KEY=<api-token>
```

`make deploy` сам поднимет `node-exporter` + `alloy`. Если креды пустые
— alloy запустится, но получишь 401 в его логах.

См. `docs/MONITORING.md`.

---

## 10. Дальнейшие деплои

```bash
ssh root@<IP>
cd /opt/marketpclce/api
make deploy        # full: pull + build + migrate + restart
# или
make redeploy      # zero-downtime: graceful restart, Caddy ретраит /api/*
```

Параметры:
- `SKIP_PULL=1` — без `git pull` (CI уже сделал checkout)
- `SKIP_MIGRATE=1` — без goose (например, restore из дампа)
- `SKIP_BACKUP=1` — без pre-migrate backup (опасно — только для безопасных миграций)

### Видео-транскод preview'ев (с 06.2026)

В `Dockerfile` runtime-стейдж теперь устанавливает `ffmpeg` через
`apk add` (см. `docs/VIDEO_TRANSCODING.md` про сам пайплайн). Это
добавляет ~70 МБ к образу. После обновления — обязательно `make deploy`
с rebuild'ом (т.е. без `SKIP_*`), иначе образ старый и worker логирует
`transcode disabled (ffmpeg not available)` при старте.

Конфиг (env, optional):
- `FFMPEG_PATH` — путь к бинарю. Пустой → `exec.LookPath("ffmpeg")`.
- `TRANSCODE_TIMEOUT` — потолок на один transcode (по умолчанию 90s).
- `TRANSCODE_TEMP_DIR` — куда worker кладёт оригиналы/preview во время
  обработки (по умолчанию `/tmp/transcode`, очищается автоматически).

Pipeline идёт async через outbox (`portfolio.video_uploaded`).
Если кластерная нагрузка вырастет — можно поднять CPU на VDS либо
выкатить второй worker (lease-and-release из P3 разводит работу).

---

## Troubleshooting

**Caddy: `failed to obtain certificate`**
DNS A-запись не резолвится на IP сервера или порт 80 закрыт.
`dig <домен> +short` и `ufw status`.

**OpenSearch падает: `max virtual memory areas vm.max_map_count`**
Не выполнил sysctl из шага 2. `sysctl -w vm.max_map_count=262144` и
`make prod-up`.

**Restore Postgres: `role "marketpclce" does not exist`**
В дампе сделан `--no-owner --clean --if-exists`. Если падает на CREATE
ROLE — добавь руками:
```bash
docker compose -f docker-compose.prod.yml --env-file .env.prod \
  exec postgres psql -U postgres -c "CREATE ROLE marketpclce LOGIN PASSWORD '<пароль>';"
```

**`/search` пуст после restore**
OpenSearch в бекап не входит. Нужно переиндексировать. Самое простое
— триггер outbox через `UPDATE users SET updated_at = now()` или
аналог.

**n8n после restore: «Wrong password»**
Сбрось owner'a и заведи заново — credentials останутся, но workflow'ы
с ними нужно будет перевязать руками:
```bash
docker exec marketplace-api-n8n-1 n8n user-management:reset
```

**`make backup-n8n` пустой (< 1KB)**
Volume `marketplace-api_n8n-data` не существует или контейнер первый
раз не стартовал. `docker volume ls | grep n8n`.

**Воркер логирует `transcode disabled (ffmpeg not available)`**
После обновления на версию с транскод-пайплайном нужно ребилднуть
образ — старый alpine без ffmpeg. `make deploy` (без `SKIP_*`) или
`docker compose -f docker-compose.prod.yml --env-file .env.prod build worker`.
Чек: `docker exec marketplace-api-worker-1 ffmpeg -version` должен
вывести `ffmpeg version 6.x.x`.

**Preview не появляется через ~3 минуты после аплоада**
Глянь outbox и preview_status:
```bash
docker exec marketplace-api-postgres-1 psql -U marketpclce -d marketpclce -c "
  SELECT id, preview_status, preview_error, preview_generated_at, updated_at
  FROM portfolio_items
  WHERE preview_status IN ('processing','failed','pending')
  ORDER BY updated_at DESC LIMIT 10;"
```
- `pending` старше 5 минут — outbox-event потерялся или ffmpeg недоступен;
  глянь логи воркера (`docker logs marketplace-api-worker-1 | grep transcode`).
- `failed` — `preview_error` содержит причину (битый файл / неподдерж. кодек / S3 404).
- `processing` старше 15 минут — handler крашится; смотри
  `outbox_dead` метрику или вручную:
  `SELECT * FROM outbox WHERE aggregate='portfolio' AND dead_at IS NOT NULL ORDER BY dead_at DESC;`

**LLM-эндпоинты возвращают 404**
`LLM_API_KEY` пустой — `/search/summarize` и `/clarify` не маунтятся
(by design).

**`web` контейнер падает: `build context ../web not found`**
Нет `/opt/marketpclce/web/`. `git clone ... /opt/marketpclce/web` и
повтори `make deploy`.
