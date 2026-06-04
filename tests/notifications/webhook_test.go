package notifications_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"marketpclce/internal/notifications"
)

// TestNewWebhookDispatcher_EmptyURL — без URL возвращает nil, чтобы
// воркер мог чекать nil-приёмник и пропускать события.
func TestNewWebhookDispatcher_EmptyURL(t *testing.T) {
	if d := notifications.NewWebhookDispatcher("", "tok", "https://app"); d != nil {
		t.Errorf("ожидали nil для пустого url, got %v", d)
	}
	if d := notifications.NewWebhookDispatcher("   ", "tok", "https://app"); d != nil {
		t.Errorf("whitespace-only url → nil, got %v", d)
	}
}

// TestSend_NilDispatcher_NoOp — Send на nil-receiver не паникует, тихо ОК.
// Используется в worker'е без N8N_WEBHOOK_URL.
func TestSend_NilDispatcher_NoOp(t *testing.T) {
	var d *notifications.WebhookDispatcher
	err := d.Send(context.Background(), notifications.Payload{EventID: "x"})
	if err != nil {
		t.Errorf("nil dispatcher Send: want nil err, got %v", err)
	}
}

// TestSend_SuccessOnHTTP2xx — реальный httptest сервер отвечает 200,
// dispatcher не возвращает ошибку. Запрос имеет правильный Content-Type
// и Bearer-токен.
func TestSend_SuccessOnHTTP2xx(t *testing.T) {
	var gotReq struct {
		ContentType string
		Auth        string
		Body        []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq.ContentType = r.Header.Get("Content-Type")
		gotReq.Auth = r.Header.Get("Authorization")
		gotReq.Body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := notifications.NewWebhookDispatcher(srv.URL, "my-token", "https://app.test")
	err := d.Send(context.Background(), notifications.Payload{
		EventID:     "outbox/42",
		Aggregate:   "project",
		AggregateID: "proj-1",
		EventType:   "project.created",
		Data:        json.RawMessage(`{"k":"v"}`),
		OccurredAt:  time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotReq.ContentType != "application/json" {
		t.Errorf("content-type: %s", gotReq.ContentType)
	}
	if gotReq.Auth != "Bearer my-token" {
		t.Errorf("auth header: %s", gotReq.Auth)
	}
	// Тело — JSON c нашими полями.
	var parsed map[string]any
	if err := json.Unmarshal(gotReq.Body, &parsed); err != nil {
		t.Fatalf("body не JSON: %v", err)
	}
	if parsed["event_id"] != "outbox/42" {
		t.Errorf("event_id missing: %+v", parsed)
	}
	if parsed["app_base_url"] != "https://app.test" {
		t.Errorf("app_base_url не вписан из конфига: %+v", parsed)
	}
}

// TestSend_Without_Token_NoAuthHeader — если токен пустой, заголовок
// Authorization не выставляется (n8n без auth поддерживается).
func TestSend_Without_Token_NoAuthHeader(t *testing.T) {
	authSeen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := notifications.NewWebhookDispatcher(srv.URL, "", "https://app")
	if err := d.Send(context.Background(), notifications.Payload{EventID: "1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if authSeen != "" {
		t.Errorf("без token Authorization не должен ставиться, got %q", authSeen)
	}
}

// TestSend_FailsOnNon2xx — n8n вернул 500, dispatcher возвращает ошибку,
// outbox её увидит и поставит retry.
func TestSend_FailsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	d := notifications.NewWebhookDispatcher(srv.URL, "", "")
	err := d.Send(context.Background(), notifications.Payload{EventID: "x"})
	if err == nil {
		t.Fatalf("ожидали ошибку для 500")
	}
	if !strings.Contains(err.Error(), "non-2xx") {
		t.Errorf("error message: %v", err)
	}
}

// TestSend_AppBaseURLFromPayloadOverridesConfig — если payload содержит
// AppBaseURL, конфиг не подменяется (тест-удобство).
func TestSend_AppBaseURLFromPayloadOverridesConfig(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		got, _ = p["app_base_url"].(string)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := notifications.NewWebhookDispatcher(srv.URL, "", "https://default.app")
	err := d.Send(context.Background(), notifications.Payload{
		EventID:    "x",
		AppBaseURL: "https://override.app",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != "https://override.app" {
		t.Errorf("override не сработал: %q", got)
	}
}
