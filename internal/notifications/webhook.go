package notifications

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"marketpclce/internal/outbox"
)

// WebhookDispatcher шлёт event payload в HTTP webhook (n8n / любой иной).
// Один воркер CRM-событий → один URL. Идемпотентность на стороне n8n
// обеспечивается передачей event_id (уникальный id из outbox).
type WebhookDispatcher struct {
	url        string
	token      string
	appBaseURL string
	// secret — ключ подписи тела (X-Signature, HMAC-SHA256 hex). Пусто
	// — заголовка нет. Нужен там, где принимающая сторона наша и
	// обязана отличить наш запрос от чужого: у n8n для этого bearer, у
	// сервиса бота — подпись, как у «Бота Работ».
	secret string
	client *http.Client
}

// WithSignature — подписывать тело. Возвращает тот же диспетчер,
// чтобы собираться одной строкой.
func (d *WebhookDispatcher) WithSignature(secret string) *WebhookDispatcher {
	if d == nil {
		return nil
	}
	d.secret = secret
	return d
}

// WithTimeout — сколько ждём ответа принимающей стороны.
//
// Десять секунд по умолчанию — про n8n, который живёт рядом и отвечает
// мгновенно. Для сервиса, который умеет засыпать (бот на Railway),
// этого мало и ошибка получается обманчивой: контейнер в этот момент
// просыпается, а мы обрываем запрос по таймауту — и так КАЖДУЮ
// попытку. Событие проходит все десять ретраев за полчаса, ни разу не
// дав адресату шанса ответить, и уезжает в DLQ с «context deadline
// exceeded». Ровно это и случилось 1 октября 2026 с project.created.
func (d *WebhookDispatcher) WithTimeout(t time.Duration) *WebhookDispatcher {
	if d == nil || t <= 0 {
		return d
	}
	d.client.Timeout = t
	return d
}

// AppBaseURL — адрес фронта, который уезжает в payload. Нужен там, где
// тело собирают снаружи: ссылки в сообщении бота ведут в кабинет, и
// собирать их из ничего нельзя.
func (d *WebhookDispatcher) AppBaseURL() string {
	if d == nil {
		return ""
	}
	return d.appBaseURL
}

// NewWebhookDispatcher — appBaseURL пробрасывается в payload, чтобы n8n
// мог собирать ссылки на CRM (`/manager/projects/{id}`) без доступа к
// env vars (n8n task runner их блокирует) и без хардкода в Code node.
func NewWebhookDispatcher(url, token, appBaseURL string) *WebhookDispatcher {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	return &WebhookDispatcher{
		url:        url,
		token:      token,
		appBaseURL: appBaseURL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Payload — то что отправляется в n8n. EventID — для идемпотентности.
// Payload.Data — оригинальный outbox payload (JSON-объект); n8n ветвится
// по EventType. AppBaseURL — базовый URL фронта (для сборки ссылок).
type Payload struct {
	EventID     string          `json:"event_id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregate_id"`
	EventType   string          `json:"event_type"`
	Data        json.RawMessage `json:"data,omitempty"`
	AppBaseURL  string          `json:"app_base_url,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

// Send — POST JSON в webhook. Возвращает ошибку при не-2xx — outbox
// поставит retry с экспоненциальным backoff. Тело ответа n8n не нужно
// (пишем head + drain).
func (d *WebhookDispatcher) Send(ctx context.Context, p Payload) error {
	if d == nil {
		return nil
	}
	if p.AppBaseURL == "" {
		p.AppBaseURL = d.appBaseURL
	}
	return d.post(ctx, p)
}

// SendEnvelope — отправить произвольное тело тем же путём: те же коды
// ответа, те же ретраи, та же подпись.
//
// Нужен доставке в бот: ей мало Payload — вместе с событием едет
// список адресатов, посчитанный НАМИ. Считать его на стороне бота
// значило бы завести вторую копию правил доставки.
func (d *WebhookDispatcher) SendEnvelope(ctx context.Context, v any) error {
	if d == nil {
		return nil
	}
	return d.post(ctx, v)
}

func (d *WebhookDispatcher) post(ctx context.Context, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	if d.secret != "" {
		mac := hmac.New(sha256.New, []byte(d.secret))
		mac.Write(body)
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		// Сетевая ошибка / таймаут / DNS / connection reset — транзиентно,
		// retry имеет смысл.
		return fmt.Errorf("post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode/100 == 5:
		// 408/429/5xx — транзиентные. 408/5xx — на стороне n8n переходный
		// сбой, 429 — backpressure (наш экспоненциальный backoff с retry
		// сделает то что надо). Возвращаем обычную ошибку → retry.
		return fmt.Errorf("webhook returned non-2xx: %s", resp.Status)
	default:
		// P4: 4xx (кроме 408/429) — перманентный отказ. 400 (битый payload),
		// 401/403 (токен/доступ), 404 (workflow удалён в n8n), 422 (валидация).
		// Дальнейшие retry бессмысленны — сразу в DLQ через outbox.ErrPermanent.
		return fmt.Errorf("webhook returned %s: %w", resp.Status, outbox.ErrPermanent)
	}
}
