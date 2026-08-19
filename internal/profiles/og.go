package profiles

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Превью ссылки специалиста в мессенджерах.
//
// Проблема: фронт — SPA, а превьюшники Telegram/WhatsApp/VK не исполняют JS.
// Они читают og-теги из ПЕРВОГО HTML-ответа, а там пустой index.html с общей
// метой сайта — поэтому ссылка на конкретного специалиста разворачивалась
// безликой карточкой «Wayprmarket» или не разворачивалась вовсе.
//
// Решение без полноценного SSR: API перехватывает GET /specialist/{id},
// берёт у Caddy готовую оболочку index.html и вставляет в <head> мету под
// конкретного человека. Дальше SPA грузится как обычно — для пользователя
// поведение не меняется, бот получает нужные теги.
//
// Мету отдаём ВСЕМ, а не только ботам по User-Agent: детект UA приходится
// поддерживать вечно (у каждого мессенджера свой агент, они меняются), а
// лишние теги обычному браузеру не мешают.

// spaShell — кеш оболочки index.html. Оболочка меняется только при деплое
// фронта, поэтому пары минут TTL достаточно, а лишний HTTP-хоп на каждый
// заход бота не нужен.
type spaShell struct {
	url    string
	client *http.Client

	mu        sync.RWMutex
	html      string
	fetchedAt time.Time
	ttl       time.Duration
}

func newSPAShell(url string) *spaShell {
	return &spaShell{
		url:    url,
		client: &http.Client{Timeout: 3 * time.Second},
		ttl:    2 * time.Minute,
	}
}

func (s *spaShell) get(ctx context.Context) (string, error) {
	s.mu.RLock()
	if s.html != "" && time.Since(s.fetchedAt) < s.ttl {
		cached := s.html
		s.mu.RUnlock()
		return cached, nil
	}
	s.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spa shell: %s вернул %d", s.url, resp.StatusCode)
	}
	// 512 КБ с запасом: index.html у нас ~4 КБ, всё что сильно больше —
	// признак того, что по этому URL лежит не оболочка.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.html = string(body)
	s.fetchedAt = time.Now()
	s.mu.Unlock()
	return s.html, nil
}

// ogMeta — то, что видит превьюшник.
type ogMeta struct {
	Title       string
	Description string
	Image       string
	URL         string
}

// buildOGMeta собирает мету из публичного профиля.
//
// Картинка — постер закреплённой работы: именно её специалист выбрал как
// визитку. Фолбэк — постер любой работы, затем аватар. Пустая картинка
// лучше кривой: Telegram при недоступном og:image рисует карточку без
// превью, а не ломается.
func buildOGMeta(p PublicProfile, baseURL string) ogMeta {
	roles := make([]string, 0, 3)
	for _, c := range p.Categories {
		if len(roles) == 3 {
			break
		}
		roles = append(roles, strings.ToLower(c.Title))
	}
	title := p.DisplayName
	if len(roles) > 0 {
		title += " — " + strings.Join(roles, ", ")
	}

	where := p.City
	if where == "" {
		where = "Удалённо"
	}
	format := p.ProductionName
	if format == "" && p.IsFreelance {
		format = "фриланс"
	}
	parts := []string{where}
	if format != "" {
		parts = append(parts, format)
	}
	parts = append(parts, fmt.Sprintf("%d %s в портфолио", len(p.Portfolio), pluralWorks(len(p.Portfolio))))

	handle := p.Username
	if handle == "" {
		handle = p.UserID.String()
	}

	return ogMeta{
		Title:       title,
		Description: strings.Join(parts, " · "),
		Image:       pickOGImage(p),
		URL:         strings.TrimRight(baseURL, "/") + "/specialist/" + handle,
	}
}

// pickOGImage — постер флагмана, иначе первой работы с постером, иначе
// аватар. Анимированные webp сознательно не берём: Telegram их не всегда
// тянет, а битая картинка хуже отсутствующей.
func pickOGImage(p PublicProfile) string {
	for _, item := range p.Portfolio {
		if item.IsFeatured && item.ThumbnailURL != "" {
			return item.ThumbnailURL
		}
	}
	for _, item := range p.Portfolio {
		if item.ThumbnailURL != "" {
			return item.ThumbnailURL
		}
	}
	return p.AvatarURL
}

