package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	Addr     string
	Password string
	DB       int
}

func New(ctx context.Context, c Config) (*redis.Client, error) {
	cli := redis.NewClient(&redis.Options{
		Addr:        c.Addr,
		Password:    c.Password,
		DB:          c.DB,
		DialTimeout: 3 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := cli.Ping(pingCtx).Err(); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return cli, nil
}
