package httpx_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"marketpclce/internal/httpx"
)

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteJSON(w, 201, map[string]string{"id": "abc"})

	if got := w.Code; got != 201 {
		t.Errorf("status: got %d, want 201", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("body не валидный JSON: %v", err)
	}
	if body["id"] != "abc" {
		t.Errorf("body[id]: got %q, want abc", body["id"])
	}
}

func TestWriteErr(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteErr(w, 403, "email_unverified")

	if got := w.Code; got != 403 {
		t.Errorf("status: got %d, want 403", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}
	body := strings.TrimSpace(w.Body.String())
	want := `{"error":"email_unverified"}`
	if body != want {
		t.Errorf("body: got %q, want %q", body, want)
	}
}

// WriteJSON должен корректно сериализовать nil-тело — пишет "null\n"
// (поведение json.Encoder). Регрессия: если кто-то заменит на ручную
// сборку и забудет — этот тест поймает.
func TestWriteJSONNil(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.WriteJSON(w, 200, nil)
	if w.Code != 200 {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "null" {
		t.Errorf("body: got %q, want \"null\"", got)
	}
}
