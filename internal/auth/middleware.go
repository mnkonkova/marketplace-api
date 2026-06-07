package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// RevocationChecker — точка отзыва access-токенов. Реализация (auth.Repo)
// сравнивает iat токена с users.password_changed_at: токен старше последней
// смены пароля считается отозванным.
//
// data-sec D8: до этого access жил до своего TTL независимо от reset'a
// пароля — украденный токен оставался валидным минуты-часы после reset'a.
// Refresh уже проверялся (см. service.go Refresh), но access нет.
type RevocationChecker interface {
	IsTokenRevoked(ctx context.Context, userID uuid.UUID, issuedAt time.Time) (bool, error)
}

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
				httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
				return
			}
			role, isApproved, isActive, err := repo.LoadIdentity(r.Context(), uid)
			if err != nil {
				httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
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

// Middleware — оригинальный конструктор без revocation-чекера. Сохранён
// для unit-тестов middleware (tests/auth/middleware_test.go), которые
// дёргают auth.Middleware(issuer) напрямую. В проде используется
// MiddlewareWithRevocation, см. router.go.
func Middleware(issuer *TokenIssuer) func(http.Handler) http.Handler {
	return MiddlewareWithRevocation(issuer, nil)
}

// MiddlewareWithRevocation — Middleware + проверка отзыва access-токена.
// rev==nil → проверка пропускается (поведение как у старого Middleware).
func MiddlewareWithRevocation(issuer *TokenIssuer, rev RevocationChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				httpx.WriteErrMsg(w, http.StatusUnauthorized, "missing_bearer", "Требуется авторизация")
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
			c, err := issuer.Parse(token, TokenAccess)
			if err != nil {
				httpx.WriteErrMsg(w, http.StatusUnauthorized, "invalid_token", "Сессия истекла — войдите снова")
				return
			}
			if rev != nil && c.IssuedAt != nil {
				revoked, rerr := rev.IsTokenRevoked(r.Context(), c.UserID, c.IssuedAt.Time)
				if rerr != nil || revoked {
					// На rerr тоже отвечаем 401, чтобы не пропустить
					// токен, отзыв которого мы не смогли проверить
					// (fail-closed). Чек идёт по индексу users.id PK —
					// при недоступности БД API всё равно нерабоч.
					httpx.WriteErrMsg(w, http.StatusUnauthorized, "invalid_token", "Сессия истекла — войдите снова")
					return
				}
			}
			ctx := WithUserID(r.Context(), c.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func OptionalMiddleware(issuer *TokenIssuer) func(http.Handler) http.Handler {
	return OptionalMiddlewareWithRevocation(issuer, nil)
}

// OptionalMiddlewareWithRevocation — Optional + проверка отзыва. Если
// токен отсутствует, пропускаем без проверки (как раньше).
func OptionalMiddlewareWithRevocation(issuer *TokenIssuer, rev RevocationChecker) func(http.Handler) http.Handler {
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
				httpx.WriteErrMsg(w, http.StatusUnauthorized, "invalid_token", "Сессия истекла — войдите снова")
				return
			}
			if rev != nil && c.IssuedAt != nil {
				revoked, rerr := rev.IsTokenRevoked(r.Context(), c.UserID, c.IssuedAt.Time)
				if rerr != nil || revoked {
					httpx.WriteErrMsg(w, http.StatusUnauthorized, "invalid_token", "Сессия истекла — войдите снова")
					return
				}
			}
			ctx := WithUserID(r.Context(), c.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
