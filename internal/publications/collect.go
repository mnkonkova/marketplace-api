package publications

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/instacurl"
)

// collectSchedule — фоновый обход: через сколько трогать ролик снова.
//
// Это расписание про РАСХОД. Архив почти не меняется, а объём сбора
// растёт линейно с каждым месяцем работы: 60 видео × 5 площадок
// ежедневно — это 300 обходов в сутки на одного креатора, и это только
// первый проект. Поэтому лестница: 0–5 дней ежедневно, 6–14 раз в два
// дня, 15–28 раз в четыре, дальше раз в восемь.
//
// Обновление по заходу живёт НЕ здесь: карточку открыли — цифры
// подтягиваются целиком, см. refreshDebounce. Так свежие цифры стоят
// ровно столько, сколько на них смотрят, а не круглые сутки.
var collectSchedule = []struct {
	upTo  time.Duration // возраст ролика, до которого действует шаг
	every time.Duration // через сколько обходить
}{
	{upTo: 5 * 24 * time.Hour, every: 24 * time.Hour},
	{upTo: 14 * 24 * time.Hour, every: 48 * time.Hour},
	{upTo: 28 * 24 * time.Hour, every: 96 * time.Hour},
}

// collectEveryOldest — шаг для всего, что старше последней границы.
const collectEveryOldest = 192 * time.Hour

// CollectEvery — шаг фонового расписания для ролика такого возраста.
func CollectEvery(age time.Duration) time.Duration {
	for _, step := range collectSchedule {
		if age <= step.upTo {
			return step.every
		}
	}
	return collectEveryOldest
}

// refreshDebounce — на сколько одно открытие карточки «накрывает» ссылку.
//
// Решение владельца от 2 октября: просмотры подтягиваются КАЖДЫЙ раз,
// когда заходят в кабинет. Затухающего расписания свежести (минута,
// пять, десять, полчаса, час, шесть часов по возрасту ролика) здесь
// больше нет: пока человек смотрит на экран, он смотрит на сегодняшнее
// число, а не на то, которое мы сочли достаточно свежим.
//
// Полминуты — не расписание, а защита от двойного счёта ОДНОГО захода:
// на карточке проекта обновление просят два виджета, F5 повторяет
// запрос, вторая вкладка открывает тот же проект. Это одно «зашли», и
// платить за него дважды незачем. Всё, что дольше полуминуты, — уже
// второй заход, и он честно стоит своих кредитов.
//
// Верхнюю границу расхода держит не это, а лимитер ручки /report/refresh
// (шесть раз в минуту, шестьдесят в час на человека) и потолок пачки:
// за один заход в сборщик уезжает не больше двадцати пяти ссылок, самых
// застоявшихся.
const refreshDebounce = 30 * time.Second

// ParkedAt — признак «ссылка снята с обхода»: её next_collect_at.
//
// Ставит его подытог периода (billing.parkPeriodLinks) и схлопывание
// закрытого проекта: числа заморожены срезом, записывать новые
// просмотры некуда, и каждый обход — кредит поставщика впустую.
//
// Любая запись next_collect_at обязана этот признак уважать, иначе
// ссылка воскресает НАВСЕГДА: подытог для её периода уже был и больше
// не повторится, а значит снять её снова будет некому. Возвращает
// ссылки в очередь только переоткрытие периода.
const ParkedAt = "'infinity'::timestamptz"

// keepParked оборачивает новое значение next_collect_at так, чтобы оно
// не трогало уже снятую с обхода ссылку. col — как поле называется в
// этом запросе (с алиасом таблицы или без).
func keepParked(col, expr string) string {
	return "CASE WHEN " + col + " = " + ParkedAt + " THEN " + col + " ELSE " + expr + " END"
}

// collectRetryDelay — на сколько отодвигается вся пачка, когда сервис
// сбора недоступен целиком. Пятнадцать минут: достаточно, чтобы очередь
// прокрутилась, и мало, чтобы не потерять день.
const collectRetryDelay = 15 * time.Minute

// CollectionRetention — сколько ещё собираем после закрытия проекта.
// По истечении ежедневный ряд схлопывается в итоговый снимок.
const CollectionRetention = 28 * 24 * time.Hour

// LinkToCollect — ссылка, которую пора обойти.
type LinkToCollect struct {
	LinkID        uuid.UUID
	PublicationID uuid.UUID
	ProjectID     uuid.UUID
	Platform      string
	URL           string
	SubmittedAt   time.Time
}

// claimLease — на сколько ссылка помечается «взятой в работу». Обход
// пачки занимает секунды, десяти минут хватает с большим запасом; если
// воркер умрёт посреди работы, ссылки вернутся в очередь сами.
const claimLease = 10 * time.Minute

