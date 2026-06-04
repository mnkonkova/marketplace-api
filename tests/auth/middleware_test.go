package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/auth"
)

// ---- TokenIssuer ----

func TestTokenIssuer_IssueAndParse_Access(t *testing.T) {
	issuer := auth.NewTokenIssuer("supersecret", 15*time.Minute, 7*24*time.Hour)
	uid := uuid.New()

	pair, err := issuer.Issue(uid, time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if pair.Access == "" || pair.Refresh == "" {
		t.Fatalf("empty tokens: %+v", pair)
	}

	c, err := issuer.Parse(pair.Access, auth.TokenAccess)
	if err != nil {
		t.Fatalf("Parse access: %v", err)
	}
	if c.UserID != uid {
		t.Errorf("user_id: want %s, got %s", uid, c.UserID)
	}
	if c.Kind != auth.TokenAccess {
		t.Errorf("kind: want access, got %s", c.Kind)
	}
}

func TestTokenIssuer_RefreshSeparateFromAccess(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	pair, _ := issuer.Issue(uuid.New(), time.Now())

	// access нельзя парсить как refresh
	if _, err := issuer.Parse(pair.Access, auth.TokenRefresh); !errors.Is(err, auth.ErrWrongKind) {
		t.Errorf("want ErrWrongKind for access-as-refresh, got %v", err)
	}
	// refresh нельзя парсить как access
	if _, err := issuer.Parse(pair.Refresh, auth.TokenAccess); !errors.Is(err, auth.ErrWrongKind) {
		t.Errorf("want ErrWrongKind for refresh-as-access, got %v", err)
	}
}

func TestTokenIssuer_RejectsExpired(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	// Выписан час назад с TTL 1 минута — давно протух.
	pair, _ := issuer.Issue(uuid.New(), time.Now().Add(-time.Hour))
	if _, err := issuer.Parse(pair.Access, auth.TokenAccess); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken for expired, got %v", err)
	}
}

func TestTokenIssuer_RejectsTamperedSignature(t *testing.T) {
	a := auth.NewTokenIssuer("secret-a", time.Minute, time.Hour)
	b := auth.NewTokenIssuer("secret-b", time.Minute, time.Hour) // другой секрет

	pair, _ := a.Issue(uuid.New(), time.Now())
	if _, err := b.Parse(pair.Access, auth.TokenAccess); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("want ErrInvalidToken for cross-secret, got %v", err)
	}
}

func TestTokenIssuer_RejectsMalformed(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	cases := []string{
		"",
		"not-a-jwt",
		"header.payload.signature.extra",
		"a.b.c", // valid format но не валидная подпись
	}
	for _, tok := range cases {
		if _, err := issuer.Parse(tok, auth.TokenAccess); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("want ErrInvalidToken for %q, got %v", tok, err)
		}
	}
}

// ---- Middleware ----

func TestMiddleware_MissingBearer_401(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	h := auth.Middleware(issuer)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler не должен быть вызван без auth")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/protected", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing_bearer") {
		t.Errorf("body should mention missing_bearer: %s", rec.Body.String())
	}
}

func TestMiddleware_InvalidToken_401(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	h := auth.Middleware(issuer)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler не должен быть вызван при invalid token")
	}))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", rec.Code)
	}
}

func TestMiddleware_ValidToken_SetsUserID(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	uid := uuid.New()
	pair, _ := issuer.Issue(uid, time.Now())

	var seenUID uuid.UUID
	h := auth.Middleware(issuer)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok := auth.UserIDFrom(r.Context())
		if !ok {
			t.Error("UserID не в контексте")
			return
		}
		seenUID = got
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+pair.Access)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: want 200, got %d", rec.Code)
	}
	if seenUID != uid {
		t.Errorf("user_id в контексте: want %s, got %s", uid, seenUID)
	}
}

func TestMiddleware_RefreshTokenAsAccess_Rejected(t *testing.T) {
	issuer := auth.NewTokenIssuer("s", time.Minute, time.Hour)
	pair, _ := issuer.Issue(uuid.New(), time.Now())

	h := auth.Middleware(issuer)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler не должен принимать refresh-token")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+pair.Refresh)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401 for refresh as access, got %d", rec.Code)
	}
}

// ---- RequireRoles ----

// stubLoader реализует IdentityLoader для тестов RequireRoles.
type stubLoader struct {
	role       string
	isApproved bool
	isActive   bool
	err        error
}

func (s stubLoader) LoadIdentity(_ context.Context, _ uuid.UUID) (string, bool, bool, error) {
	return s.role, s.isApproved, s.isActive, s.err
}

func runWithRole(t *testing.T, loader auth.IdentityLoader, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	called := false
	h := auth.RequireRoles(loader, roles...)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := auth.WithUserID(req.Context(), uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	t.Logf("handler called=%v", called)
	return rec
}

func TestRequireRoles_NoUserID_401(t *testing.T) {
	loader := stubLoader{role: "admin", isActive: true, isApproved: true}
	h := auth.RequireRoles(loader, "admin")(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler не должен вызываться без user_id")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", rec.Code)
	}
}

func TestRequireRoles_WrongRole_403(t *testing.T) {
	loader := stubLoader{role: "client", isActive: true, isApproved: true}
	rec := runWithRole(t, loader, "admin")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: want 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden_role") {
		t.Errorf("body should mention forbidden_role: %s", rec.Body.String())
	}
}

func TestRequireRoles_Inactive_403(t *testing.T) {
	loader := stubLoader{role: "admin", isActive: false, isApproved: true}
	rec := runWithRole(t, loader, "admin")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: want 403 for inactive, got %d", rec.Code)
	}
}

func TestRequireRoles_UnapprovedManager_403(t *testing.T) {
	loader := stubLoader{role: auth.RoleManager, isActive: true, isApproved: false}
	rec := runWithRole(t, loader, auth.RoleManager)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: want 403 for unapproved manager, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden_unapproved") {
		t.Errorf("body should mention forbidden_unapproved: %s", rec.Body.String())
	}
}

func TestRequireRoles_ApprovedManager_OK(t *testing.T) {
	loader := stubLoader{role: auth.RoleManager, isActive: true, isApproved: true}
	called := false
	h := auth.RequireRoles(loader, auth.RoleManager)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if role, _ := auth.RoleFrom(r.Context()); role != auth.RoleManager {
			t.Errorf("role в контексте: want manager, got %s", role)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := auth.WithUserID(req.Context(), uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if !called {
		t.Errorf("approved manager должен пройти guard")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: want 200, got %d", rec.Code)
	}
}

func TestRequireRoles_AdminBypassesUnapprovedCheck(t *testing.T) {
	// is_approved=false должен влиять ТОЛЬКО на роль manager. Для admin/specialist/client — игнор.
	loader := stubLoader{role: "admin", isActive: true, isApproved: false}
	called := false
	h := auth.RequireRoles(loader, "admin")(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := auth.WithUserID(req.Context(), uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("admin с is_approved=false должен пройти (статус %d, called=%v)", rec.Code, called)
	}
}
