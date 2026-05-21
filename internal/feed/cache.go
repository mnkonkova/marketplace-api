package feed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache — обёртка над Redis для кэширования страниц /feed. Кэш разделён по
// сериализованному набору фильтров + курсору, поэтому next-page тоже кэшируется
// независимо. TTL короткий (десятки секунд), чтобы лента не отставала от
// реальных публикаций больше чем на TTL. nil-safe: если Cache=nil, Service
// просто молча мимо.
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewCache(rdb *redis.Client, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Cache{rdb: rdb, ttl: ttl}
}

func (c *Cache) Get(ctx context.Context, q Query) (Result, bool) {
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

func (c *Cache) Set(ctx context.Context, q Query, r Result) {
	if c == nil || c.rdb == nil {
		return
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, cacheKey(q), raw, c.ttl).Err()
}

func cacheKey(q Query) string {
	cats := append([]string(nil), q.Categories...)
	skills := append([]string(nil), q.SkillSlugs...)
	ids := make([]string, len(q.UserIDs))
	for i, id := range q.UserIDs {
		ids[i] = id.String()
	}
	sort.Strings(cats)
	sort.Strings(skills)
	sort.Strings(ids)
	key := struct {
		Q   string   `json:"q"`
		C   []string `json:"c"`
		S   []string `json:"s"`
		Ids []string `json:"i"`
		Ci  string   `json:"ci"`
		Ps  int      `json:"ps"`
		Cur string   `json:"cur"`
	}{q.Q, cats, skills, ids, q.City, q.PerSpecialist, q.Cursor}
	b, _ := json.Marshal(key)
	sum := sha256.Sum256(b)
	return "feed:cache:" + hex.EncodeToString(sum[:])
}