// DueForCollection — ссылки, которые пора собрать, не больше limit за раз.
//
// Ссылки не просто выбираются, а ЗАНИМАЮТСЯ: тем же запросом им
// сдвигается next_collect_at на короткую аренду. Без этого два экземпляра
// воркера (а они сосуществуют несколько секунд при каждом деплое) брали
// бы одни и те же сорок ссылок и слали их в instacurl дважды. Запись
// идемпотентна и данные бы не пострадали — страдал бы счёт за кредиты.
//
// Частоту при этом задаёт ТОЛЬКО next_collect_at: жёсткое правило «один
// ролик — не чаще раза в календарный день» снято (см. тело запроса),
// потому что при нём любой шаг меньше суток был невыразим. От двойного
// учёта защищает первичный ключ video_stat_daily, а не оно.
//
// SKIP LOCKED — тот же приём, что в outbox-воркере: две транзакции не
// столкнутся на одной строке, а просто разберут разные.
func (r *Repo) DueForCollection(ctx context.Context, now time.Time, limit int) ([]LinkToCollect, error) {
	if limit <= 0 {
		limit = 50
	}
	// Суточного правила («один ролик не чаще раза в календарный день»)
	// здесь больше нет: частоту целиком задаёт next_collect_at, а его
	// считает CollectEvery. Пока правило стояло, «обновлять раз в десять
	// минут» было невыразимо в принципе — выборка всё равно отдавала
	// ссылку один раз в сутки, независимо от расписания.
	q := `
WITH claimed AS (
    SELECT l.id
    FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    JOIN projects pr ON pr.id = p.project_id
    WHERE l.next_collect_at <= $1
      AND p.status <> 'cancelled'
      AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)
    ORDER BY l.next_collect_at
    LIMIT $2
    FOR UPDATE OF l SKIP LOCKED
),
taken AS (
    UPDATE publication_links l
    SET next_collect_at = ` + keepParked("l.next_collect_at", "$1::timestamptz + $3::interval") + `
    FROM claimed c
    WHERE l.id = c.id
    RETURNING l.id, l.publication_id, l.platform, l.url_canonical, l.submitted_at
)
SELECT t.id, t.publication_id, p.project_id, t.platform, t.url_canonical, t.submitted_at
FROM taken t
JOIN project_publications p ON p.id = t.publication_id`
	rows, err := r.db.Query(ctx, q, now, limit, claimLease)
	if err != nil {
		return nil, fmt.Errorf("list links due for collection: %w", err)
	}
	defer rows.Close()

	out := make([]LinkToCollect, 0, limit)
	for rows.Next() {
		var l LinkToCollect
		if err := rows.Scan(&l.LinkID, &l.PublicationID, &l.ProjectID,
			&l.Platform, &l.URL, &l.SubmittedAt); err != nil {
			return nil, fmt.Errorf("scan link due: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClaimProjectLinks — ссылки одного проекта под обновление по заходу.
//
// Отдельно от DueForCollection, потому что вопрос другой. Та выбирает по
// очереди: «кому пора по фоновому расписанию, кто первый». Эта берёт
// ВСЁ, что у проекта живое, — человек открыл карточку, и цифры на ней
// должны быть сегодняшними. Отбора по свежести больше нет: отсекается
// только повтор одного и того же захода (refreshDebounce).
//
// Потолок — limit: за раз в сборщик уезжает не больше двадцати пяти
// ссылок, и берутся самые застоявшиеся. У проекта с шестью десятками
// роликов второй заход доберёт остальные.
//
// Взятые ссылки арендуются ровно так же, как в фоновом обходе: их
// next_collect_at уезжает на claimLease вперёд, и воркер, проснувшийся
// в эту секунду, их не возьмёт.
//
// triedBefore — порог «считать ещё не тронутым». Два вызывающих и два
// порога, и это ровно одно решение, вынесенное в параметр:
//
//   - заход в карточку даёт now-refreshDebounce: повтор одного захода
//     не платим, второй заход платим;
//   - форсированный обход на закрытии периода даёт МОМЕНТ НАЧАЛА этого
//     обхода: пачка за пачкой, пока не кончатся ссылки, и ни одна не
//     обходится в нём дважды.
func (r *Repo) ClaimProjectLinks(
	ctx context.Context, projectID uuid.UUID, creatorID *uuid.UUID,
	now, triedBefore time.Time, limit int,
) ([]LinkToCollect, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE ... SKIP LOCKED и аренда в ОДНОЙ транзакции с выборкой.
	//
	// Было: SELECT без блокировки, отбор в Go, отдельный UPDATE. Два
	// одновременных открытия карточки (менеджер и креатор, двойной клик,
	// просто F5 подряд) видели один и тот же набор и оба уходили в
	// сборщик — по 25 платных обращений каждый. Комментарий утверждал,
	// что от этого защищает аренда; он врал: аренда записывалась, но
	// нигде не читалась.
	rows, err := tx.Query(ctx, `
SELECT l.id, l.publication_id, l.platform, l.url_canonical, l.submitted_at,
       l.last_collected_at, l.last_collect_try_at
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
JOIN projects pr ON pr.id = p.project_id
WHERE p.project_id = $1
  -- Креатор обновляет СВОИ ролики, а не весь проект: чужие стоят
  -- кредитов, которых он не тратил, и сдвигают чужое расписание.
  AND ($4::uuid IS NULL OR p.creator_user_id = $4)
  AND p.status <> 'cancelled'
  AND l.next_collect_at <> `+ParkedAt+`
  AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $2)
  -- Что считать «ещё не тронутым» — решает вызывающий, см. triedBefore.
  --
  -- По ПОСЛЕДНЕЙ ПОПЫТКЕ, а не по последней удаче: last_collected_at
  -- пишется только при успехе, и ссылка, по которой сбор не удаётся
  -- никогда (удалённый ролик, площадка без метрик), выглядела бы «ни
  -- разу не собранной» вечно и улетала в сборщик при каждом F5. Дороже
  -- всего обходились бы ровно те ссылки, которые заведомо ничего не
  -- вернут.
  AND (l.last_collect_try_at IS NULL OR l.last_collect_try_at < $5::timestamptz)
-- Порядок — по давности последнего касания, а не по свежести сдачи.
--
-- Было ORDER BY submitted_at DESC: лимит отрезал двадцать пять САМЫХ
-- НОВЫХ ссылок, и уже их отсеивал отбор по свежести. У проекта с
-- тремя десятками роликов старые не обновлялись никогда — до них
-- просто не доходила очередь.
ORDER BY GREATEST(
    COALESCE(l.last_collect_try_at, to_timestamp(0)),
    COALESCE(l.last_collected_at, to_timestamp(0))
), l.submitted_at DESC
LIMIT $3
FOR UPDATE OF l SKIP LOCKED`, projectID, now, limit, creatorID, triedBefore)
	if err != nil {
		return nil, fmt.Errorf("list project links: %w", err)
	}
	stale := make([]LinkToCollect, 0, limit)
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var link LinkToCollect
		// last_collected_at и last_collect_try_at читаются запросом, но
		// в Go больше не нужны: отбор по ним делает сам запрос.
		var collected, tried *time.Time
		if err := rows.Scan(&link.LinkID, &link.PublicationID, &link.Platform,
			&link.URL, &link.SubmittedAt, &collected, &tried); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan project link: %w", err)
		}
		link.ProjectID = projectID
		stale = append(stale, link)
		ids = append(ids, link.LinkID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(stale) == 0 {
		return nil, tx.Commit(ctx)
	}

	// Отметку попытки ставим СРАЗУ, до похода в сборщик: она и есть
	// защита от повтора. Расписание при этом двигаем только вперёд —
	// GREATEST: у ссылки, которой по фону идти через восемь дней, оно
	// иначе сбрасывалось бы на десять минут, и открытие карточки
	// старого проекта затаскивало бы весь архив в ближайшую очередь.
	if _, err := tx.Exec(ctx, `
UPDATE publication_links
SET last_collect_try_at = $2::timestamptz,
    next_collect_at = `+keepParked("next_collect_at",
		"GREATEST(next_collect_at, $2::timestamptz + $3::interval)")+`
WHERE id = ANY($1)`, ids, now, claimLease); err != nil {
		return nil, fmt.Errorf("lease project links: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return stale, nil
}

// SaveStats — снимок метрик на дату плюс перенос ссылки на следующий обход.
//
// Повторный вызов за ту же дату перезаписывает снимок, а не плодит строки:
// PK (link_id, stat_date). Пропущенный день не искажает историю — просто
// не будет строки за этот день.
func (r *Repo) SaveStats(ctx context.Context, link LinkToCollect,
	views, likes, comments, shares *int64, publishedAt *time.Time, now time.Time) error {

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// shares пишем как есть, включая NULL: «площадка не отдала репосты» и
	// «репостов ноль» — разные ответы, и подставлять ноль вместо
	// неизвестного значит занижать вовлечённость молча.
	if _, err := tx.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments, shares, collected_at)
VALUES ($1, $2::date, $3, $4, $5, $6, $7)
ON CONFLICT (link_id, stat_date) DO UPDATE
SET views = EXCLUDED.views,
    likes = EXCLUDED.likes,
    comments = EXCLUDED.comments,
    shares = EXCLUDED.shares,
    collected_at = EXCLUDED.collected_at`,
		link.LinkID, now, views, likes, comments, shares, now); err != nil {
		return fmt.Errorf("upsert daily stat: %w", err)
	}

	every := CollectEvery(now.Sub(link.SubmittedAt))
	if _, err := tx.Exec(ctx, `
UPDATE publication_links
SET last_collected_at = $2::timestamptz,
    last_collect_try_at = $2::timestamptz,
    -- Удачный обход стирает прошлую причину: она была про то, чего
    -- больше нет, а в кабинете висела бы подписью под живой цифрой.
    last_collect_error = '',
    collect_every = $3,
    -- Дата публикации пишется один раз и больше не перезаписывается:
    -- ролик не выходит дважды, а источник со временем начинает врать
    -- (меняет часовой пояс, отдаёт дату перезалива). Первое непустое
    -- значение и есть ответ; COALESCE тут именно про это, а не про
    -- "подставить что-нибудь".
    published_at = COALESCE(published_at, $4),
    -- Приведение ::timestamptz обязательно: без него Postgres не может
    -- выбрать оператор "+" (кандидатов несколько: date, time, timestamp)
    -- и выводит для параметра противоречивые типы — 42P08.
    next_collect_at = `+keepParked("next_collect_at", "$2::timestamptz + $3::interval")+`
WHERE id = $1`, link.LinkID, now, every, publishedAt); err != nil {
		return fmt.Errorf("reschedule link: %w", err)
	}
	return tx.Commit(ctx)
}

// publishedAtFormats — в каком виде источник отдаёт дату публикации.
//
// Он присылает строку, и какой именно формат — известно только по живым
// данным: у каждой площадки своё, а сборщик отдаёт то, что нашёл. Поэтому
// список, а не один формат, и молчаливый отказ вместо ошибки: мусор в
// этом поле не должен ломать сбор метрик, ради которого мы и ходили.
var publishedAtFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parsePublishedAt — дата публикации из ответа сборщика. nil, если поля
// нет или оно не разбирается: «не знаем» честнее выдуманной даты, и
// колонка для того и nullable.
func parsePublishedAt(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Секунды эпохи — тоже встречающийся вид. Проверяем до форматов:
	// time.Parse на числе всё равно не сработает.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		// Отсекаем заведомую чушь: до 2000 года площадок не было, а
		// миллисекунды легко принять за секунды и уехать в 56-й век.
		if n > 946_684_800 && n < 4_102_444_800 {
			t := time.Unix(n, 0).UTC()
			return &t
		}
		return nil
	}
	for _, layout := range publishedAtFormats {
		if t, err := time.Parse(layout, raw); err == nil {
			t = t.UTC()
			// Тот же фильтр здравого смысла: нулевая дата и будущее
			// говорят о том, что разобрали не то.
			if t.Year() < 2000 || t.After(time.Now().AddDate(1, 0, 0)) {
				return nil
			}
			return &t
		}
	}
	return nil
}

// maxCollectError — сколько текста причины храним.
//
// Триста символов: причина — это строка для человека («метрики
// отдельных постов для vk пока не поддержаны»), а не стектрейс. Чужой
// сервис однажды пришлёт килобайт HTML вместо сообщения, и место под
// него в каждой строке таблицы ссылок нам не нужно.
const maxCollectError = 300

// MarkFailed — сервис не отдал метрики (площадка не поддержана, ролик
// удалён, кончились кредиты у поставщика).
//
// Снимок не пишем — нулей в отчёте быть не должно, они врут сильнее, чем
// пропуск. Но следующий обход всё равно отодвигаем: иначе битая ссылка
// будет дёргаться на каждом тике и съест слоты у живых.
//
// А причину СОХРАНЯЕМ. Раньше она тут терялась, и в кабинете «площадка
// не собирается», «ролик удалён» и «никто не посмотрел» выглядели
// одинаково — пустой цифрой. Первые два — наша работа, третье — работа
// креатора, и путать их нельзя.
func (r *Repo) MarkFailed(ctx context.Context, link LinkToCollect, now time.Time, reason string) error {
	every := CollectEvery(now.Sub(link.SubmittedAt))
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "сборщик не ответил по этой ссылке"
	}
	// По РУНАМ, а не по байтам: кириллица двухбайтовая, и срез по байту
	// разрубает букву пополам. Postgres такую строку не принимает
	// («invalid byte sequence for encoding UTF8»), MarkFailed возвращает
	// ошибку, а сбор на ней обрывает всю пачку — остальные ссылки
	// остаются без снимков и через десять минут оплачиваются заново.
	if utf8.RuneCountInString(reason) > maxCollectError {
		reason = string([]rune(reason)[:maxCollectError])
	}
	_, err := r.db.Exec(ctx, `
UPDATE publication_links
SET collect_every = $2,
    last_collect_error = $4,
    last_collect_try_at = $3::timestamptz,
    next_collect_at = `+keepParked("next_collect_at", "$3::timestamptz + $2::interval")+`
WHERE id = $1`, link.LinkID, every, now, reason)
	if err != nil {
		return fmt.Errorf("reschedule failed link: %w", err)
	}
	return nil
}

// StopCollectionAfter — проект закрыт: дособираем ещё retention и на этом
// всё. Ставится в момент закрытия проекта.
func (r *Repo) StopCollectionAfter(ctx context.Context, projectID uuid.UUID, closedAt time.Time, retention time.Duration) error {
	_, err := r.db.Exec(ctx,
		`UPDATE projects SET collection_stops_at = $2 WHERE id = $1`,
		projectID, closedAt.Add(retention))
	if err != nil {
		return fmt.Errorf("set collection stop: %w", err)
	}
	return nil
}

// CollapseFinished — по проектам, у которых срок сбора вышел, свернуть
// ежедневный ряд в итоговый снимок и удалить детальные строки.
//
// ВНИМАНИЕ: удаление необратимо, восстановить историю по дням будет
// неоткуда. Поэтому снимок пишется ПЕРВЫМ и в той же транзакции: если
// запись снимка упадёт, удаления не произойдёт.
//
// Итог считается по последнему снимку каждой ссылки — просмотры
// накопительные, поэтому суммировать дни было бы кратной ошибкой.
func (r *Repo) CollapseFinished(ctx context.Context, now time.Time) (int, error) {
	rows, err := r.db.Query(ctx, `
SELECT id FROM projects
WHERE collection_stops_at IS NOT NULL
  AND collection_stops_at <= $1
  AND NOT EXISTS (SELECT 1 FROM project_stat_summary s WHERE s.project_id = projects.id)
  -- Схлопывать можно только то, что уже подытожено: период
  -- подытоживается срезом просмотров по каждой площадке, и снимать его
  -- будет неоткуда, если ежедневный ряд удалить раньше.
  --
  -- Условие ставим на РОЛИКИ, а не на периоды: пока есть вышедший
  -- ролик, который не накрыт ни одним подытоженным периодом, историю
  -- держим. Проверять «нет открытых периодов» нельзя (текущий открыт
  -- всегда, ряд не схлопнулся бы никогда), а проверять только
  -- заведённые периоды мало: у проекта, которого фоновая задача ещё не
  -- касалась, их нет вовсе.
  --
  -- Схлопывание не отменяется, а ждёт: периоды закрытого проекта уже в
  -- прошлом, и подытог доберётся до них в ближайшие часы.
  AND NOT EXISTS (
      SELECT 1
      FROM project_publications p
      WHERE p.project_id = projects.id AND p.status <> 'cancelled'
        AND (SELECT MIN(COALESCE(l.published_at, l.submitted_at))::date
             FROM publication_links l WHERE l.publication_id = p.id) IS NOT NULL
        AND NOT EXISTS (
            SELECT 1 FROM project_periods pp
            WHERE pp.project_id = p.project_id AND pp.status = 'locked'
              AND (SELECT MIN(COALESCE(l2.published_at, l2.submitted_at))::date
                   FROM publication_links l2 WHERE l2.publication_id = p.id)
                  BETWEEN pp.starts_on AND pp.ends_on
        )
  )`, now)
	if err != nil {
		return 0, fmt.Errorf("list projects to collapse: %w", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0, 8)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan project to collapse: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	done := 0
	for _, id := range ids {
		if err := r.collapseOne(ctx, id, now); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

func (r *Repo) collapseOne(ctx context.Context, projectID uuid.UUID, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Снимок — первым. last_per_link берёт последнюю дату по каждой
	// ссылке: просмотры накопительные, сумма по дням завысила бы итог
	// в разы.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_stat_summary
    (project_id, views, likes, comments, videos_count, as_of, collapsed_at)
SELECT $1,
       COALESCE(SUM(v.views), 0),
       COALESCE(SUM(v.likes), 0),
       COALESCE(SUM(v.comments), 0),
       COUNT(DISTINCT p.id),
       $2::date,
       $2
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments
    FROM video_stat_daily d
    WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE
WHERE p.project_id = $1
ON CONFLICT (project_id) DO NOTHING`, projectID, now); err != nil {
		return fmt.Errorf("write project summary: %w", err)
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM video_stat_daily
WHERE link_id IN (
    SELECT l.id FROM publication_links l
    JOIN project_publications p ON p.id = l.publication_id
    WHERE p.project_id = $1
)`, projectID); err != nil {
		return fmt.Errorf("delete daily stats: %w", err)
	}

	// Снимаем с обхода: собирать больше нечего.
	if _, err := tx.Exec(ctx, `
UPDATE publication_links
SET next_collect_at = `+ParkedAt+`
WHERE publication_id IN (SELECT id FROM project_publications WHERE project_id = $1)`,
		projectID); err != nil {
		return fmt.Errorf("park links: %w", err)
	}
	return tx.Commit(ctx)
}

// ProjectStats — сводные цифры проекта для отчёта: либо живые, либо из
// итогового снимка, если ряд уже схлопнут.
type ProjectStats struct {
	ProjectID   uuid.UUID  `json:"project_id"`
	Views       int64      `json:"views"`
	Likes       int64      `json:"likes"`
	Comments    int64      `json:"comments"`
	VideosCount int        `json:"videos_count"`
	AsOf        *time.Time `json:"as_of,omitempty"`
	// Collapsed — цифры из снимка, детальной истории по дням больше нет.
	Collapsed bool `json:"collapsed"`
}

// Stats — итоги проекта. Всегда с датой последнего сбора: без неё
// возникает вопрос «почему цифра не изменилась за час».
func (r *Repo) Stats(ctx context.Context, projectID uuid.UUID) (ProjectStats, error) {
	out := ProjectStats{ProjectID: projectID}

	var (
		views, likes, comments *int64
		videos                 *int
		asOf                   *time.Time
	)
	err := r.db.QueryRow(ctx, `
SELECT views, likes, comments, videos_count, as_of
FROM project_stat_summary WHERE project_id = $1`, projectID).
		Scan(&views, &likes, &comments, &videos, &asOf)
	if err == nil {
		out.Collapsed = true
		out.Views, out.Likes, out.Comments = deref(views), deref(likes), deref(comments)
		if videos != nil {
			out.VideosCount = *videos
		}
		out.AsOf = asOf
		return out, nil
	}
	if err != pgx.ErrNoRows {
		return out, fmt.Errorf("read project summary: %w", err)
	}

	err = r.db.QueryRow(ctx, `
SELECT COALESCE(SUM(v.views), 0), COALESCE(SUM(v.likes), 0),
       COALESCE(SUM(v.comments), 0), COUNT(DISTINCT p.id), MAX(v.collected_at)
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments, collected_at
    FROM video_stat_daily d
    WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE
WHERE p.project_id = $1`, projectID).
		Scan(&out.Views, &out.Likes, &out.Comments, &out.VideosCount, &asOf)
	if err != nil {
		return out, fmt.Errorf("read live stats: %w", err)
	}
	out.AsOf = asOf
	return out, nil
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func ageInDays(from, to time.Time) int {
	d := truncateDay(to).Sub(truncateDay(from))
	days := int(d.Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// Collector — то, что умеет отдать метрики по ссылкам. Интерфейс, а не
// конкретный клиент: сбор — единственное место, где домен зависит от
// чужого сервиса, и в тестах его надо подменять, не поднимая instacurl.
type Collector interface {
	Collect(ctx context.Context, urls []string) ([]instacurl.Result, error)
}

// WithCollector — подключить сбор статистики. Без него RunCollection
// возвращает ErrCollectorNotSet: молча ничего не делать нельзя, иначе
// пустой отчёт выглядит как «роликов не смотрят».
func (s *Service) WithCollector(c Collector) *Service {
	s.collector = c
	return s
}

// CollectStats — что сделал один проход сбора.
type CollectStats struct {
	Considered int `json:"considered"`
	Saved      int `json:"saved"`
	// NoData — сервис ответил, но метрик не дал: площадка не поддержана,
	// ролик удалён, кончились кредиты. Это ожидаемый исход, а не сбой.
	NoData int `json:"no_data"`
	// ThresholdNotified — скольким клиентам ушло «ролик перешагнул порог».
	ThresholdNotified int `json:"threshold_notified"`
	// Projects — проекты, по которым в этом проходе появились НОВЫЕ
	// цифры. По ним надо пересчитать начисления открытого периода:
	// просмотры и есть то, из чего они считаются, и пока пересчёта нет,
	// сумма на экране менеджера отстаёт от собранного. Пусто, когда
	// сохранять было нечего.
	Projects []uuid.UUID `json:"-"`
}

// RunCollection — один проход: взять просроченные ссылки, сходить за
// метриками, записать снимки.
//
// Одной пачкой, а не по ссылке: instacurl держит два параллельных слота,
// и дробить запрос на сорок мелких — верный способ упереться в его же
// rate limit.
func (s *Service) RunCollection(ctx context.Context, now time.Time, batchSize int) (CollectStats, error) {
	var st CollectStats
	if s.collector == nil {
		return st, ErrCollectorNotSet
	}

	links, err := s.repo.DueForCollection(ctx, now, batchSize)
	if err != nil {
		return st, err
	}
	if len(links) == 0 {
		return st, nil
	}
	// Пачка заполнилась целиком — значит, очередь длиннее нашей
	// пропускной способности. Считаем это отдельно: отставание ссылок
	// (crm_links_stale) загорится только через сутки, когда цифры в
	// отчёте уже протухнут.
	if len(links) >= batchSize {
		ObserveCollectSaturated()
	}
	return s.collectClaimed(ctx, links, now)
}

// RefreshProject — обойти ссылки ОДНОГО проекта, которым пора.
//
// Зовётся при открытии карточки проекта менеджером или креатором:
// человек смотрит на цифры, и цифры на это время становятся живыми.
// Берутся ВСЕ живые ссылки проекта, самые застоявшиеся первыми, —
// отбора по свежести больше нет. Отсекается только повтор одного и того
// же захода, см. refreshDebounce.
//
// creatorID — чьи ролики обновляем. nil означает «весь проект» и
// годится только менеджеру: креатор не должен тратить кредиты на чужие
// ролики и двигать чужое расписание.
//
// Фоновый обход при этом идёт своим чередом: от двойной работы защищает
// отметка попытки, которая ставится в момент взятия, и SKIP LOCKED.
func (s *Service) RefreshProject(
	ctx context.Context, projectID uuid.UUID, creatorID *uuid.UUID, now time.Time, limit int,
) (CollectStats, error) {
	var st CollectStats
	if s.collector == nil {
		return st, ErrCollectorNotSet
	}
	links, err := s.repo.ClaimProjectLinks(
		ctx, projectID, creatorID, now, now.Add(-refreshDebounce), limit)
	if err != nil {
		return st, err
	}
	if len(links) == 0 {
		return st, nil
	}
	return s.collectClaimed(ctx, links, now)
}

// collectClaimed — общий путь для фонового обхода и обхода по заходу в
// карточку: сходить в сервис, разложить ответ, записать снимки.
//
// Общий намеренно: расписание у этих двух путей разное, а всё, что
// после выборки, обязано быть одинаковым. Две копии разошлись бы на
// первой же правке — например, на том, писать ли снимок, когда сервис
// вернул «нет данных» (не писать: ноль врёт сильнее пропуска).
func (s *Service) collectClaimed(ctx context.Context, links []LinkToCollect, now time.Time) (CollectStats, error) {
	var st CollectStats
	st.Considered = len(links)

	// Один канонический адрес может принадлежать нескольким ссылкам: один
	// ролик сдали двое, или один креатор в двух выкладках. Список на
	// отправку дедуплицируем (лишний поход стоит кредит), а обратное
	// соответствие держим списком — иначе все ссылки, кроме последней,
	// никогда не получали бы статистику.
	urls := make([]string, 0, len(links))
	byURL := make(map[string][]LinkToCollect, len(links))
	for _, l := range links {
		if _, dup := byURL[l.URL]; !dup {
			urls = append(urls, l.URL)
		}
		byURL[l.URL] = append(byURL[l.URL], l)
	}

	results, err := s.collector.Collect(ctx, urls)
	if err != nil {
		ObserveCollectError(collectErrorReason(err))
		// Пачку отодвигаем на короткий backoff, не трогая интервал.
		// Наказывать ссылки за чужую недоступность неправильно, но и
		// оставлять их первыми в очереди нельзя: выборка идёт по
		// next_collect_at ASC, и без сдвига следующий тик возьмёт те же
		// сорок ссылок, снова упрётся в таймаут — сбор встанет навсегда
		// и молча.
		if derr := s.repo.DeferLinks(ctx, links, now.Add(collectRetryDelay)); derr != nil {
			return st, fmt.Errorf("collect: %w (и отложить не удалось: %v)", err, derr)
		}
		// Сервис недоступен целиком — расписание не двигаем, попробуем
		// на следующем тике. Двигать его здесь значило бы наказать
		// ссылки за чужую недоступность.
		return st, fmt.Errorf("collect: %w", err)
	}

	seen := make(map[uuid.UUID]bool, len(links))
	// Проекты, которым цифры реально записали: пересчитывать нечего там,
	// где сбор ничего не принёс.
	touched := make(map[uuid.UUID]bool, len(links))
	for _, res := range results {
		matched, ok := byURL[res.URL]
		if !ok {
			// Сервис вернул то, чего мы не просили. Игнорируем: писать
			// метрики в чужую ссылку хуже, чем не писать вовсе.
			continue
		}
		m, hasData := res.Metrics()
		for _, link := range matched {
			seen[link.LinkID] = true

			if !hasData {
				st.NoData++
				ObserveCollect(link.Platform, "no_data")
				if err := s.repo.MarkFailed(ctx, link, now, res.Error); err != nil {
					return st, err
				}
				continue
			}
			if err := s.repo.SaveStats(ctx, link, m.Views, m.Likes, m.Comments, m.Shares,
				parsePublishedAt(m.PublishedAt), now); err != nil {
				return st, err
			}
			// Полнота, а не только успех: сервис мог вернуть лайки и
			// комментарии, но не просмотры — так бывает, когда меняется
			// вёрстка площадки и разбор одного поля отваливается.
			// Формально сбор удался, фактически в отчёте по этой площадке
			// будут нули.
			if m.Views == nil {
				ObserveCollect(link.Platform, "no_views")
			} else {
				ObserveCollect(link.Platform, "ok")
			}
			st.Saved++
			touched[link.ProjectID] = true
		}
	}

	// Порог просмотров проверяем по выкладке, а не по ссылке: клиент
	// задаёт порог для ролика, а ролик — это пять площадок вместе.
	notified := make(map[uuid.UUID]bool, len(links))
	for _, l := range links {
		if !seen[l.LinkID] || notified[l.PublicationID] {
			continue
		}
		notified[l.PublicationID] = true
		// Ошибку уведомления намеренно не поднимаем выше: цифры уже
		// сохранены, и терять их из-за неотправленного письма нельзя.
		// Проблемы с очередью видны по outbox_* и своим алертам.
		if sent, err := s.repo.NotifyThresholdCrossed(ctx, l.ProjectID, l.PublicationID, now); err == nil && sent {
			st.ThresholdNotified++
		}
	}

	// По чему сервис не ответил вовсе — тоже отодвигаем, иначе эти ссылки
	// будут дёргаться на каждом тике и займут слоты у живых.
	for _, l := range links {
		if !seen[l.LinkID] {
			st.NoData++
			ObserveCollect(l.Platform, "no_data")
			// Про эту ссылку сервис не сказал вообще ничего — ни цифр, ни
			// причины. Так и пишем: иначе пустая причина читалась бы как
			// «обход не проводился».
			if err := s.repo.MarkFailed(ctx, l, now, "сборщик не вернул ответ по этой ссылке"); err != nil {
				return st, err
			}
		}
	}
	for id := range touched {
		st.Projects = append(st.Projects, id)
	}
	return st, nil
}

// RunCollapse — свернуть ряды по проектам, у которых срок сбора вышел.
func (s *Service) RunCollapse(ctx context.Context, now time.Time) (int, error) {
	return s.repo.CollapseFinished(ctx, now)
}

// Stats — итоги проекта для отчёта.
func (s *Service) Stats(ctx context.Context, projectID uuid.UUID) (ProjectStats, error) {
	return s.repo.Stats(ctx, projectID)
}

// collectErrorReason — во что превращается ошибка вызова для метрики.
// Разные причины чинятся по-разному: недоступность — это инфраструктура,
// отказ по ключу — конфигурация, лимит — наш собственный график сбора.
func collectErrorReason(err error) string {
	switch {
	case errors.Is(err, instacurl.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, instacurl.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, instacurl.ErrNotConfigured):
		return "not_configured"
	default:
		return "unreachable"
	}
}

// DeferLinks — отодвинуть обход пачки ссылок, не трогая их интервал.
//
// Нужен, когда сервис сбора недоступен целиком: интервал остаётся
// прежним (ролик не постарел), а очередь получает возможность
// прокрутиться дальше.
func (r *Repo) DeferLinks(ctx context.Context, links []LinkToCollect, until time.Time) error {
	if len(links) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.LinkID)
	}
	_, err := r.db.Exec(ctx,
		`UPDATE publication_links SET next_collect_at = `+keepParked("next_collect_at", "$2")+` WHERE id = ANY($1)`, ids, until)
	if err != nil {
		return fmt.Errorf("defer links: %w", err)
	}
	return nil
}

// ---- закрытие периода ----
//
// Форсированный обход перед срезом: решение владельца от 2 октября —
// «1 раз форсированно, когда заканчивается период».
//
// Нужен потому, что обычный обход идёт по заходу в кабинет. Если в
// последний день периода карточку никто не открыл, срез снялся бы с
// позавчерашних цифр — и по ним выставили бы счёт. Это единственный
// проход, который не зависит от того, смотрит ли кто-то на экран.
//
// Вторая половина того решения — «после выставления счёта эти видео
// больше не просматривать» — уже сделана и живёт там, где знают про
// периоды: billing.parkPeriodLinks снимает ссылки с обхода в той же
// транзакции, что и срез, а resumePeriodLinks возвращает их при
// переоткрытии. Второй такой же код здесь означал бы два ответа на
// вопрос «что считается роликом периода».

// forceRefreshBatch — сколько ссылок за одну пачку форсированного
// обхода. То же, что потолок пачки по заходу: instacurl держит два
// параллельных слота и шестьдесят запросов в минуту, и пачка крупнее
// упирается в его же лимит, а не ускоряет проход.
const forceRefreshBatch = 25

// forceRefreshBatches — потолок пачек за один проход.
//
// Защита от бесконечного цикла, а не ограничение продукта: пачка за
// пачкой идут, пока ссылки не кончатся, и в норме проход заканчивается
// сам. Двести пачек — это пять тысяч ссылок, то есть больше, чем бывает
// у проекта за год; если упёрлись в потолок, остальное доберёт следующее
// открытие карточки.
const forceRefreshBatches = 200

// ForceRefreshProject — обойти ВСЕ живые ссылки проекта, не спрашивая о
// свежести.
//
// Зовётся на подытоге периода, до снятия среза. Пачка за пачкой: порог
// «ещё не тронут» — момент начала прохода, поэтому обойдённое в этом же
// проходе второй раз не берётся, а остановка — по пустой пачке.
func (s *Service) ForceRefreshProject(
	ctx context.Context, projectID uuid.UUID, now time.Time,
) (CollectStats, error) {
	var total CollectStats
	if s.collector == nil {
		return total, ErrCollectorNotSet
	}
	// Один клок на штамп и на порог. Порог — момент начала прохода, а
	// штамп ставится этим же временем: пачка, которую только что взяли,
	// на следующем круге уже не проходит условие «тронут раньше начала».
	// Возьми их из разных источников (аргумент now от вызывающего и
	// time.Now() внутри) — и пачка возвращала бы одно и то же, пока не
	// упрётся в потолок пачек: двести платных походов за одну ссылку.
	start := time.Now().UTC()
	for i := 0; i < forceRefreshBatches; i++ {
		links, err := s.repo.ClaimProjectLinks(
			ctx, projectID, nil, start, start, forceRefreshBatch)
		if err != nil {
			return total, err
		}
		if len(links) == 0 {
			return total, nil
		}
		st, err := s.collectClaimed(ctx, links, now)
		total.Considered += st.Considered
		total.Saved += st.Saved
		total.NoData += st.NoData
		total.ThresholdNotified += st.ThresholdNotified
		total.Projects = append(total.Projects, st.Projects...)
		if err != nil {
			return total, err
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
	return total, nil
}
