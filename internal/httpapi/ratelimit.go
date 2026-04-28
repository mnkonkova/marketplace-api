package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"

	"marketpclce/internal/ratelimit"
)

func RateLimit(rl *ratelimit.Limiter, scope string, windows []ratelimit.Window) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rl == nil || len(windows) == 0 {
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
				slog.Error("rate limit eval", "scope", scope, "err", err)
			}
			next.ServeHTTP(w, r)
		})
	}
}
