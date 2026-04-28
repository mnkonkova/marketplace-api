package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type ctxKey int

const userCtxKey ctxKey = 1

func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userCtxKey, id)
}

func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userCtxKey).(uuid.UUID)
	return id, ok
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
