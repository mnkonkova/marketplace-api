# Видео-транскодинг preview'ев (design doc)

**Статус:** не реализовано, спека на будущее.
**Контекст:** портфолио спецов — сейчас в `portfolio_items.video_url`
лежит сырое видео из multipart-upload'а (типично 5-50 МБ, H.264 720p+).
Фид (`/feed`) и карточка спеца показывают это видео как preview —
autoplay на mute, loop, no controls. Браузер качает весь файл, мобильный
трафик и first-paint страдают.

**Цель:** генерировать рядом с оригиналом маленький preview (480p,
H.264, 5-10 сек loop, ~500KB), который фид показывает вместо полной
версии. Полную крутим только когда юзер кликнул «развернуть».

---

## 1. Нефункциональные требования

- Транскодинг **асинхронный**: аплоад не ждёт. Спец залил видео →
  получил 201 → preview прилетает в течение 1-3 минут отдельным событием.
- На время отсутствия preview'а фид показывает оригинал
  (degraded mode, не запрещает фид).
- Идемпотентно: повторное событие на тот же `portfolio_item_id` не
  ломает существующий preview, перезаписывает атомарно.
- Cost-conscious: FFmpeg в собственном worker'е, без облачного
  Mux/Cloudinary. Один воркер с CPU обработает ~50-100 видео/час.
- Защита от рекурсии: preview-видео не должно само инициировать
  ещё одно событие transcode.

---

## 2. Архитектура

### Event flow

```
[POST /me/portfolio]                  [worker]
  │                                      │
  ├─→ insert portfolio_items             │
  ├─→ emit outbox:                       │
  │     aggregate=portfolio              │
  │     event=portfolio.video_uploaded   │
  │     payload={ item_id, s3_key }      │
  └─→ 201 client                         │
                                         │
                                         ↓ poll outbox (P3 lease-and-release)
                                  download s3://<key>
                                  ffmpeg → /tmp/<id>_preview.mp4
                                  upload s3://portfolio/<user>/<id>_preview.mp4
                                  UPDATE portfolio_items
                                      SET preview_url = $1,
                                          preview_status = 'ready',
                                          preview_generated_at = now()
                                      WHERE id = $2
                                  emit outbox:
                                      aggregate=specialist
                                      event=specialist.upserted
                                      (для переиндексации в OS feed_videos)
```

### S3 layout

```
portfolio/
  <user_uuid>/
    <item_uuid>.mp4              # оригинал, как сейчас
    <item_uuid>_preview.mp4      # 480p, ~500KB, H.264 baseline для совместимости
    <item_uuid>_thumb.jpg        # (опц., вторая итерация) первый кадр для poster=
```

`_preview.mp4` и `_thumb.jpg` пишет ИСКЛЮЧИТЕЛЬНО worker. Spec/API
никогда не выписывают presigned PUT под суффиксы `_preview` / `_thumb` —
`assertOwnedKey` (см. data-sec D3) этого и не позволит, потому что фронт
просит ключ только под `<item_uuid>.mp4`.

### DB schema (миграция `00021_portfolio_preview.sql`)

```sql
-- +goose Up
ALTER TABLE portfolio_items
    ADD COLUMN preview_url TEXT,
    ADD COLUMN preview_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (preview_status IN ('pending', 'processing', 'ready', 'failed')),
    ADD COLUMN preview_error TEXT,
    ADD COLUMN preview_generated_at TIMESTAMPTZ;

-- partial index: воркер скан только pending'ов (на случай reconciliation-цикла).
CREATE INDEX portfolio_items_pending_preview_idx
    ON portfolio_items(created_at)
    WHERE preview_status = 'pending';

-- +goose Down
DROP INDEX IF EXISTS portfolio_items_pending_preview_idx;
ALTER TABLE portfolio_items
    DROP COLUMN preview_generated_at,
    DROP COLUMN preview_error,
    DROP COLUMN preview_status,
    DROP COLUMN preview_url;
```

`preview_status`:
- `pending`  — запись создана, preview ещё не сгенерирован
- `processing` — worker взял в работу (lease, P3)
- `ready` — preview готов, `preview_url` заполнен
- `failed` — все попытки исчерпаны, `preview_error` содержит причину

