// Package integration — общие хелперы для тестов с реальной БД.
//
// Все тесты тут запускаются только если DATABASE_URL виден из env
// (через .env корня репо). Иначе t.Skip — таким образом CI без БД
// продолжает работать на unit-тестах.
//
// Изоляция: каждый тест берёт свой *pgx.Tx из общего pool и в конце
// делает Rollback. Любые INSERT/UPDATE откатываются, БД остаётся в
// исходном состоянии для следующего теста и для dev-окружения.
package integration

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
)

// Pool — общий pool на тесты. Создаётся лениво. Если DATABASE_URL пустой —
// возвращает nil; тесты должны проверить и сделать Skip.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		// .env лежит в корне marketplace-api — поднимаемся на 2 уровня.
		_ = godotenv.Load("../../.env")
		_ = godotenv.Load("../../../.env")
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return
		}
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("integration: pool init failed: %v", err)
			return
		}
		pool = p
	})
	if pool == nil {
		t.Skip("DATABASE_URL not set — integration tests skipped")
	}
	return pool
}
