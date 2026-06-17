package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"marketpclce/internal/httpx"
	"marketpclce/internal/ratelimit"
)

func slogRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx, rl := httpx.WithReqLog(r.Context())
			r = r.WithContext(ctx)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			defer func() {
				args := []any{
					"method", r.Method,
					"path", r.URL.Path,
					"status", ww.Status(),
					"bytes", ww.BytesWritten(),
					"dur_ms", time.Since(start).Milliseconds(),
					"req_id", middleware.GetReqID(r.Context()),
					"remote_ip", ratelimit.ClientIP(r),
				}
				// user_id и reason пишут downstream-middleware (auth) и
				// handlers через httpx.SetReq*. Добавляем только если есть.
				if rl.UserID != "" {
					args = append(args, "user_id", rl.UserID)
				}
				if rl.Reason != "" {
					args = append(args, "reason", rl.Reason)
				}
				logger.Info("http", args...)
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

// CORS — простая allow-list реализация: разрешаем перечисленные origin'ы,
// отвечаем на preflight (OPTIONS) и пропускаем стандартные методы/заголовки
// (Authorization для JWT в т.ч.). Если allowed пуст — middleware no-op и
// CORS-заголовки не выставляются (фронт обслуживается с того же домена).
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	allowAll := false
	for _, o := range allowed {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
		}
		set[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(set) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin != "" {
				_, exact := set[origin]
				switch {
				case exact:
					// Origin в явном allow-list — можно отражать с credentials,
					// JWT в Authorization и куки уезжают cross-origin безопасно.
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				case allowAll:
					// Wildcard "*" в CORS_ORIGINS: ставим `*` без credentials.
					// Браузер сам отбросит fetch(credentials:'include') на `*`,
					// то есть аутентифицированные ручки остаются недоступны для
					// произвольных origin'ов. До этого здесь отражался любой
					// Origin вместе с Allow-Credentials:true — это снимало SOP
					// для JWT-кабинета (data-sec D2).
					w.Header().Set("Access-Control-Allow-Origin", "*")
				default:
					// Origin не подходит — заголовков не ставим, браузер
					// заблокирует ответ. OPTIONS всё равно ниже отдаём 204.
				}
				if exact || allowAll {
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")
					w.Header().Set("Access-Control-Max-Age", "600")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
