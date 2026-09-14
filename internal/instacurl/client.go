// Package instacurl — тонкий клиент к сервису сбора публичной статистики
// (FastAPI, отдельный VDS). Контракт: POST /collect с заголовком
// X-API-Key, тело {urls: [...]}, ответ {results: [...]}.
//
// Важное про этот сервис, что определяет форму клиента:
//   - он держит очередь на ДВА параллельных запроса, поэтому слать пачки
//     параллельно бессмысленно — упрутся в тот же лимит;
//   - у него свой rate limit (по умолчанию 60/минуту);
//   - кеш 7 дней: повторный запрос той же ссылки может вернуться из кеша
//     с from_cache=true, и это нормальный ответ, а не ошибка;
//   - метрики отдельного ролика он умеет не для всех площадок. Для
//     неподдержанных приходит ok=false с внятным error — это ожидаемый
//     ответ, а не сбой связи.
package instacurl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	// ErrNotConfigured — адрес или ключ не заданы. Клиент в этом случае
	// не создаётся: неработающая интеграция хуже отсутствующей, потому
	// что молча возвращает нули вместо просмотров.
	ErrNotConfigured = errors.New("instacurl: не задан адрес или ключ")
	// ErrUnauthorized — ключ не принят.
	ErrUnauthorized = errors.New("instacurl: ключ отклонён")
	// ErrRateLimited — сервис попросил притормозить. Ретраить сразу
	// бессмысленно: у него окно в минуту.
	ErrRateLimited = errors.New("instacurl: превышен лимит запросов")
)

// PostMetrics — метрики одного ролика.
type PostMetrics struct {
	ID          string `json:"id"`
	URL         string `json:"url,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Views       *int64 `json:"views,omitempty"`
	Likes       *int64 `json:"likes,omitempty"`
	Comments    *int64 `json:"comments,omitempty"`
	Shares      *int64 `json:"shares,omitempty"`
}

// Result — то, что сервис вернул по одной ссылке.
//
// OK=false — это НЕ ошибка транспорта: площадка не поддержана, ролик
// удалён, кончились кредиты у поставщика. Такой ответ надо сохранить и
// показать, а не ретраить до посинения.
type Result struct {
	Platform  string        `json:"platform"`
	URL       string        `json:"url"`
	Kind      string        `json:"kind"`
	MediaID   string        `json:"media_id,omitempty"`
	OK        bool          `json:"ok"`
	Error     string        `json:"error,omitempty"`
	ErrorCode string        `json:"error_code,omitempty"`
	FromCache bool          `json:"from_cache"`
	CacheAge  float64       `json:"cache_age_seconds,omitempty"`
	Posts     []PostMetrics `json:"posts"`
	Notes     []string      `json:"notes,omitempty"`
}

// Metrics — метрики ролика из ответа. Второе значение false, если сервис
// ответил ok=false или не приложил ни одного поста.
func (r Result) Metrics() (PostMetrics, bool) {
	if !r.OK || len(r.Posts) == 0 {
		return PostMetrics{}, false
	}
	return r.Posts[0], true
}

type collectReq struct {
	URLs  []string `json:"urls"`
	Force bool     `json:"force,omitempty"`
}

type collectResp struct {
	Results []Result `json:"results"`
}

// Client — HTTP-клиент к instacurl.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New — nil, если адрес или ключ не заданы. Вызывающий обязан это
// проверить и не запускать сбор: см. ErrNotConfigured.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	if baseURL == "" || apiKey == "" {
		return nil
	}
	if timeout <= 0 {
		// Сервис ходит в чужие площадки через Playwright: медленный ответ
		// здесь — норма, а не признак поломки.
		timeout = 60 * time.Second
	}
	return &Client{
		baseURL: trimSlash(baseURL),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

// Collect — метрики по списку ссылок. Пустой список — не ошибка.
func (c *Client) Collect(ctx context.Context, urls []string) ([]Result, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if len(urls) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(collectReq{URLs: urls})
	if err != nil {
		return nil, fmt.Errorf("marshal collect request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/collect", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build collect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call instacurl: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	default:
		return nil, fmt.Errorf("instacurl ответил %d", resp.StatusCode)
	}

	var out collectResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode collect response: %w", err)
	}
	return out.Results, nil
}

// Health — жив ли сервис. Нужен для алерта InstacurlDown.
func (c *Client) Health(ctx context.Context) error {
	if c == nil {
		return ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("instacurl /health ответил %d", resp.StatusCode)
	}
	return nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
