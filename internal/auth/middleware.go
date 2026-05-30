package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type ctxKey int

const (
	userCtxKey ctxKey = 1
	// actorCtxKey хранит метку «человек или service-account» для будущих
	// аудит-логов. service-token путь выставляет actor=service, обычный
	// access-токен — actor=user.
	actorCtxKey ctxKey = 2
)

const (
	ActorUser    = "user"
	ActorService = "service"
)

func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userCtxKey, id)
}

func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userCtxKey).(uuid.UUID)
	return id, ok
}

// ActorTypeFrom — "user" или "service" (см. константы). Пусто = middleware не запускалось.
func ActorTypeFrom(ctx context.Context) string {
	s, _ := ctx.Value(actorCtxKey).(string)
	return s
}

func withActorType(ctx context.Context, t string) context.Context {
	return context.WithValue(ctx, actorCtxKey, t)
}

// RoleLookup — узкий интерфейс, чтобы RequireAdminOrModerator не зависел от
// auth.Repo напрямую. Реализация в проде — auth.Repo.GetRole.
type RoleLookup interface {
	GetRole(ctx context.Context, userID uuid.UUID) (string, error)
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
			ctx := withActorType(WithUserID(r.Context(), c.UserID), ActorUser)
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
			ctx := withActorType(WithUserID(r.Context(), c.UserID), ActorUser)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRoles — middleware для /admin/*. Принимает либо access-токен юзера
// (роль резолвится из users.role через RoleLookup), либо service-токен
// (роль берётся из claim Role, БД не дергаем — service-аккаунт это просто
// машина с фиксированной ролью). Список allowed — итоговые роли, которые
// пропускаем (обычно "admin", "moderator").
//
// 401 — нет токена / битый токен.
// 403 — токен валиден, но роль не в allow-list.
func RequireRoles(issuer *TokenIssuer, lookup RoleLookup, allowed ...string) func(http.Handler) http.Handler {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, r := range allowed {
		allowedSet[r] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				http.Error(w, `{"error":"missing_bearer"}`, http.StatusUnauthorized)
				return
			}
			token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
			claims, err := issuer.ParseAny(token, TokenAccess, TokenService)
			if err != nil {
				http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
				return
			}

			var role, actor string
			switch claims.Kind {
			case TokenService:
				if !claims.IsService {
					// Чужой kind/claim mismatch — параноидальный chk.
					http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
					return
				}
				role = claims.Role
				actor = ActorService
			case TokenAccess:
				if lookup == nil {
					// Конфиг ошибка: middleware подключили без репо. Лучше 500,
					// чем молча пропускать.
					http.Error(w, `{"error":"role_lookup_unavailable"}`, http.StatusInternalServerError)
					return
				}
				dbRole, lerr := lookup.GetRole(r.Context(), claims.UserID)
				if lerr != nil {
					http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
					return
				}
				role = dbRole
				actor = ActorUser
			default:
				http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
				return
			}

			if _, ok := allowedSet[role]; !ok {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}

			ctx := withActorType(WithUserID(r.Context(), claims.UserID), actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