### Outbox event

В `internal/outbox/emit.go`:
```go
AggregatePortfolio = "portfolio"

EventPortfolioVideoUploaded = "portfolio.video_uploaded"
```

Payload:
```go
type PortfolioVideoUploadedPayload struct {
    ItemID  uuid.UUID `json:"item_id"`
    UserID  uuid.UUID `json:"user_id"`
    S3Key   string    `json:"s3_key"`  // portfolio/<user>/<item>.mp4
}
```

---

## 3. FFmpeg pipeline

### Параметры

```bash
ffmpeg -y \
  -ss 00:00:02 \                         # пропустить первые 2 сек (часто заставка)
  -i <input> \
  -t 8 \                                 # длительность — 8 сек
  -vf "scale=-2:480,setsar=1" \          # 480p, сохранить пропорции, even-width
  -c:v libx264 \
  -profile:v baseline -level 3.0 \       # совместимость с Safari iOS
  -preset veryfast \                     # CPU↔file-size компромисс
  -crf 28 \                              # ~500KB на 8 сек 480p (проверить)
  -c:a aac -b:a 64k -ac 1 \              # звук: AAC mono 64k (~50KB / 8 сек)
  -movflags +faststart \                 # MP4 atom в начале для стриминга
  <output>
```

### Целевой размер

- 480p × 8 сек × CRF 28 ≈ 400-700 KB видео.
- AAC mono 64k × 8 сек ≈ 50 KB звука.
- Итого preview ≈ 450-750 KB.
- Для статичных кадров — 250-350 KB.
- Hard cap: `-fs 1500k` если перепрыгивает.

**Про звук**: до 2026-06 preview шёл без аудио (`-an`) — был баг с
параллельным воспроизведением фона upgrade'нутого full при скролле.
После фикса (только active article играет, default muted) аудио
безопасно включено. UGC/showreel-видео без звука = половина впечатления.

### Loop cut логика

«Loop-cut» в постановке = берём 5-10 сек из середины. На первой
итерации просто `-ss 2 -t 8` (offset + duration). Вторая итерация —
эвристика на основе ffprobe:
- если total < 10 сек → используем весь файл (нет места для skip)
- если total > 30 сек → берём с `-ss 5`
- иначе `-ss 2`

Скип секций «не интересно» (detect dark/blank) — оставим на потом
(`-vf "blackdetect=d=0.5"` + `-vf "select=gt(scene\,0.4)"`).

---

## 4. Worker handler

### Размещение

Новый домен `internal/transcode/` (по аналогии с `internal/notifications/`):
- `service.go` — Pipeline struct, метод `Process(ctx, payload)`
- `ffmpeg.go` — обёртка над `exec.Command`, парсинг ошибок, таймаут
- `storage.go` — interface к S3 (download/upload)

Регистрация в `cmd/worker/main.go`:
```go
transcoder := transcode.NewService(transcode.Config{
    FFmpegPath: cfg.FFmpegPath,        // default "/usr/bin/ffmpeg"
    TempDir:    cfg.TranscodeTempDir,   // default "/tmp/transcode"
    Storage:    s3Client,
    Repo:       transcodeRepo,
})

portfolioHandler := func(ctx context.Context, outboxID int64, aggregateID, eventType string, payload []byte) error {
    if eventType != outbox.EventPortfolioVideoUploaded {
        return fmt.Errorf("%w: unknown event %s", outbox.ErrPermanent, eventType)
    }
    return transcoder.Process(ctx, payload)
}

worker := outbox.NewWorker(pool, logger,
    map[string]outbox.Handler{
        outbox.AggregatePortfolio: portfolioHandler,
        // ... остальные
    },
    ...)
```

### Lease + idempotency

- Outbox lease (P3, lockable/scheduled gauge) автоматически защищает
  от двух воркеров на одном `item_id`.
