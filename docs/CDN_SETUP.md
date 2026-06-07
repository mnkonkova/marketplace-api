# Yandex Cloud CDN: подключение для Object Storage

Архитектура: CDN сидит **перед** Object Storage как кеш для чтения.
Юзеры качают медиа через edge-кеш, origin (S3) видит только miss'ы.
Presigned PUT'ы из браузера продолжают идти напрямую в Object Storage
через minio-go SDK — CDN write не поддерживает.

Эффект: видео в фиде грузится с ближайшего edge-узла за ~50ms вместо
~200-300ms из ru-central1, исходящий трафик с S3 падает на порядок
(оплата за GB out от Yandex Object Storage снижается соответствующе).

---

## 1. Подготовка Object Storage (origin)

Bucket должен быть **public-read** для CDN — иначе CDN не сможет
вытягивать контент с origin без отдельных подписей. У нас уже так
(сейчас фронт читает напрямую), ничего менять не нужно.

Проверь, что есть `S3_PUBLIC_URL` в `.env.prod` — это адрес bucket'а
вида `https://media.example.com` (CNAME) либо
`https://storage.yandexcloud.net/marketpclce-prod`. Если пусто — задаст
автоматически `${S3_ENDPOINT}/${S3_BUCKET}`, тоже сработает.

---

## 2. Создать CDN resource

1. В консоли Yandex Cloud → **Cloud CDN** → **Create resource**.
2. **Content origin**: `Bucket` → выбери `marketpclce-prod`.
   - Yandex автоматически проставит origin URL.
3. **Primary domain name** (откуда юзеры будут качать):
   - Вариант А (быстрее): использовать дефолтный
     `cdn-marketpclce-XXXXX.edgecdn.ru`.
   - Вариант Б (брендовый): свой CNAME, например
     `media-cdn.example.com`. В DNS поставь CNAME-запись на
     `cl-XXXXX.edgecdn.ru` (Yandex даст конкретное имя в консоли).
     LetsEncrypt-серт для https заведётся автоматически у Yandex после
     валидации DNS.
4. **Cache-Control**: `Use Origin headers` — наш backend ставит
   `public, max-age=31536000, immutable` на Upload(), у фронта при
   presigned PUT мы тоже можем включить. CDN будет кешировать год.
5. **Compression**: включить `Brotli` + `gzip` для html/json — на видео
   не действует, но overhead нулевой.
6. **CORS**: добавить как пасс-туру с origin — для `<video src>` и
   `<img src>` это не нужно, но если фронт когда-то решит делать
   `fetch()` через CDN, без этого упрётся в CORS-stop.

После создания запиши **primary domain** — это `CDN_BASE_URL`.

---

## 3. Включить в backend'е

В `.env.prod`:

```
CDN_BASE_URL=https://media-cdn.example.com
```

После рестарта (`make redeploy` достаточно — миграции не нужны):
- `s3.Client.PublicURL(key)` возвращает CDN URL.
- Все **новые** записи в БД (`portfolio_items.video_url`,
  `portfolio_items.preview_url`, `specialist_profiles.avatar_url`,
  `portfolio_items.thumbnail_url`) сохраняются с CDN URL.
- **Старые** записи продолжают работать — `KeyFromURL` понимает оба
  префикса (CDN и origin), фронт отдаёт URL как есть.

Опционально — backfill старых URL на CDN (необязательно, всё работает
и так через transparent serve):

```sql
-- portfolio_items.video_url
UPDATE portfolio_items SET video_url = REPLACE(video_url,
    'https://storage.yandexcloud.net/marketpclce-prod/',
    'https://media-cdn.example.com/')
  WHERE video_url LIKE 'https://storage.yandexcloud.net/%';

-- то же для preview_url, thumbnail_url, и specialist_profiles.avatar_url
```

После backfill'a OS feed_videos придётся переиндексировать (новые URL
улетят через outbox при следующем upsert'е спеца, либо принудительно
через `UPDATE users SET updated_at = now()` массово).

---

## 4. Проверка

```bash
# 1. Bucket напрямую (origin):
curl -I "https://storage.yandexcloud.net/marketpclce-prod/portfolio/<u>/<v>.mp4"

# 2. Через CDN — должен вернуть 200 + headers со стороны CDN:
curl -I "https://media-cdn.example.com/portfolio/<u>/<v>.mp4"
# Жди header X-Cache: MISS на первом запросе, HIT — на втором.
```

Проверь Cache-Control в ответе CDN. Должен быть наш
`public, max-age=31536000, immutable`.

---

## 5. Мониторинг

Yandex CDN отдаёт встроенные метрики в консоли:
- **Cache hit ratio** — должен быть >80% после прогрева (день-два
  трафика).
- **Bandwidth (egress)** — сравни с прежним bandwidth Object Storage.
  Должен быть значимо ниже (большая часть GB out теперь через CDN).
- **Errors** — 4xx/5xx на CDN side vs origin side.

Из backend-метрик ничего нового не нужно — CDN прозрачен для нашего
кода. Если будут жалобы на «404 из CDN», проверь:
1. Bucket правда public-read?
2. CDN resource ссылается на правильный bucket?
3. Файл точно есть в bucket'е (`curl https://storage.yandexcloud.net/<bucket>/<key>`)?

---

## 6. Invalidation / Purge

Нам **не нужен** — все upload'ы content-addressable (имя ключа =
UUID объекта), при правке всегда уходит новый ключ, старый sweep
подбирает. Cache-Control `immutable` говорит CDN/браузеру кешировать
по максимуму.

Если когда-то понадобится (например, replace аватара с тем же ключом
при патче профиля — у нас этого нет, проверь): Yandex CDN покажет
`Purge` кнопку в консоли + есть API для автоматизации.

---

## 7. Cost-check

| Что | Цена YC (примерно, 06.2026) | Комментарий |
|---|---|---|
| Object Storage egress | 1.5 ₽/GB | До 100 GB free/мес |
| Cloud CDN egress (RU) | 1.0 ₽/GB | Дешевле — это сама фишка |
| CDN: HTTP-запросы | 0.04 ₽ за 1000 | Незаметно при разумной нагрузке |
| Cache-hit ratio 80% → 80% egress идёт через CDN, 20% через S3 | | Бытовое снижение ~30% бюджета |

Подробности и калькулятор — `cloud.yandex.ru/prices?section=cdn`.

---

## 8. Откат

Если что-то пошло не так — пустой `CDN_BASE_URL` в `.env.prod`,
`make redeploy`. Backend моментально переключится на origin URL'ы.
Все старые записи с CDN URL продолжат работать (CDN-домен всё ещё
жив, просто новые записи будут с origin'ом). Без потерь.

Чтобы отключить **полностью** — отключи CDN resource в консоли Yandex,
он перестанет принимать запросы.
