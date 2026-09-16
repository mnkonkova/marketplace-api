package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"marketpclce/internal/auth"
	"marketpclce/tests/integration"
)

// Отказ на входе обязан различать «в запросе не то» и «пароль не тот».
//
// Раньше оба случая отвечали одинаково — 401 bad_credentials. Клиент,
// назвавший поле `email` вместо `login`, читал «неверный логин или
// пароль» и шёл искать проблему в паролях. Один раз это кончилось тем,
// что на общем стенде вслепую переписали хеши четырём учётным записям —
// и только потом кто-то открыл DTO ручки и увидел имя поля.
//
// При этом подсказывать, СУЩЕСТВУЕТ ли пользователь, по-прежнему нельзя:
// на неверный пароль и на несуществующий логин ответ обязан остаться
// одним и тем же. Тест сторожит обе половины сразу — их легко разменять
// одну на другую.
func TestLoginTellsMalformedRequestFromWrongPassword(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	user, cleanup := h.NewUser(t, userOpts{Kind: "client"})
	defer cleanup()

	var email string
	if err := pool.QueryRow(t.Context(), `SELECT email FROM users WHERE id = $1`, user).
		Scan(&email); err != nil {
		t.Fatalf("почта пользователя: %v", err)
	}

	svc := auth.NewService(auth.NewRepo(pool), auth.NewTokenIssuer("test-secret", time.Hour, 24*time.Hour))
	r := chi.NewRouter()
	r.Post("/api/v1/auth/login", auth.NewHandler(svc).Login)

	post := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	t.Run("поле названо не так — это ошибка запроса", func(t *testing.T) {
		code, body := post(t, `{"email":"`+email+`","password":"whatever"}`)
		if code != http.StatusBadRequest || body["error"] != "invalid_input" {
			t.Fatalf("код %d, ошибка %v — ожидался 400/invalid_input; иначе опечатку в имени поля ищут в паролях",
				code, body["error"])
		}
	})

	t.Run("пароля нет вовсе — тоже ошибка запроса", func(t *testing.T) {
		code, body := post(t, `{"login":"`+email+`"}`)
		if code != http.StatusBadRequest || body["error"] != "invalid_input" {
			t.Fatalf("код %d, ошибка %v — ожидался 400/invalid_input", code, body["error"])
		}
	})

	// А вот дальше подсказывать нельзя: и на чужой пароль, и на
	// несуществующий логин ответ один и тот же, иначе по нему перебирают
	// существующие учётные записи.
	t.Run("неверный пароль и несуществующий логин неразличимы", func(t *testing.T) {
		wrongPass, b1 := post(t, `{"login":"`+email+`","password":"not-the-password"}`)
		noSuch, b2 := post(t, `{"login":"nobody-`+email+`","password":"not-the-password"}`)
		if wrongPass != http.StatusUnauthorized || noSuch != http.StatusUnauthorized {
			t.Fatalf("коды %d и %d — оба ожидались 401", wrongPass, noSuch)
		}
		if b1["error"] != b2["error"] || b1["message"] != b2["message"] {
			t.Errorf("ответы различаются: %v / %v — по ним перебирают существующие учётные записи", b1, b2)
		}
	})
}