- Идемпотентность: первое действие handler'а — `UPDATE portfolio_items
  SET preview_status='processing' WHERE id=$1 AND preview_status IN
  ('pending','failed')`. Если RowsAffected=0 — значит другой воркер
  уже отметил `ready`/`processing`, возвращаем nil (skip как success).
- На финальном `ready` дополнительно `WHERE preview_status='processing'`
  чтобы не перезаписать ручной revert админа.

### Permanent errors

В `outbox.ErrPermanent` (P4) переводим:
- Файла нет в S3 (404) — оригинал удалили
- FFmpeg вернул "Invalid data found" — битый аплоад
- Размер оригинала > 200 МБ — выходит за лимит multipart'a, что-то не так

Сетевые ошибки и таймауты FFmpeg — транзиентные, ретраимся через backoff.

### Cleanup

`/tmp/transcode/<item_id>.mp4` и `_preview.mp4` всегда удаляются в
`defer`, даже на панике. Disk-leak — главный риск таких pipeline'ов.

---

## 5. Инфра

### Worker Docker image

Базовый образ переходит с `alpine:latest` (нет ffmpeg в пакетах) на
**`linuxserver/ffmpeg`** или собственный multi-stage:

```dockerfile
FROM golang:1.22-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /worker ./cmd/worker

