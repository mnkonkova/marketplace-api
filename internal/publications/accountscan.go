package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/instacurl"
)

// Обход аккаунтов креаторов.
//
// Находки до сих пор приносили руками: менеджер увидел ролик у себя в
// ленте и завёл карточку «это ваш ролик?». На этом всё и держалось —
// то есть не держалось: ролик выходит, набирает просмотры, а в плане
// числится несданным, потому что креатор забыл вставить адрес и никто
// этого не заметил.
//
// Площадка знает о ролике в тот же день. Адрес аккаунта у нас есть —
// креатор заводит его сам (project_accounts). Остаётся сходить по этому
// адресу и сравнить, чего в выкладках нет.
//
// Обход ничего не привязывает. Он кладёт находку тем же путём, что и
// менеджер руками, — Service.AddSuggestion, — и дальше отвечает человек.
// Сервис, привязывающий ролики сам, при первой же ошибке сопоставления
// припишет креатору чужую работу, и узнают об этом из счёта.

// ErrAccountScannerNotSet — обход аккаунтов не настроен.
//
// Ошибка, а не тихий no-op, по той же причине, что и у сбора: «обход
// выключен» и «обход сломан» должны различаться. Вызывающий (воркер)
// обязан не поднимать тикер, когда обход выключен конфигом, — тогда до
// этой ошибки дело не доходит вовсе.
var ErrAccountScannerNotSet = errors.New("обход аккаунтов не настроен")

// accountScanLookbackDays — насколько старый ролик ещё может стать
// находкой.
//
// Ровно две ширины окна подсказки (suggestionWindowDays в обе стороны):
// находка полезна только тогда, когда рядом есть незакрытая выкладка, к
// которой её можно предложить. Профиль отдаёт последние десять постов
// независимо от их возраста, и без этого ограничения первый же обход
// завалил бы кабинет карточками про прошлогодний архив — среди которых
// потерялись бы те две, что действительно ждут ответа.
const accountScanLookbackDays = 2 * suggestionWindowDays

// AccountScanner — то, что умеет по адресу профиля вернуть список
// свежих роликов.
//
// Подпись совпадает с Collector, и это не случайность: instacurl
// отвечает на тот же POST /collect, только на адрес аккаунта — тогда в
// ответе kind="profile" и список постов вместо одного. Интерфейс всё
// равно отдельный, потому что отдельно принимается решение «ходить или
// нет»: сбор по сданным ссылкам обязателен (без него отчёт врёт), а
// обход аккаунтов — расход сверх него, по кредиту на аккаунт в сутки,
// и включается он своим ключом.
type AccountScanner interface {
	Collect(ctx context.Context, urls []string) ([]instacurl.Result, error)
}

// WithAccountScanner — включить обход аккаунтов. Без него
// RunAccountScan возвращает ErrAccountScannerNotSet и никуда не ходит.
func (s *Service) WithAccountScanner(sc AccountScanner) *Service {
	s.scanner = sc
	return s
}

// AccountToScan — аккаунт, который пора обойти.
type AccountToScan struct {
	AccountID     uuid.UUID
	ProjectID     uuid.UUID
	CreatorUserID uuid.UUID
	Platform      string
	URL           string
}

