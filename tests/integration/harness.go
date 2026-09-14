package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/auth"
	"marketpclce/internal/billing"
	"marketpclce/internal/httpapi"
	"marketpclce/internal/orders"
	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
)

// APIHarness — настоящий роутер приложения на тестовой БД.
//
// Живёт в импортируемом пакете, а не в _test-файле, чтобы им пользовались
// и интеграционные тесты, и сквозные из tests/e2e. Второй экземпляр этого
// же харнесса рядом разъехался бы с первым на первой же правке роутера.
//
// Зачем он: до сих пор HTTP-слой не был покрыт ничем. Ручки тестировались
// через сервисы, а middleware — авторизация, проверка роли, отзыв токена —
// не проверялись вообще. Между тем именно там живёт граница доступа:
// сервис не знает, что запрос пришёл от заказчика под видом менеджера.
//
// Харнесс поднимает httpapi.NewRouter с реальными auth.Repo и TokenIssuer,
// поэтому проверяется тот же путь, что в проде: Bearer → разбор токена →
// проверка отзыва → LoadIdentity → сверка роли → хендлер.
type APIHarness struct {
	// Pool — та же тестовая база. Наружу, потому что сквозные сценарии
	// иногда кладут в неё то, чего приложение не создаёт: статистику
	// просмотров собирает instacurl, ходящий на пять внешних площадок.
	Pool   *pgxpool.Pool
	srv    http.Handler
	issuer *auth.TokenIssuer
}

func NewAPIHarness(t *testing.T, pool *pgxpool.Pool) *APIHarness {
	t.Helper()

	issuer := auth.NewTokenIssuer("harness-secret", 15*time.Minute, 7*24*time.Hour)
	authRepo := auth.NewRepo(pool)

	projectsSvc := projects.NewService(projects.NewRepo(pool))
	pubSvc := publications.NewService(publications.NewRepo(pool))
	ordersSvc := orders.NewService(orders.NewRepo(pool))

	srv := httpapi.NewRouter(httpapi.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		TokenIssuer: issuer,
		AuthRepo:    authRepo,
		// Реальный чекер отзыва: иначе тест не заметит, если ручка станет
		// принимать токен, выписанный до смены пароля.
		AuthRevocation: authRepo,
		Projects:       projects.NewHandler(projectsSvc),
		Publications:   publications.NewHandler(pubSvc),
		Orders:         orders.NewHandler(ordersSvc),
		Billing:        billing.NewHandler(billing.NewService(billing.NewRepo(pool))),
	})
	return &APIHarness{Pool: pool, srv: srv, issuer: issuer}
}

// token — access-токен пользователя, как после входа.
func (h *APIHarness) Token(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	return h.TokenAt(t, userID, time.Now())
}

// TokenAt — токен, выписанный в указанный момент. Нужен там, где
// проверяется отзыв: он должен быть выдан ДО смены пароля, иначе тест
// проверяет срок жизни, а не отзыв.
func (h *APIHarness) TokenAt(t *testing.T, userID uuid.UUID, at time.Time) string {
	t.Helper()
	pair, err := h.issuer.Issue(userID, at)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return pair.Access
}

// do — запрос к роутеру. token == "" означает «без заголовка Authorization».
func (h *APIHarness) Do(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// DoRaw — запрос, ответ которого не JSON: выгрузка CSV, редирект, файл.
// Do разбирает тело как объект и для таких ответов не годится.
func (h *APIHarness) DoRaw(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

// UserOpts — каким получится пользователь. Ноль-значение = обычный
// заказчик: активный и одобренный.
type UserOpts struct {
	Kind       string // client | specialist | both
	IsManager  bool
	IsAdmin    bool
	NotActive  bool
	NotApprove bool
}

// newUser — пользователь в БД плюс функция уборки.
func (h *APIHarness) NewUser(t *testing.T, o UserOpts) (uuid.UUID, func()) {
	t.Helper()
	if o.Kind == "" {
		o.Kind = "client"
	}
	ctx := context.Background()
	var id uuid.UUID
	err := h.Pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_manager, is_admin,
                   is_approved, is_active, email_verified_at)
VALUES ($1, 'x', $2, $3, $4, $5, $6, now())
RETURNING id`,
		"harness-"+uuid.NewString()+"@example.com", o.Kind,
		o.IsManager, o.IsAdmin, !o.NotApprove, !o.NotActive).Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id, func() { _, _ = h.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id) }
}