FROM alpine:3.19
RUN apk add --no-cache ffmpeg ca-certificates
COPY --from=build /worker /worker
ENTRYPOINT ["/worker"]
```

Лучше явно зафиксировать версию ffmpeg (`apk add ffmpeg=6.1.1-r0`)
— иначе через год обновление APK сломает CRF kalibrovku.

### Ресурсы

- CPU: ffmpeg veryfast 480p H.264 ≈ 0.5-1× realtime → 8 сек видео = 8-16
  сек CPU.
- RAM: ~200 MB на процесс.
- Диск: до 250 МБ /tmp на одно видео в работе (оригинал + preview).
- Сеть: download original ~30 МБ + upload preview ~0.5 МБ.

VDS на 4 vCPU без проблем обработает ~100 видео/час параллельно через
N горутин (cap в config — `cfg.TranscodeMaxParallel = 2`).

### Метрики

```go
transcodeSuccessTotal   counter{event_type}
transcodeErrorsTotal    counter{event_type, reason}  // permanent / network / timeout
transcodeDurationSeconds histogram{}
transcodeQueueDepth     gauge  // SELECT COUNT(*) WHERE preview_status='pending'
```

Алерт в `grafana/alerts.yml`:
- `TranscodeQueueBacklog`: `transcode_queue_depth > 50 for 30m` — warn
- `TranscodeErrorSpike`: `rate(errors[5m]) > 0.5` — page

---

## 6. Frontend

### API контракт

`portfolio_items` отдаёт два URL'а:
```json
{
  "id": "...",
  "video_url": "https://s3.../portfolio/<u>/<id>.mp4",
  "preview_url": "https://s3.../portfolio/<u>/<id>_preview.mp4",
  "preview_status": "ready",
  ...
}
```

Фронт:
- В фиде/карточке — `<video src=preview_url loop muted autoplay>`.
- При клике «развернуть» — заменяем на `video_url` с controls.
- Если `preview_status != 'ready'` — фолбэк на `video_url` (degraded mode).

Совместимость: `preview_url` опциональный; старые карточки до миграции
покажут оригинал — это OK.

---

## 7. Альтернативы (рассмотрены и отвергнуты)

| Вариант | Почему нет |
|---|---|
| **Mux** (внешний сервис) | $0.005/мин обработки + $0.001/мин стриминга. На 1000 видео/день — $40/мес. Тут хочется in-house контроль. |
| **Cloudinary** | Сильно дороже Mux, плюс трансформации навязывают свой CDN. |
| **HLS-multibitrate (480/720/1080)** | Overkill для preview-стрима. Дойдём до этого если будет VoD-полноценный фид. |
| **Транскод в API-хендлере** | Блокирует /me/portfolio на 10-30 сек. UX через помойку. |
| **Браузерный transcode (ffmpeg.wasm)** | 10-15 МБ WASM, тормозит мобильник, не сэкономит S3-затраты. |
| **AV1/HEVC** | Лучшая компрессия, но Safari iOS только сейчас осваивает; baseline H.264 даёт совместимость 100%. |

---

## 8. Что НЕ делаем сейчас (явно out-of-scope)

- Полноценный VoD-пайплайн (HLS/DASH, multi-bitrate).
- Thumbnail extraction отдельным шагом (можно потом, `-frames:v 1`).
- Аудио-обработка (preview без звука, для autoplay-mute достаточно).
- Watermarking логотипом маркетплейса (политика не решена).
- ML-эвристика выбора «интересного» 8-секундного куска
  (sceneDetect → выберем лучшую секцию). Эвристика смены сцен —
  v2.

---

## 9. План реализации (когда возьмём в работу)

Discrete spurts, чтобы можно было катить инкрементально:

1. **Миграция 00021** + DTO-поля `preview_url`/`preview_status` →
   фронт начинает читать (старые null'ы → fallback на `video_url`).
2. **`internal/transcode/`** скелет: интерфейсы, тесты на ffmpeg
   обёртку с моком (`exec.LookPath("ffmpeg")` → fail-back stub).
3. **Outbox `AggregatePortfolio` + handler** в worker.
4. **Docker image воркера** — multi-stage с ffmpeg.
5. **Emit `portfolio.video_uploaded`** в `AddPortfolioVideo` сервиса.
   До этого момента preview не генерируется — только новые записи.
6. **Backfill-скрипт** `make backfill-previews` — выгребает все
   `preview_status='pending'` и эмитит outbox-события.
7. **Метрики + алерты** в `grafana/alerts.yml`.
8. **Frontend** — переключиться с `video_url` на `preview_url` в фиде.

---

## 10. Тест-план

- Unit: `transcode.Pipeline.Process` с моком ffmpeg/storage.
- Integration: с реальным ffmpeg на тестовом видео (`tests/testdata/sample.mp4`)
  — проверка что preview <1MB, длительность ≤10s, codec h264, resolution 480p.
- Property: повторный handler на тот же item_id не меняет ready-preview.
- Permanent error: corrupt файл → DLQ + `preview_status='failed'`.
- Load: 100 параллельных событий → все обрабатываются в течение 5 минут
  на 4 vCPU.

---

## Связанные

- P3 `lease-and-release` — основа async-pipeline'a (`docs/CRM_V5_BRIEF.md`
  ↔ outbox refactor).
- D3 `assertOwnedKey` — гарантирует что фронт не нарисует свой
  `_preview.mp4`-ключ в обход воркера.
- P4 `ErrPermanent` — путь для битых файлов / удалённых оригиналов.
- P7 `BulkIndex` — переиндексация feed_videos после готовности preview.

---

## 11. Animated WebP «гифки» (v2, после base preview работает)

### Проблема

`_preview.mp4` решает размер (500KB вместо 30MB), но не решает iOS
**Low Power Mode**: при включённой энергоэкономии Safari блокирует
autoplay для ВСЕХ `<video>` тегов, даже muted+playsinline+правильно
аттрибутированных. Юзер на iPhone в LPM видит постер с нативной
иконкой плей, должен тапнуть.

Также: на главной странице у нас 6-8 видео-плиток одновременно
(`hero-mosaic` + `works-grid`) — iOS имеет soft-limit ~4 параллельных
`<video>` элементов с autoplay; лишние не стартуют.

### Решение

Добавить **animated WebP** (или AVIF) рядом с `_preview.mp4`. WebP
анимация:
- Грузится через `<img src="...webp">` — **autoplay работает даже в
  Low Power Mode и без лимитов** (это технически картинка, не видео)
- ~180-280 KB на 4 сек 320p (в 2-3 раза меньше mp4-preview).
  До tune-апа 2026-06 был 240p / ~50-150 KB, но выглядело блёкло на
  больших экранах — подняли разрешение и quality.
- 95%+ браузеров поддерживает (caniuse: 96% global)

Сценарий — **WebP применяем ТОЛЬКО на главной странице**, не везде:

| Место | Тег | Source | Почему |
|---|---|---|---|
| Главная: `hero-mosaic` (4-6 плиток сверху) | `<img>` | `animated_thumb.webp` | autoplay в LPM, маленькие файлы, 6 видео одновременно |
| Главная: `works-grid` (рекомендуемые работы) | `<img>` | `animated_thumb.webp` | то же |
| Feed (`/feed`, Reels-mode) | `<video>` | `preview_url` или `video_url` | юзер на 1 видео в момент, нужен звук + плеер |
| Карточка спеца (`/specialist/:id`) | `<video>` | `preview_url` | engaged-просмотр, аккуратное качество |
| Portfolio-grid внутри спеца | `<video controls>` | `video_url` | юзер кликает чтобы развернуть, autoplay не нужен |

«Гифки» решают проблему массового autoplay на главной (где 6+ плиток).
В feed/cabinet их не нужно — там на экране всегда 1 видео, и юзер
явно сюда пришёл.

### S3 layout (расширенный)

```
portfolio/
  <user_uuid>/
    <item_uuid>.mp4              # оригинал
    <item_uuid>_preview.mp4      # 480p, ~500KB
    <item_uuid>_preview.webp     # 320p × 4-10 сек, ~180-280KB
    <item_uuid>_thumb.jpg        # первый кадр для poster (опц.)