// DueForScan — аккаунты, которые пора обойти, не больше limit за раз.
//
// Строки не просто выбираются, а ЗАНИМАЮТСЯ коротким лизом, и по той же
// причине, что в DueForCollection: при деплое два воркера сосуществуют
// несколько секунд, и без лиза оба взяли бы одни и те же аккаунты и
// сходили бы в instacurl дважды. Находки от этого не удвоились бы
// (уникальный индекс), удвоился бы счёт за кредиты.
//
// Суточное правило стоит здесь же, по last_scanned_at, и проверяется ДО
// похода в сервис. Рестарт воркера в середине дня не приводит к
// повторному обходу: лиз истечёт, строка вернётся в очередь, но условие
// по дате её не пропустит.
//
// Кого не берём и почему:
//   - брендовые доступы (creator_user_id IS NULL) — это почта и
//     рекламный кабинет заказчика, роликов там нет;
//   - platform='other' и пустой адрес — обходить нечего;
//   - креатор, выведенный из состава (project_creators.removed_at) —
//     его ролики этому проекту больше не принадлежат, и предлагать их
//     сюда значит приписывать чужую работу;
//   - отменённый проект и проект, по которому сбор остановлен
//     (collection_stops_at) — там и сданные ссылки уже не обходятся,
//     тратить кредиты на поиск новых тем более незачем.
func (r *Repo) DueForScan(ctx context.Context, now time.Time, limit int) ([]AccountToScan, error) {
	if limit <= 0 {
		limit = 10
	}
	const q = `
WITH claimed AS (
    SELECT a.id
    FROM project_accounts a
    JOIN projects pr ON pr.id = a.project_id
    WHERE a.creator_user_id IS NOT NULL
      AND a.url <> ''
      AND a.platform <> 'other'
      AND a.next_scan_at <= $1
      AND (a.last_scanned_at IS NULL OR a.last_scanned_at::date < $1::date)
      AND pr.status <> 'cancelled'
      AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)
      AND EXISTS (
          SELECT 1 FROM project_creators pc
          WHERE pc.project_id = a.project_id
            AND pc.creator_user_id = a.creator_user_id
            AND pc.removed_at IS NULL
      )
    ORDER BY a.next_scan_at
    LIMIT $2
    FOR UPDATE OF a SKIP LOCKED
),
taken AS (
    UPDATE project_accounts a
    SET next_scan_at = $1::timestamptz + $3::interval
    FROM claimed c
    WHERE a.id = c.id
    RETURNING a.id, a.project_id, a.creator_user_id, a.platform, a.url
)
SELECT id, project_id, creator_user_id, platform, url FROM taken`
	rows, err := r.db.Query(ctx, q, now, limit, claimLease)
	if err != nil {
		return nil, fmt.Errorf("list accounts due for scan: %w", err)
	}
	defer rows.Close()

	out := make([]AccountToScan, 0, limit)
	for rows.Next() {
		var a AccountToScan
		if err := rows.Scan(&a.AccountID, &a.ProjectID, &a.CreatorUserID,
			&a.Platform, &a.URL); err != nil {
			return nil, fmt.Errorf("scan account due: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkScanned — аккаунт обойдён, следующий раз в НОЛЬ ЧАСОВ.
//
// Ставится и на успех, и на пустой ответ. Пустой ответ стоил ровно того
// же кредита, что и удачный, и повторять его сегодня незачем: приватный
// профиль, неподдержанная площадка и опечатка в адресе к вечеру сами не
// починятся, а слот у живых аккаунтов отнимут.
//
// Полночь, а не «через сутки», и это решение владельца от 2 октября:
// «подписчиков раз в день в 00:00». Разница не косметическая. Срез
// подписчиков — основа доплаты за прирост, а прирост считается по
// границам периода, то есть по ДАТАМ. От «+24 часа» аккаунт уплывает по
// времени суток: обошли в 23:50, следующий раз в 23:50 следующего дня —
// и в один календарный день снимка нет вовсе, а в соседний попадают
// два. На границе периода это ровно та разница, по которой человеку
// платят.
//
// Очередь после полуночи открывается целиком и расходится тиком
// воркера; суточное правило в DueForScan (last_scanned_at::date <
// сегодня) не даёт пройти по аккаунту дважды за день.
func (r *Repo) MarkScanned(ctx context.Context, accountID uuid.UUID, now time.Time) error {
	_, err := r.db.Exec(ctx, `
UPDATE project_accounts
SET last_scanned_at = $2::timestamptz,
    next_scan_at = date_trunc('day', $2::timestamptz) + interval '1 day'
WHERE id = $1`, accountID, now)
	if err != nil {
		return fmt.Errorf("reschedule account scan: %w", err)
	}
	return nil
}

// DeferAccounts — отодвинуть обход пачки, не считая её обойдённой.
//
// Нужен, когда сервис недоступен целиком: last_scanned_at не трогаем
// (поход не состоялся, и сегодняшняя попытка у аккаунта ещё есть), но
// next_scan_at сдвигаем — иначе следующий тик возьмёт ту же пачку,
// снова упрётся в тот же таймаут, и обход встанет навсегда и молча.
func (r *Repo) DeferAccounts(ctx context.Context, accounts []AccountToScan, until time.Time) error {
	if len(accounts) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.AccountID)
	}
	_, err := r.db.Exec(ctx,
		`UPDATE project_accounts SET next_scan_at = $2 WHERE id = ANY($1)`, ids, until)
	if err != nil {
		return fmt.Errorf("defer account scan: %w", err)
	}
	return nil
}

// knownCanonicals — какие из найденных адресов спрашивать уже не надо.
//
// Два разных «уже не надо», и оба обязательны:
//
//   - по адресу уже есть находка в этом проекте — в любом состоянии.
//     Pending значит «карточка висит и ждёт ответа», dismissed — «человек
//     сказал, что не его». Обход идёт каждый день, и без этой проверки
//     отвергнутое возвращалось бы завтра той же карточкой. Уникальный
//     индекс от дублей защищает и сам, но спросить дешевле, чем писать
//     впустую, и только так честно считается, сколько находок НОВЫХ.
//   - ссылка уже сдана. Спрашивать «это ваш ролик?» про ролик, который
//     человек сдал своими руками, — это не подсказка, а признак того,
//     что сервис не помнит, что ему отдали.
//
// Сданное смотрим по всем выкладкам КРЕАТОРА, а не только этого
// проекта: ролик сдают один раз, и если он уже зачтён в соседнем
// проекте, предложить его здесь значит предложить посчитать одну работу
// дважды.
func (r *Repo) knownCanonicals(ctx context.Context, projectID, creatorID uuid.UUID,
	canonical []string) (map[string]bool, error) {

	if len(canonical) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT url_canonical FROM publication_link_suggestions
WHERE project_id = $1 AND url_canonical = ANY($3)
UNION
SELECT l.url_canonical
FROM publication_links l
JOIN project_publications p ON p.id = l.publication_id
WHERE p.creator_user_id = $2 AND l.url_canonical = ANY($3)`,
		projectID, creatorID, canonical)
	if err != nil {
		return nil, fmt.Errorf("check known urls: %w", err)
	}
	defer rows.Close()

	known := make(map[string]bool, len(canonical))
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan known url: %w", err)
		}
		known[u] = true
	}
	return known, rows.Err()
}

// AccountScanStats — что сделал один проход обхода.
type AccountScanStats struct {
	// Considered — аккаунтов взято в работу.
	Considered int `json:"considered"`
	// Scanned — аккаунтов, по которым сервис вернул список роликов.
	Scanned int `json:"scanned"`
	// NoData — сервис ответил, но списка не дал: профиль приватный,
	// площадка не поддержана, адрес указан с опечаткой. Ожидаемый исход,
	// а не сбой.
	NoData int `json:"no_data"`
	// Found — НОВЫХ находок. Ролики, уже сданные или уже спрошенные, сюда
	// не попадают: иначе цифра в логе росла бы каждый день на одно и то
	// же и ничего не значила.
	Found int `json:"found"`
	// Skipped — ролики, до находки не дошедшие: уже сданы, уже спрошены,
	// слишком старые или с адресом, который мы не разбираем.
	Skipped int `json:"skipped"`
}

// RunAccountScan — один проход: взять аккаунты, которым пора, сходить за
// списком свежих роликов, положить то, чего в выкладках нет.
//
// Одной пачкой, а не по аккаунту: instacurl держит два параллельных
// слота, и дробить запрос на мелкие — верный способ упереться в его же
// лимит в минуту.
func (s *Service) RunAccountScan(ctx context.Context, now time.Time, batchSize int) (AccountScanStats, error) {
	var st AccountScanStats
	if s.scanner == nil {
		return st, ErrAccountScannerNotSet
	}

	accounts, err := s.repo.DueForScan(ctx, now, batchSize)
	if err != nil {
		return st, err
	}
	st.Considered = len(accounts)
	if len(accounts) == 0 {
		return st, nil
	}

	// Один адрес может стоять у нескольких строк: один и тот же TikTok
	// креатора заведён в двух проектах. Спрашиваем про него один раз
	// (второй поход стоил бы второго кредита), а разбираем результат для
	// каждой строки отдельно — находка кладётся в свой проект.
	urls := make([]string, 0, len(accounts))
	byURL := make(map[string][]AccountToScan, len(accounts))
	for _, a := range accounts {
		if _, dup := byURL[a.URL]; !dup {
			urls = append(urls, a.URL)
		}
		byURL[a.URL] = append(byURL[a.URL], a)
	}

	results, err := s.scanner.Collect(ctx, urls)
	if err != nil {
		// Причины те же и чинятся так же, что у сбора по ссылкам —
		// сервис-то один. Отдельного счётчика ошибок у обхода нет
		// намеренно: два разных алерта на «ключ отклонён» ловили бы одну
		// и ту же поломку.
		ObserveCollectError(collectErrorReason(err))
		if derr := s.repo.DeferAccounts(ctx, accounts, now.Add(collectRetryDelay)); derr != nil {
			return st, fmt.Errorf("account scan: %w (и отложить не удалось: %v)", err, derr)
		}
		return st, fmt.Errorf("account scan: %w", err)
	}

	seen := make(map[uuid.UUID]bool, len(accounts))
	for _, res := range results {
		matched, ok := byURL[res.URL]
		if !ok {
			// Сервис ответил про то, чего мы не спрашивали. Игнорируем:
			// приписать чужие ролики аккаунту хуже, чем не найти свои.
			continue
		}
		posts := profilePosts(res)
		for _, acc := range matched {
			seen[acc.AccountID] = true
			if err := s.repo.MarkScanned(ctx, acc.AccountID, now); err != nil {
				return st, err
			}
			// Подписчики — ДО проверки на пустой список постов: у личной
			// страницы VK посты собираются, а аудитория скрыта, и
			// наоборот тоже бывает. Снимок и находки — разные ответы на
			// разные вопросы, и терять первый из-за второго незачем.
			if res.Followers != nil {
				if err := s.repo.SaveFollowers(ctx, acc.AccountID, now, *res.Followers); err != nil {
					return st, err
				}
			}
			if len(posts) == 0 {
				st.NoData++
				ObserveAccountScan(acc.Platform, "no_data")
				continue
			}
			st.Scanned++
			ObserveAccountScan(acc.Platform, "ok")
			found, skipped, err := s.suggestFromPosts(ctx, acc, res.Handle, posts, now)
			if err != nil {
				return st, err
			}
			st.Found += found
			st.Skipped += skipped
		}
	}

	// По чему сервис не ответил вовсе — тоже считаем обойдённым.
	// Оставить такую строку с прежним last_scanned_at значит дёргать её
	// на каждом тике: очередь берётся по next_scan_at ASC, и молчащий
	// аккаунт встанет в её начало и займёт слоты у живых.
	for _, a := range accounts {
		if seen[a.AccountID] {
			continue
		}
		st.NoData++
		ObserveAccountScan(a.Platform, "no_data")
		if err := s.repo.MarkScanned(ctx, a.AccountID, now); err != nil {
			return st, err
		}
	}
	return st, nil
}

// profilePosts — ролики из ответа по адресу аккаунта.
//
// kind проверяется строго. kind="media" означает, что в поле аккаунта
// стоит адрес ОДНОГО ролика, а не профиля: человек вставил туда ссылку
// на свою работу. Списка по такому адресу не будет никогда, сколько ни
// ходи, и предлагать этот единственный ролик находкой нельзя — он
// пришёл не из поиска, а из того, что креатор сам вписал в анкету.
func profilePosts(res instacurl.Result) []instacurl.PostMetrics {
	if !res.OK || res.Kind != "profile" {
		return nil
	}
	return res.Posts
}

// suggestFromPosts — превратить список роликов аккаунта в находки.
//
// Возвращает, сколько находок завелось и сколько роликов отсеяно.
func (s *Service) suggestFromPosts(ctx context.Context, acc AccountToScan, handle string,
	posts []instacurl.PostMetrics, now time.Time) (found, skipped int, err error) {

	type candidate struct {
		link      Link
		published time.Time
	}
	cands := make([]candidate, 0, len(posts))
	canonical := make([]string, 0, len(posts))

	oldest := now.AddDate(0, 0, -accountScanLookbackDays)
	for _, p := range posts {
		link, err := ParseLink(p.URL)
		if err != nil {
			// Площадка отдала адрес, который мы не разбираем: профиль
			// ведёт на площадку вне обязательной пятёрки, или это
			// вовсе не ролик. Пропускаем молча — ошибка в одном посте
			// не должна ронять обход всего аккаунта.
			skipped++
			continue
		}
		published := parsePublishedAt(p.PublishedAt)
		// Ролик без даты выхода не отличить от архива, а архив — это
		// основная масса постов на любом живом аккаунте. Предлагать его
		// нельзя: карточек будет десять, и все мимо.
		if published == nil || published.Before(oldest) {
			skipped++
			continue
		}
		cands = append(cands, candidate{link: link, published: *published})
		canonical = append(canonical, link.Canonical)
	}
	if len(cands) == 0 {
		return 0, skipped, nil
	}

	known, err := s.repo.knownCanonicals(ctx, acc.ProjectID, acc.CreatorUserID, canonical)
	if err != nil {
		return 0, skipped, err
	}

	for _, c := range cands {
		if known[c.link.Canonical] {
			skipped++
			continue
		}
		published := c.published
		if _, err := s.AddSuggestion(ctx, AddSuggestionInput{
			ProjectID:     acc.ProjectID,
			CreatorUserID: acc.CreatorUserID,
			URL:           c.link.Raw,
			// Названия ролика сервис не отдаёт, а подпись автора отдаёт —
			// её и кладём: «Нашли в TikTok @anya.kim от 16.09» человек
			// опознаёт, не открывая ссылку.
			AuthorHandle: handle,
			PublishedAt:  &published,
		}); err != nil {
			return found, skipped, fmt.Errorf("add suggestion %s: %w", c.link.Canonical, err)
		}
		found++
	}
	return found, skipped, nil
}

// SaveFollowers — снимок подписчиков аккаунта за день.
//
// Накопительное число, а не прибавка: площадка отдаёт «сколько сейчас»,
// и прирост за период считается разницей крайних снимков. Второй заход в
// тот же день перезаписывает снимок — обход ходит раз в сутки, но
// рестарт воркера и ручной прогон бывают, и тогда свежее число вернее.
func (r *Repo) SaveFollowers(
	ctx context.Context, accountID uuid.UUID, day time.Time, followers int64,
) error {
	_, err := r.db.Exec(ctx, `
INSERT INTO account_follower_daily (account_id, stat_date, followers, taken_at)
VALUES ($1, $2::date, $3, now())
ON CONFLICT (account_id, stat_date) DO UPDATE
SET followers = EXCLUDED.followers, taken_at = now()`, accountID, day, followers)
	if err != nil {
		return fmt.Errorf("save account followers: %w", err)
	}
	return nil
}
