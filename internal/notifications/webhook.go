package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WebhookDispatcher шлёт event payload в HTTP webhook (n8n / любой иной).
// Один воркер CRM-событий → один URL. Идемпотентность на стороне n8n
// обеспечивается передачей event_id (уникальный id из outbox).
type WebhookDispatcher struct {
	url    string
	token  string
	client *http.Client
}

func NewWebhookDispatcher(url, token string) *WebhookDispatcher {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	return &WebhookDispatcher{
		url:   url,
		token: token,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Payload — то что отправляется в n8n. EventID — для идемпотентности.
// Payload.Data — оригинальный outbox payload (JSON-объект); n8n ветвится
// по EventType.
type Payload struct {
	EventID     string          `json:"event_id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregate_id"`
	EventType   string          `json:"event_type"`
	Data        json.RawMessage `json:"data,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

// Send — POST JSON в webhook. Возвращает ошибку при не-2xx — outbox
// поставит retry с экспоненциальным backoff. Тело ответа n8n не нужно
// (пишем head + drain).
func (d *WebhookDispatcher) Send(ctx context.Context, p Payload) error {
	if d == nil {
		return nil
	}
	body, err := json.Marshal(p)
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
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return errors.New("webhook returned non-2xx: " + resp.Status)
	}
	return nil
}