```

### FFmpeg для animated WebP

Длительность гифки **динамическая до 10 сек**. Логика:

| Оригинал | offset (`-ss`) | duration (`-t`) | fps | quality |
|---|---|---|---|---|
| ≤ 5 сек | 0 | весь | 15 | 85 |
| 5-15 сек | 2 | весь − 2 | 15 | 82 |
| > 15 сек | 2 | 10 | 12 | 78 |

Параметры выбираются `ffprobe`-ом ДО transcode-pass'a (мы и так делаем
probe в текущем pipeline). Идея: ~80-150 кадров суммарно при 320p и
quality 78-85 — выглядит как нормальное видео, не блёклое превью.

```bash
ffmpeg -y \
  -ss <OFFSET> \
  -i <input> \
  -t <DURATION> \
  -vf "fps=<FPS>,scale=320:-2:flags=lanczos" \
  -loop 0 \                         # бесконечный loop
  -c:v libwebp \
  -compression_level 6 \
  -quality <Q> \                    # 78-85 баланс размер/качество
  -an \
  -fs 350K \
  <output>.webp
```

Целевой размер: **180-280 KB**. Hard cap `-fs 350K`; если первый pass
дал >300KB — второй pass с `quality -8` (мин 70, не ниже — иначе теряем
смысл tune-апа). Если и это не вошло — оставляем что есть.

**Историческая справка (до 2026-06):** scale=240, fps=10-12, quality=65-75,
cap `-fs 200K`. Размер был 50-150 KB, но на больших экранах гифки
выглядели блёкло и пиксельно. Подняли scale до 320, quality до 78-85,
fps до 12-15 — стало в ~2-2.5× тяжелее (на главной 4 ролика ≈ 1 МБ
вместо 400 КБ), но визуально читается как нормальное видео.

### DB схема (миграция 00022)

```sql
ALTER TABLE portfolio_items
    ADD COLUMN animated_thumb_url TEXT;
-- preview_status общий для обоих формат: ffmpeg делает оба за один
-- handler-call (download оригинала один раз).
```

Поле опциональное: если транскод-handler сгенерил только mp4 (а webp
упал) — animated_thumb_url остаётся NULL, фронт фолбэчит на postеr+mp4.

### Frontend контракт

В hero/works заменить:
```html
<!-- было -->
<video src="{{ preview_url }}" autoplay muted loop playsinline>

<!-- стало (animated WebP, autoplay везде) -->
<img src="{{ animated_thumb_url }}" alt="" loading="lazy">

