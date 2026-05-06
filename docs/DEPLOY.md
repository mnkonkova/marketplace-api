# Deploy на Timeweb (beta-MVP)

Шпаргалка для разворачивания marketpclce на одной VDS Timeweb с Docker Compose.
Все сервисы (postgres / opensearch / redis / api / worker / caddy) живут на
одной машине; медиа уезжает в Yandex Object Storage по S3.

---

## 1. Заказать VDS

Кабинет Timeweb Cloud → **Cloud Servers** → Создать:

| Параметр | Значение |
|---|---|
| ОС | Ubuntu 22.04 LTS |
| RAM / vCPU | **4 GB / 2 vCPU** (минимум 3 GB) |
| Диск | 60 GB SSD |
| Локация | Москва (ru-1) |

Запиши IP и root-пароль из письма.

> Жёсткий минимум — 3 GB RAM, тогда `OPENSEARCH_JAVA_OPTS=-Xms384m -Xmx384m`
> в `docker-compose.prod.yml`. На 4 GB можно ничего не править.

---

## 2. Подключиться и поставить docker

```bash
ssh root@<IP>
apt update && apt -y upgrade
apt -y install docker.io docker-compose-v2 git make ufw

# Файрвол: только 22, 80, 443
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

systemctl enable --now docker
```

OpenSearch требует `vm.max_map_count ≥ 262144`:
```bash
echo 'vm.max_map_count=262144' >> /etc/sysctl.conf
sysctl -p
```

---

## 3. Поддомен и DNS

В кабинете Timeweb Cloud → **Domains** (или у любого регистратора) добавь
A-запись:

```
app.<твой-поддомен>   A   <IP VDS>   TTL 300
```

Подожди 1–5 минут пока DNS резолвится (`dig app.твой-поддомен +short`).
Это **обязательно** до первого старта Caddy — иначе Let's Encrypt не выдаст
сертификат через HTTP-01 challenge.

---

## 4. Клон и конфиг

```bash
git clone https://github.com/mnkonkova/marketplace-mvp.git /opt/marketpclce
cd /opt/marketpclce

cp .env.prod.example .env.prod
nano .env.prod
```

Обязательные поля в `.env.prod`:

| Поле | Как получить |
|---|---|
| `DOMAIN` | `app.твой-поддомен.timeweb.cloud` |
| `POSTGRES_PASSWORD` | `openssl rand -hex 16` |
| `DATABASE_URL` | впиши тот же пароль |
| `OPENSEARCH_PASSWORD` | `openssl rand -base64 24` (≥ 10 символов, спец-символы) |
| `JWT_SECRET` | `openssl rand -hex 32` |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` / `S3_BUCKET` | Yandex Cloud Console → Service Account → Static access key |
| `LLM_API_KEY` | console.anthropic.com или DeepSeek |

---

## 5. Первый запуск

```bash
make deploy
```

Скрипт:
1. Соберёт docker-image (api/worker/seed/goose в одном).
2. Поднимет postgres / opensearch / redis.
3. Прогонит миграции через goose.
4. Стартанёт api / worker / caddy. Caddy сам выпишет TLS-сертификат от
   Let's Encrypt — это занимает 10–60 секунд.

Проверь:
```bash
make prod-ps                                    # все сервисы healthy
curl -fsSL https://app.твой-поддомен.timeweb.cloud/health
```

Открой `https://app.твой-поддомен.timeweb.cloud` в браузере.

---

## 6. (Опционально) Демо-данные

Чтобы каталог не был пуст:
```bash
make prod-seed
```

Сидер заполнит `users` / `specialist_profiles` / `portfolio_items`. Worker
проиндексирует их в OpenSearch через outbox в течение 5–10 секунд.

---

## 7. Дальнейшие деплои

После пуша в `main`:
```bash
ssh root@<IP>
cd /opt/marketpclce
git pull
make deploy
```

`make deploy` идемпотентен: если в коде ничего не поменялось — ребилд
будет no-op, миграции пропустит уже применённые.

---

## 8. Бэкапы PostgreSQL

Разовый дамп:
```bash
docker compose -f docker-compose.prod.yml --env-file .env.prod \
  exec -T postgres pg_dump -U marketpclce marketpclce \
  | gzip > /var/backups/marketpclce-$(date +%F).sql.gz
```

Cron (ежесуточно в 04:00):
```cron
0 4 * * * cd /opt/marketpclce && docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T postgres pg_dump -U marketpclce marketpclce | gzip > /var/backups/marketpclce-$(date +\%F).sql.gz
```

---

## 9. Полезные команды

```bash
make prod-logs                                              # все сервисы
$(PROD_DC) logs -f api worker                               # только app
$(PROD_DC) restart api worker                               # рестарт без билда
make prod-down                                              # остановить всё
$(PROD_DC) exec postgres psql -U marketpclce marketpclce    # SQL-консоль
$(PROD_DC) exec redis redis-cli                             # redis-cli
$(PROD_DC) exec api wget -qO- http://localhost:8080/health  # healthcheck из контейнера
```

(`PROD_DC = docker compose -f docker-compose.prod.yml --env-file .env.prod`)

---

## Troubleshooting

**Caddy: `failed to obtain certificate`** —
DNS A-запись поддомена не резолвится на IP сервера или порт 80 закрыт.
Проверь `dig <domain> +short` и `ufw status`.

**OpenSearch падает с `max virtual memory areas vm.max_map_count`** —
не выполнил sysctl из шага 2: `sysctl -w vm.max_map_count=262144` и
повторно `make prod-up`.

**API стартует, но `/search` пуст после seed** —
worker должен индексировать outbox. `make prod-logs` → ищи `outbox indexed`.
Если падает — проверь `OPENSEARCH_URL=http://opensearch:9200` в `.env.prod`.

**Out-of-memory при индексации** —
ужми `OPENSEARCH_JAVA_OPTS=-Xms384m -Xmx384m` в `docker-compose.prod.yml`
или возьми VDS с 6 GB RAM.

**LLM-эндпоинты возвращают 404** —
не задан `LLM_API_KEY` — `/search/summarize` и `/clarify` в этом случае не
маунтятся (это by-design, см. `internal/httpapi/router.go`).
