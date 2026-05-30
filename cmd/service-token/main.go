// service-token — разовая генерация бессрочного service-токена для Directus
// и других server-to-server клиентов. Подписывается JWT_SECRET'ом, выпускает
// токен с kind=service, is_service=true, role=admin (по умолчанию) и UserID
// сервисного аккаунта из БД (users.email = directus-service@platform.internal).
//
// Использование (запускать локально на машине с доступом к БД и .env):
//
//	go run ./cmd/service-token
//	go run ./cmd/service-token -role moderator -email other-service@platform.internal
//
// Полученное значение положить в .env.prod как DIRECTUS_SERVICE_TOKEN.
// Никакой ротации в рантайме API не предусмотрено: для замены — выпустить
// новый и подменить значение env, перезапустить контейнеры.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"marketpclce/internal/auth"
	"marketpclce/internal/config"
)

func main() {
	role := flag.String("role", "admin", "роль для service-токена (admin|moderator)")
	email := flag.String("email", "directus-service@platform.internal",
		"email сервисного аккаунта в users (должен быть создан миграцией)")
	flag.Parse()

	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		fail("config", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fail("db pool", err)
	}
	defer pool.Close()

	var userID uuid.UUID
	err = pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, *email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		fail("user lookup",
			fmt.Errorf("service-account user %q не найден; примените миграцию 00010", *email))
	}
	if err != nil {
		fail("user lookup", err)
	}

	issuer := auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTAccessTTL, cfg.JWTRefreshTTL)
	tok, err := issuer.IssueService(userID, *role, time.Now())
	if err != nil {
		fail("issue", err)
	}

	fmt.Println(tok)
}

func fail(stage string, err error) {
	fmt.Fprintf(os.Stderr, "service-token: %s: %v\n", stage, err)
	os.Exit(1)
}
