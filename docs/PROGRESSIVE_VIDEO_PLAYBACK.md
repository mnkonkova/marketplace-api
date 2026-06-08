# Progressive Video Enhancement (design doc)

**Статус:** не реализовано, спека на будущее.
**Связанные:** [docs/VIDEO_TRANSCODING.md](./VIDEO_TRANSCODING.md) (генерация
preview backend'ом), `marketplace-web/web/src/widgets/feed-view`,
`marketplace-web/web/src/widgets/portfolio-grid`,
`marketplace-web/web/src/entities/feed/lib/preview.ts`.

**Цель:** карточка видео в фиде/портфолио сначала показывает preview
(480p ~500KB — мгновенный TTFB через CDN), параллельно в фоне грузит
оригинал (720p+ ~10-50MB), и когда тот готов — **бесшовно
переключается** на него с тем же временем воспроизведения.

Юзер получает мгновенный отклик + плавно растущее качество без
ручного выбора и UI-переключателей.

---

## 1. Зачем

Сейчас фронт показывает либо preview (фид, hero, карточки), либо
оригинал (`portfolio-grid` с controls). Между ними — клик «развернуть»
у спеца, статическая замена `<video src>`.

Проблемы:
- **Карточки фида в loop'е** — preview достаточно качественный для
  мини-просмотра, но при долгом просмотре заметно «мыло» 480p
- **Portfolio-grid** — сейчас грузит оригинал сразу, длинный TTFB
  до первого кадра (30-50MB надо хотя бы частично качнуть)
- **Hover-эффект на карточке** — преview уже играет, юзер ждёт
  улучшения, но улучшения нет

Прогрессивное улучшение решает обе боли: быстрый старт + хорошее
качество для тех кто реально смотрит.

---

## 2. Сценарии использования

| Сценарий | preview | full | Когда грузить full |
|---|---|---|---|
| **Hero на главной** (фон-мозаика) | ✅ | ❌ | Не нужен. Декоративный, юзер не смотрит долго |
| **Featured grid на главной** | ✅ | ❌ или ✅ | Загружать full если юзер навёл hover >1s. Бюджет: 8 карточек × 30MB = много, не безусловно |
| **Feed cards в /search и /feed** | ✅ | ✅ (lazy) | Через 2 сек после `loadeddata` preview'a — индикатор интереса |
| **Portfolio grid у спеца** | ✅ | ✅ (eager) | Сразу после монтирования карточки — юзер явно открыл портфолио |
| **Полноэкранный плеер с controls** | ❌ | ✅ | Только оригинал, preview там не нужен |

Главное правило: full грузим **только когда есть вероятность что юзер
реально его посмотрит**. Лезть в карман юзера за бандвидсом — плохо
для мобильного трафика.

---

## 3. Архитектура

### Angular-директива `[appProgressiveVideo]`

```typescript
@Directive({ selector: 'video[appProgressiveVideo]', standalone: true })
export class ProgressiveVideoDirective {
  /** Preview URL (всегда), обычно ~500KB 480p */
  @Input({ required: true }) previewSrc!: string;
  /** Оригинал, грузится в фоне после задержки */
  @Input({ required: true }) fullSrc!: string;
  /** Когда триггерить загрузку full: immediate | onHover | onPlay-2s | manual */
  @Input() upgradeTrigger: UpgradeTrigger = 'onPlay-2s';
  /** Делать ли swap если юзер ушёл с табы / видео не в viewport */
  @Input() pauseBackgroundUpgrade = true;
  
  // Output для аналитики (опц.)
  @Output() qualityUpgraded = new EventEmitter<void>();
}
```

### Использование

```html
<!-- Карточка в feed-view -->
<video
  [src]="video.preview_url"
  appProgressiveVideo
  [previewSrc]="video.preview_url"
  [fullSrc]="video.url"
  upgradeTrigger="onPlay-2s"
  autoplay loop muted playsinline>
</video>
```

Директива берёт на себя:
1. Старт воспроизведения с preview
2. По триггеру создаёт **второй скрытый `<video>`** с full
3. Когда second.readyState ≥ HAVE_FUTURE_DATA — синхронизирует
   `currentTime`, скрытно переключает src на основном `<video>`
   (либо через MSE blob, либо через cross-fade двух элементов)
4. Освобождает память второго элемента

### Сам swap

Два варианта реализации:

#### A. **Двойной `<video>` + cross-fade** (рекомендую)

Простой, кроссбраузерный.

```typescript
// Создаём teaser
const full = document.createElement('video');
full.src = fullSrc;
full.muted = true;
full.preload = 'auto';
full.style.opacity = '0';
container.appendChild(full);

full.addEventListener('canplaythrough', () => {
  full.currentTime = preview.currentTime % full.duration;
  full.play().then(() => {
    // CSS transition: opacity 0 → 1 (300ms)
    full.style.opacity = '1';
    setTimeout(() => preview.remove(), 300);
  });
});
```

**Плюсы:** работает в любом браузере (Safari iOS включительно).
**Минусы:** 2× память на момент перехода, ~300ms «двойного»
элемента в DOM.

#### B. **MediaSource Extensions (MSE) + единый `<video>`**

В единственном `<video>` через `MediaSource` API append'им сначала
сегменты preview, потом сегменты full. Без переключения.

**Плюсы:** один элемент, ноль «вспышек», экономия памяти.
**Минусы:** требует **HLS/DASH-сегментированных** видео, а у нас
просто `.mp4` файлы. Безумно сложнее (нужно делать сегментацию на
бэке через ffmpeg `-f segment`).

→ Берём **вариант A**. MSE — when we go HLS-multibitrate (см.
"out of scope" в `VIDEO_TRANSCODING.md`).

---

## 4. Триггеры загрузки full

| Триггер | Когда срабатывает | Use case |
|---|---|---|
| `immediate` | Параллельно с preview, сразу | Portfolio grid — юзер уже открыл |
| `onPlay-2s` | Через 2 сек после `loadeddata` preview'a | Feed scroll — индикатор реального интереса |
| `onHover` | На `mouseenter` карточки (desktop) | Featured grid |
| `onIntersect-50%` | Когда карточка ≥50% в viewport (IntersectionObserver) | Lazy fallback для тач-устройств |
| `manual` | Только через метод `.upgrade()` | Кастомная логика (например клик «HD») |

Дефолт: `onPlay-2s` — баланс между UX и бандвидсом.

---

## 5. Edge cases

### A. Юзер ушёл со страницы до загрузки full

Директива слушает `visibilitychange` / `pagehide`. Если флаг
`pauseBackgroundUpgrade` (дефолт true) — на скрытии **отменяет
загрузку** через `full.removeAttribute('src'); full.load();`.

### B. Мобильный data-saver

Браузер ставит `connection.saveData = true` через Network Information
API. В этом случае директива **никогда** не апгрейдит на full — preview
forever. Юзер дешёвый трафик экономит.

```typescript
if ((navigator as any).connection?.saveData) {
  return; // skip full
}
```

### C. Медленное соединение

Если `connection.effectiveType` ∈ {'slow-2g', '2g'} → также skip
upgrade. На 3g+ — апгрейдим.

### D. Full так и не загрузился (404, timeout)

`full.onerror` → удаляем второй элемент, продолжаем preview. Никакого
UI-сообщения юзеру — он не должен знать что мы пытались.

### E. Preview ещё не ready, а триггер upgrade сработал

Ждём `preview.loadeddata`, только потом стартуем full. Иначе
синхронизация `currentTime` рандомная.

### F. Multiple карточек в фиде одновременно

В фиде может быть 5-10 одновременно играющих карточек. Если все
триггерят upgrade сразу — 5-10 параллельных загрузок по 30MB.

→ Глобальный **rate-limiter** в `ProgressiveVideoService`: max 2
параллельных upgrade'a. Остальные в очереди.

---

## 6. Метрики (frontend)

Слать на бэк (либо в Grafana через clarify-style endpoint, либо
просто log в console — для начала):

```typescript
{
  event: 'video_quality_upgraded',
  video_id: 'uuid',
  upgrade_trigger: 'onPlay-2s',
  preview_size: 487532,
  full_size: 31528736,
  upgrade_latency_ms: 1840,    // от триггера до canplaythrough
  user_was_watching: true,     // preview сыграл >2s
}
```

Можно увидеть:
- Сколько % видео реально апгрейдились (юзеры смотрят)
- Среднее время апгрейда
- На каких триггерах самая высокая reach-rate

---

## 7. Что НЕ делаем сейчас (out of scope)

- **HLS-multibitrate** с 480p/720p/1080p вариантами. Это серьёзный
  ребилд транскод-пайплайна. Когда дойдём до 1k+ DAU.
- **Adaptive bitrate** на основе bandwidth-измерения в браузере.
  Сейчас просто preview → full (две точки), не нужен ABR.
- **DASH/MSE-based segmentation**. См. вариант B в §3.
- **Pre-cache full в Service Worker** для популярных видео. Можно
  потом если будут метрики говорить «топ-10 видео смотрят
  непропорционально часто».
- **Picture-in-Picture** mode handling. Пока обычный inline player.

---

## 8. План реализации

Discrete spurts, можно катить поэтапно:

1. **`ProgressiveVideoDirective`** в `marketplace-web/web/src/shared/video/`
   с триггером `manual` + `onPlay-2s` (минимальный набор).
2. **Unit-тесты** на ключевые сценарии: success swap, error fallback,
   data-saver skip, cleanup on destroy.
3. **Прикрутить в `feed-view.component`** — autoplay loop карточки в
   `/search` и `/feed`. Trigger: `onPlay-2s`.
4. **Прикрутить в `portfolio-grid.component`** desktop-view. Trigger:
   `immediate` (юзер явно открыл).
5. **Метрики** через `localStorage` log + кнопка debug в dev-режиме
   для измерений.
6. **Подкрутить триггеры** по результатам аналитики (может оказаться
   что `onIntersect-50%` лучше для тач).

---

## 9. Связанные

- **VIDEO_TRANSCODING.md** — preview генерируется бэком, фронт
  получает оба URL'а через `FeedVideo.url + preview_url`.
- **D6** (security): фронт сейчас валидирует что оба URL из нашего
  bucket'а через `KeyFromURL`. С CDN тоже работает — `KeyFromURL`
  распознаёт CDN-домен.
- **CDN** (docs/CDN_SETUP.md): preview и full оба идут через CDN
  кеш — экономия трафика особенно важна когда full грузится в фоне.
