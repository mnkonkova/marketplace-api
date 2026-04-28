package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

type Window struct {
	Limit  int
	Period time.Duration
}

type Error struct {
	RetryAfter time.Duration
}

func (e *Error) Error() string { return "rate limited" }

func IsRateLimited(err error) (*Error, bool) {
	var rl *Error
	if errors.As(err, &rl) {
		return rl, true
	}
	return nil, false
}

const checkAndIncrLua = `
for i = 1, #KEYS do
  local limit = tonumber(ARGV[(i-1)*2 + 1])
  local cur = redis.call('GET', KEYS[i])
  if cur and tonumber(cur) >= limit then
    return i
  end
end
for i = 1, #KEYS do
  local ttl_ms = tonumber(ARGV[(i-1)*2 + 2])
  local cur = redis.call('INCR', KEYS[i])
  if cur == 1 then
    redis.call('PEXPIRE', KEYS[i], ttl_ms)
  end
end
return 0
`

type Limiter struct {
	rdb    *redis.Client
	script *redis.Script
}

func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, script: redis.NewScript(checkAndIncrLua)}
}

func (l *Limiter) Allow(ctx context.Context, scope, subject string, windows []Window) error {
	if len(windows) == 0 {
		return nil
	}
	keys := make([]string, len(windows))
	args := make([]any, 0, len(windows)*2)
	for i, w := range windows {
		keys[i] = fmt.Sprintf("rl:%s:%s:%d", scope, subject, int(w.Period.Seconds()))
		args = append(args, w.Limit, w.Period.Milliseconds())
	}
	res, err := l.script.Run(ctx, l.rdb, keys, args...).Int()
	if err != nil {
		return fmt.Errorf("rate limit eval: %w", err)
	}
	if res > 0 {
		return &Error{RetryAfter: windows[res-1].Period}
	}
	return nil
}

func ClientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
