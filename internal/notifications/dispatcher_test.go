package notifications

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestDispatcher(t *testing.T, url string) *Dispatcher {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return NewDispatcher(Config{
		WebhookBaseURL: url,
		HTTPTimeout:    2 * time.Second,
		Registerer:     prometheus.NewRegistry(),
	}, logger)
}

// 200 от n8n → handler возвращает nil (delivered).
func TestDispatcher_2xx_Delivered(t *testing.T) {
	var got envelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	h := d.Handle("project")

	err := h(context.Background(), "agg-123", "project.created", []byte(`{"x":1}`))
	if err != nil {
		t.Errorf("expected nil error on 200, got %v", err)
	}
	if got.EventID != "agg-123" || got.EventType != "project.created" || got.Aggregate != "project" {
		t.Errorf("envelope mismatch: %+v", got)
	}
	if string(got.Payload) != `{"x":1}` {
		t.Errorf("payload mismatch: %s", got.Payload)
	}
}

// 500 → error возвращается (worker должен retry-ить через outbox).
func TestDispatcher_5xx_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("workflow crashed"))
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	h := d.Handle("project")
	err := h(context.Background(), "agg-500", "project.disputed", []byte(`{}`))
	if err == nil {
		t.Errorf("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "n8n returned 500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

// 400 → no error (logical drop, не ретраим), но counter увеличился.
func TestDispatcher_4xx_Dropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad webhook payload"))
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	h := d.Handle("project")
	err := h(context.Background(), "agg-400", "project.created", []byte(`{}`))
	if err != nil {
		t.Errorf("expected nil error on 400 (drop without retry), got %v", err)
	}
}

// Network failure → error.
func TestDispatcher_NetworkError(t *testing.T) {
	// Несуществующий порт → connection refused.
	d := newTestDispatcher(t, "http://127.0.0.1:1/no-such")
	h := d.Handle("project")
	err := h(context.Background(), "agg-net", "step.transitioned", []byte(`{}`))
	if err == nil {
		t.Error("expected network error, got nil")
	}
}

// Headers: Content-Type + extra headers применяются.
func TestDispatcher_Headers(t *testing.T) {
	var gotCT, gotAuth string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("X-Webhook-Token")
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d := NewDispatcher(Config{
		WebhookBaseURL: srv.URL,
		HTTPTimeout:    2 * time.Second,
		ExtraHeaders:   map[string]string{"X-Webhook-Token": "secret123"},
		Registerer:     prometheus.NewRegistry(),
	}, logger)
	h := d.Handle("project")
	if err := h(context.Background(), "agg-h", "project.created", []byte(`{}`)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotAuth != "secret123" {
		t.Errorf("X-Webhook-Token = %q", gotAuth)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}
