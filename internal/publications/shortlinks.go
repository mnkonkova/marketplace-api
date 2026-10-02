package publications

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Короткие ссылки «поделиться» и почему их разворачиваем МЫ.
//
// Приложение TikTok даёт ссылку вида vt.tiktok.com/ZSbyuhrDf — в ней нет
// ни автора, ни id ролика, только код редиректа. Мы такую ссылку
// принимали как есть: в коде стояло допущение «редирект развернёт
// сборщик».
//
// Допущение оказалось неверным. 1 октября 2026 сборщик ответил на неё
// «kind: profile, handle: ZSbyuhrDf, Account doesn't exist» — он принял
// хвост короткой ссылки за имя аккаунта. Та же ссылка в полном виде
// отдала 680 просмотров. То есть у ролика, который собрался бы без
// единой проблемы, в кабинете стоял ноль — и ноль этот читается как
// «никто не смотрит».
//
// Поэтому разворачиваем сами, в момент сдачи: один раз на ссылку,
// вместо того чтобы надеяться на чужую сторону при каждом обходе.

// shortHosts — хосты, которые означают «здесь только редирект».
//
// Список, а не «разворачивать всё»: ходить в сеть при сдаче каждой
// ссылки незачем, и лишний поход на живой адрес — это лишняя задержка в
// форме, которую человек ждёт.
var shortHosts = map[string]bool{
	"vt.tiktok.com": true,
	"vm.tiktok.com": true,
	"l.likee.video": true,
	"vk.cc":         true,
}

// expandTimeout — сколько ждём редиректа.
//
// Пять секунд: это внутри запроса, который ждёт человек, и сдача ссылки
// не должна зависеть от того, как сегодня себя чувствует чужая площадка.
// Не успели — сохраняем как есть, ровно как было раньше.
const expandTimeout = 5 * time.Second

// URLExpander — кто разворачивает короткие ссылки. Интерфейс нужен
// тесту: ходить в живой TikTok из прогона нельзя, а проверять надо
// именно поведение вокруг — что развёрнутое сохраняется, а отказ не
// ломает сдачу.
type URLExpander interface {
	Expand(ctx context.Context, raw string) (string, error)
}

// httpExpander — обычный GET с остановкой на первом же редиректе.
//
// Тело не читаем и не ждём: нужен только Location. Площадки на короткий
// адрес отвечают 301/302 сразу, а полноценная загрузка страницы ролика
// стоила бы секунд.
type httpExpander struct{ client *http.Client }

// NewURLExpander — разворачиватель коротких ссылок поверх http.Client.
func NewURLExpander() URLExpander {
	return &httpExpander{
		client: &http.Client{
			Timeout: expandTimeout,
			// Редиректы не ходим цепочкой сами: достаточно первого
			// Location. Цепочка из пяти переходов — это пять походов в
			// сеть внутри формы сдачи.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (e *httpExpander) Expand(ctx context.Context, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	// Без правдоподобного агента TikTok отдаёт заглушку вместо редиректа.
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 "+
			"(KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1")
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", nil
	}
	abs, err := resp.Request.URL.Parse(loc)
	if err != nil {
		return "", err
	}
	return abs.String(), nil
}

// expandAll — развернуть набор ссылок разом.
//
// По одной это пять последовательных походов в сеть по пять секунд
// таймаута каждый; человек в это время смотрит на крутящуюся кнопку
// «Сдать». Параллельно он ждёт самый долгий ответ, а не их сумму.
//
// Порядок сохраняется: результат кладётся в тот же индекс, что и вход,
// — дальше по нему же определяется площадка.
func (s *Service) expandAll(ctx context.Context, urls []string) []string {
	out := make([]string, len(urls))
	if s.expander == nil {
		copy(out, urls)
		return out
	}
	// Больше пяти походов разом не делаем никогда.
	//
	// Площадок пять, и столько же ссылок в честной сдаче. Но список
	// приходит от клиента, и ограничение «одна ссылка на площадку»
	// проверяется ПОЗЖЕ, при разборе: без потолка один запрос с тысячей
	// коротких адресов превращался бы в тысячу одновременных походов
	// наружу с нашего адреса.
	sem := make(chan struct{}, len(AllPlatforms))
	var wg sync.WaitGroup
	for i, raw := range urls {
		out[i] = raw
		if !isShortLink(raw) {
			continue
		}
		wg.Add(1)
		go func(i int, raw string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = s.expandShort(ctx, raw)
		}(i, raw)
	}
	wg.Wait()
	return out
}

// WithURLExpander — включить разворачивание коротких ссылок. Без него
// сервис работает как раньше: короткая ссылка сохраняется как есть.
func (s *Service) WithURLExpander(e URLExpander) *Service {
	s.expander = e
	return s
}

// isShortLink — ссылка ведёт на редирект, а не на страницу ролика.
func isShortLink(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return shortHosts[strings.ToLower(strings.TrimPrefix(u.Host, "www."))]
}

// expandShort — развернуть короткую ссылку, если это она.
//
// Любая неудача — это исходная ссылка, а не ошибка сдачи: ролик уже
// вышел, человек его сдаёт, и ронять сдачу из-за недоступного редиректа
// значит потерять работу ради аккуратности адреса. Хуже, чем было, при
// этом не становится — было ровно то же самое.
func (s *Service) expandShort(ctx context.Context, raw string) string {
	if s.expander == nil || !isShortLink(raw) {
		return raw
	}
	ctx, cancel := context.WithTimeout(ctx, expandTimeout)
	defer cancel()
	full, err := s.expander.Expand(ctx, raw)
	if err != nil || strings.TrimSpace(full) == "" {
		return raw
	}
	// Развернулось в тот же короткий хост (цепочка редиректов, капча) —
	// толку нет, оставляем исходное.
	if isShortLink(full) {
		return raw
	}
	return full
}