func pluralWorks(n int) string {
	m10, m100 := n%10, n%100
	switch {
	case m10 == 1 && m100 != 11:
		return "работа"
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		return "работы"
	default:
		return "работ"
	}
}

// injectOG вставляет теги перед </head>, предварительно вырезав общие
// og-теги сайта — иначе в документе окажется по два og:title, и какой из
// них возьмёт превьюшник, не определено.
func injectOG(shell string, m ogMeta) string {
	shell = stripSiteOGTags(shell)
	shell = replaceTitle(shell, m.Title)

	var b strings.Builder
	b.WriteString("\n    <!-- og-мета конкретного специалиста, подставлена сервером -->\n")
	writeMeta := func(attr, name, content string) {
		if content == "" {
			return
		}
		fmt.Fprintf(&b, "    <meta %s=%q content=%q />\n", attr, name, html.EscapeString(content))
	}
	writeMeta("property", "og:title", m.Title)
	writeMeta("property", "og:description", m.Description)
	writeMeta("property", "og:url", m.URL)
	writeMeta("property", "og:type", "profile")
	writeMeta("name", "description", m.Description)
	if m.Image != "" {
		writeMeta("property", "og:image", m.Image)
		// Размеры Telegram использует для раскладки карточки; постеры у нас
		// 720p по большей стороне (см. ExtractThumbnail в transcode).
		writeMeta("property", "og:image:width", "1280")
		writeMeta("property", "og:image:height", "720")
		writeMeta("name", "twitter:card", "summary_large_image")
		writeMeta("name", "twitter:image", m.Image)
	}
	writeMeta("name", "twitter:title", m.Title)
	writeMeta("name", "twitter:description", m.Description)

	if idx := strings.Index(shell, "</head>"); idx >= 0 {
		return shell[:idx] + b.String() + shell[idx:]
	}
	// Нет </head> — оболочка не та, что мы ждали. Отдаём как есть: пусть
	// страница работает без превью, это лучше сломанного HTML.
	return shell
}

// replaceTitle меняет <title> оболочки на имя специалиста. og:title важнее
// для мессенджеров, но <title> уходит в поиск и в заголовок вкладки — а
// SPA проставит его только после загрузки JS, которого у бота нет.
func replaceTitle(shell, title string) string {
	if title == "" {
		return shell
	}
	start := strings.Index(shell, "<title>")
	end := strings.Index(shell, "</title>")
	if start < 0 || end < start {
		return shell
	}
	return shell[:start+len("<title>")] + html.EscapeString(title) + " · wayprmarket" + shell[end:]
}

// stripSiteOGTags убирает общие og:/twitter:-теги из index.html.
func stripSiteOGTags(shell string) string {
	var out strings.Builder
	for _, line := range strings.Split(shell, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<meta") &&
			(strings.Contains(trimmed, `property="og:`) ||
				strings.Contains(trimmed, `name="twitter:`) ||
				strings.Contains(trimmed, `name="description"`)) {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// SpecialistPage — GET /specialist/{id}. Отдаёт оболочку SPA с подставленной
// метой. На любой сбой (нет такого специалиста, оболочка недоступна)
// возвращает 5xx/404 — Caddy в этом случае сам отдаёт статический
// index.html, и страница просто открывается без персональной меты.
func (h *Handler) SpecialistPage(w http.ResponseWriter, r *http.Request) {
	if h.shell == nil {
		http.Error(w, "spa shell not configured", http.StatusInternalServerError)
		return
	}
	handle := chi.URLParam(r, "id")
	id, err := uuid.Parse(handle)
	if err != nil {
		resolved, rerr := h.svc.ResolveUserIDByUsername(r.Context(), handle)
		if rerr != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		id = resolved
	}
	p, err := h.svc.GetPublic(r.Context(), id)
	if err != nil {
		// Неопубликованный или несуществующий профиль: меты быть не должно,
		// пусть SPA сам покажет «Профиль не найден».
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	shell, err := h.shell.get(r.Context())
	if err != nil {
		http.Error(w, "shell unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Превью кешируют и мессенджеры, и браузер. Короткий public-кеш даёт
	// выигрыш на повторных заходах, но не морозит мету после смены промо.
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, injectOG(shell, buildOGMeta(p, h.appBaseURL)))
}
