package mailer_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"marketpclce/internal/mailer"
)

// TestSend_FailsWithoutAPIKey — Send без api_key возвращает ошибку,
// чтобы воркер не отправлял пустые запросы в Unisender.
func TestSend_FailsWithoutAPIKey(t *testing.T) {
	u := mailer.NewUnisenderGo(mailer.UnisenderGoConfig{
		APIKey:    "",
		BaseURL:   "https://example",
		FromEmail: "from@x",
	})
	err := u.Send(context.Background(), mailer.Message{To: "to@x", Subject: "S", Plain: "P"})
	if err == nil || !strings.Contains(err.Error(), "api key") {
		t.Errorf("ожидаем ошибку api key, got %v", err)
	}
}

func TestSend_FailsWithoutFromEmail(t *testing.T) {
	u := mailer.NewUnisenderGo(mailer.UnisenderGoConfig{
		APIKey:    "key",
		BaseURL:   "https://example",
		FromEmail: "",
	})
	err := u.Send(context.Background(), mailer.Message{To: "to@x", Subject: "S", Plain: "P"})
	if err == nil || !strings.Contains(err.Error(), "from_email") {
		t.Errorf("ожидаем ошибку from_email, got %v", err)
	}
}

// TestSend_SuccessfulPOST — реальный httptest сервер отвечает 200,
// проверяем что unisender body содержит правильные поля.
func TestSend_SuccessfulPOST(t *testing.T) {
	var captured struct {
		path      string
		apiKey    string
		body      []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.apiKey = r.Header.Get("X-API-KEY")
		captured.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"job_id":"123"}`))
	}))
	defer srv.Close()

	u := mailer.NewUnisenderGo(mailer.UnisenderGoConfig{
		APIKey:    "test-api-key",
		BaseURL:   srv.URL,
		FromEmail: "noreply@app.test",
		FromName:  "App",
	})
	err := u.Send(context.Background(), mailer.Message{
		To:      "user@example.com",
		ToName:  "User",
		Subject: "Подтверждение",
		Plain:   "Привет",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if captured.path != "/email/send.json" {
		t.Errorf("path: want /email/send.json, got %s", captured.path)
	}
	if captured.apiKey != "test-api-key" {
		t.Errorf("X-API-KEY: %s", captured.apiKey)
	}
	// Body содержит ожидаемые поля Unisender Go API.
	var parsed map[string]any
	if err := json.Unmarshal(captured.body, &parsed); err != nil {
		t.Fatalf("body не JSON: %v", err)
	}
	msg, _ := parsed["message"].(map[string]any)
	if msg["subject"] != "Подтверждение" || msg["from_email"] != "noreply@app.test" {
		t.Errorf("subject/from_email mismatch: %+v", msg)
	}
}

// TestSend_FailsOn4xx — Unisender вернул 400 (например невалидный email),
// dispatcher возвращает ошибку с кодом и телом.
func TestSend_FailsOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid email"}`))
	}))
	defer srv.Close()

	u := mailer.NewUnisenderGo(mailer.UnisenderGoConfig{
		APIKey:    "k",
		BaseURL:   srv.URL,
		FromEmail: "from@app",
	})
	err := u.Send(context.Background(), mailer.Message{To: "bad", Subject: "s", Plain: "p"})
	if err == nil {
		t.Fatalf("ожидаем ошибку для 400")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error должна содержать статус и тело: %v", err)
	}
}

// TestSend_TrimsTrailingSlashFromBaseURL — конфиг с trailing slash не
// должен ломать сборку URL до /email/send.json.
func TestSend_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := mailer.NewUnisenderGo(mailer.UnisenderGoConfig{
		APIKey:    "k",
		BaseURL:   srv.URL + "/",
		FromEmail: "from@app",
	})
	if err := u.Send(context.Background(), mailer.Message{To: "u@x", Plain: "p"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if hitPath != "/email/send.json" {
		t.Errorf("ожидаем что trailing-slash отброшен: hitPath=%s", hitPath)
	}
}
