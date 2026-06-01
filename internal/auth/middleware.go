package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

type ctxKey int

const (
	userCtxKey ctxKey = 1
	roleCtxKey ctxKey = 2
)

func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userCtxKey, id)
}

func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userCtxKey).(uuid.UUID)
	return id, ok
}

// WithRole — кладёт CRM-роль в контекст. Ставится RequireRoles после
// проверки. Удобно дальше в хендлерах различать ветки (manager видит все
// шаги, client — только visible_to_client).
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleCtxKey, role)
}

func RoleFrom(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(roleCtxKey).(string)
	return r, ok
}

// IdentityLoader — узкий интерфейс под RequireRoles. Реализуется auth.Repo.
type IdentityLoader interface {
	LoadIdentity(ctx context.Context, id uuid.UUID) (role string, isApproved bool, isActive bool, err error)
}

// RequireRoles — guard на CRM-роли. Должен идти ПОСЛЕ Middleware (читает
// UserID из контекста). Для manager дополнительно требует is_approved=true:
// зарегистрированный, но не аппрувленный, получает 403 forbidden_unapproved
// и не попадает в /manager/* до аппрува админом.
func RequireRoles(repo IdentityLoader, roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, ok := UserIDFrom(r.Context())
			if !ok {
				httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
				return
			}
			role, isApproved, isActive, err := repo.LoadIdentity(r.Context(), uid)
			if err != nil {
				httpx.WriteErr(w, http.StatusUnauthorized, "no_user")
				return
			}
			if !isActive {
				httpx.WriteErr(w, http.StatusForbidden, "inactive")
				return
			}
			if _, ok := allowed[role]; !ok {
				httpx.WriteErr(w, http.StatusForbidden, "forbidden_role")
				return
			}
			if role == RoleManager && !isApproved {
				httpx.WriteErrMsg(w, http.StatusForbidden, "forbidden_unapproved",
					"Менеджер ещё не аппрувлен админом — доступ к кабинету закрыт.")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithRole(r.Context(), role)))
		})
	}
}

func Middleware(issuer *TokenIssuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				http.Error(w, `{"error":"missing_bearer"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
			c, err := issuer.Parse(token, TokenAccess)
			if err != nil {
				http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
				return
			}
			ctx := WithUserID(r.Context(), c.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func OptionalMiddleware(issuer *TokenIssuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
			c, err := issuer.Parse(token, TokenAccess)
			if err != nil {
				http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
				return
			}
			ctx := WithUserID(r.Context(), c.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
