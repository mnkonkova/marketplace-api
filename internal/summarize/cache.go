package summarize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"marketpclce/internal/search"
)

type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewCache(rdb *redis.Client, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Cache{rdb: rdb, ttl: ttl}
}

func (c *Cache) Get(ctx context.Context, q search.Query) (Result, bool) {
	if c == nil || c.rdb == nil {
		return Result{}, false
	}
	raw, err := c.rdb.Get(ctx, cacheKey(q)).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return Result{}, false
	}
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return Result{}, false
	}
	return r, true
}

func (c *Cache) Set(ctx context.Context, q search.Query, r Result) {
	if c == nil || c.rdb == nil {
		return
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, cacheKey(q), raw, c.ttl).Err()
}

func cacheKey(q search.Query) string {
	cats := append([]string(nil), q.Categories...)
	skills := append([]string(nil), q.SkillSlugs...)
	sort.Strings(cats)
	sort.Strings(skills)
	key := struct {
		Q          string   `json:"q"`
		Categories []string `json:"c"`
		Skills     []string `json:"s"`
		City       string   `json:"city"`
		RateMin    *int     `json:"rmin,omitempty"`
		RateMax    *int     `json:"rmax,omitempty"`
		Limit      int      `json:"lim"`
	}{q.Q, cats, skills, q.City, q.RateMin, q.RateMax, q.Limit}
	b, _ := json.Marshal(key)
	sum := sha256.Sum256(b)
	return "summarize:cache:" + hex.EncodeToString(sum[:])
}
