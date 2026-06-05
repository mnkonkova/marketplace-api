package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"

	"marketpclce/internal/ratelimit"
)

// P8: expensiveScopes — scope'ы, для которых при отсутствии лимитера
// (rl==nil, обычно при недоступности Redis) middleware fail-CLOSED'ит и
// отдаёт 503. Это endpoint'ы с высокой стоимостью побочного эффекта:
//   - summarize/clarify/profilecheck → LLM-billing (claude/deepseek $$$)
//   - leads → отправка писем спецам, потенциально дорогая рассылка
//   - auth → анти-брутфорс на login/register/redeem_invite
// Без этой защиты curl-bombing на summarize за минуту жжёт LLM-квоту.
// Для остальных scope'ов (read и т.п.) fail-OPEN: лучше неограниченный
// read чем 503 на каталоге, если Redis лёг.
var expensiveScopes = map[string]struct{}{
	"summarize": {},
	"clarify":   {},
	"leads":     {},
	"auth":      {},
}

func RateLimit(rl *ratelimit.Limiter, scope string, windows []ratelimit.Window) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rl == nil || len(windows) == 0 {
			if _, expensive := expensiveScopes[scope]; expensive {
				slog.Warn("ratelimit fail-closed: no limiter configured for expensive scope",
					"scope", scope)
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"rate_limit_unavailable","message":"Сервис временно недоступен. Попробуйте позже."}`))
				})
			}
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := rl.Allow(r.Context(), scope, ratelimit.ClientIP(r), windows)
			if rlErr, ok := ratelimit.IsRateLimited(err); ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(rlErr.RetryAfter.Seconds())))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
				return
			}
			if err != nil {
				// Redis вернул ошибку на конкретном запросе (не nil-limiter):
				// для expensive — fail-CLOSED, для остальных — log+continue.
				if _, expensive := expensiveScopes[scope]; expensive {
					slog.Warn("ratelimit fail-closed: limiter error on expensive scope",
						"scope", scope, "err", err)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"rate_limit_unavailable","message":"Сервис временно недоступен. Попробуйте позже."}`))
					return
				}
				slog.Error("rate limit eval", "scope", scope, "err", err)
			}
			next.ServeHTTP(w, r)
		})
	}
}