<!-- если animated_thumb_url пустой — fallback на video как раньше -->
```

В feed-view (полноэкран) остаётся `<video>` — там размер уже не
критичен, и нужен звук + контроль.

### План работ (когда возьмём)

1. Расширить ffmpeg pipeline двумя выходами (mp4 + webp). Sequential —
   сначала mp4, потом webp с нуля (peak RAM не растёт, см. §11.6).
2. Добавить колонку `animated_thumb_url`.
3. Backend: отдавать `animated_thumb_url` в `/feed` и `portfolio_items`.
4. Backfill через `make backfill-previews` (там уже инфраструктура есть).
5. **Frontend ТОЛЬКО на главной** (hero + works grid) поменять
   `<video>` → `<img>`. Feed/specialist/portfolio не трогаем.
6. Добавить метрику `transcode_peak_rss_bytes` чтобы мониторить —
   если хоть один раз > 800MB на 1G лимите, поднять worker до 1.5g.

### OOM-анализ

Транскод **последовательный** (один worker handler за раз,
TranscodeMaxParallel нет). После mp4-pass'a `ffmpeg.go` через
`exec.Cmd.Wait()` ждёт завершения процесса, RAM освобождается.
Webp-pass запускается отдельным `exec.Command` с нуля.

| Pass | Peak RAM | Длительность |
|---|---|---|
| mp4 (480p libx264) | 500-700 MB | 8-15 сек |
| webp (240p libwebp) | 200-300 MB | 4-8 сек |
| **Worker peak** (sequential) | **~500-700 MB** | без изменений |

Worker `mem_limit: 1g` — запас ~30-50%. Если в метриках после релиза
увидим spike'и >800MB — поднять до `1.5g` в docker-compose.prod.yml.

### Альтернативы

| Вариант | Почему нет |
|---|---|
| **Animated GIF** | На порядок больше WebP при том же качестве, без прозрачности нет смысла. |
| **AVIF animated** | Лучше компрессия чем WebP, но поддержка в Safari пока неполная (16.4+), фолбэков больше. Дойдём через год. |
| **APNG** | Безопасный, но 2-3x больше WebP. |
| **MP4 через `<img>` (CSS image-rendering)** | Не работает: `<img>` парсит только image-форматы. |

---

## §12. Известный пробел: внешние mp4 остаются без aspect

**Статус:** не сделано, зафиксировано 2026-08-17.

### Что не работает

Работа портфолио может ссылаться на файл, который специалист хостит сам:
`POST /me/portfolio` принимает готовый `video_url`, а не только результат
загрузки в наш бакет (см. комментарий к `PortfolioCreateInput`). У таких
записей `aspect` не появляется никогда:

- **воркер их не видит.** Событие `portfolio.video_uploaded` эмитится
  только когда `media.KeyFromURL(in.VideoURL) != ""` (`profiles/service.go`),
  то есть для файлов в нашем S3. Нет ключа — нет события — нет ffprobe;
- **`cmd/backfill-aspect` их пропускает** по той же причине: команда
  качает оригинал через `storage.Download(key)`, а ключа нет. В логе это
  видно как `skip: внешний video_url (нет s3-ключа)`.

### Чем это грозит

Публичная страница специалиста рендерит работы в родном формате, и при
пустом `aspect` фронт меряет его сам на `loadedmetadata`. Замер зависит от
чужого хоста: редиректы, отсутствие CORS-независимой отдачи, медленный
ответ, снятый с публикации файл — и формат неизвестен. Фолбэк по умолчанию
вертикальный, поэтому горизонтальный ролик молча показывается вертикальным
(наблюдалось на dev-данных с `samplelib.com`).

### Как чинить

`ffprobe` умеет читать удалённый URL напрямую, скачивать файл целиком не
нужно:

```
ffprobe -v error -select_streams v:0 \
  -show_entries stream=width,height:stream_tags=rotate:stream_side_data=rotation:format=duration \
  -of json "https://example.com/video.mp4"
```

То есть в `backfill-aspect` (и, если решим, в отдельном обработчике для
таких работ) добавляется ветка: нет S3-ключа → пробуем URL напрямую.

**Обязательное условие — фильтр адресов.** Команда ходит по URL, который
ввёл пользователь, с нашей инфраструктуры: это SSRF-поверхность. Нужно
резолвить хост заранее и отбрасывать приватные и служебные диапазоны
(10/8, 172.16/12, 192.168/16, 127/8, 169.254/16 — включая метадата-сервис
облака, ::1, fc00::/7), запрещать редиректы на такие адреса и ставить
жёсткий таймаут. Без этого ручку нельзя запускать даже вручную: URL
приходит извне и может указывать внутрь периметра.

### Оценка

Полдня: ветка в `backfill-aspect` + guard по адресам с юнит-тестами на
диапазоны + прогон на проде. Отдельный вопрос — эмитить ли для внешних
работ событие, чтобы формат подхватывался при добавлении, а не только
разовым прогоном.
