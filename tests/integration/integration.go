// Package integration — общие хелперы для тестов с реальной БД.
//
// Тесты подключаются только к TEST_DATABASE_URL (не к DATABASE_URL!).
// Это намеренное разделение: запуск `make test` или `go test ./...`
// без TEST_DATABASE_URL — пропускает integration-блок и НЕ трогает
// dev/prod БД. Раньше один и тот же DATABASE_URL шёл и в API, и в
// тесты — любой багнутый cleanup оставлял мусор в dev (см. историю
// 2026-06-04: 32 ChangeFunnel-воронки в dev-БД от ChangeFunnel-тестов).
//
// Как поднять test-БД локально (docker compose уже даёт postgres на
// localhost:5432 для dev; для тестов поднимаем отдельную БД В ТОМ ЖЕ
// контейнере):
//
//	docker exec marketplace-api-postgres-1 psql -U marketpclce -c \
//	  "CREATE DATABASE marketpclce_test;"
//	TEST_DATABASE_URL="postgres://marketpclce:<pass>@localhost:5432/marketpclce_test?sslmode=disable" \
//	  make migrate-up   # сначала прогнать миграции в test-БД
//	TEST_DATABASE_URL="...test..." make test
//
// В CI поднимается отдельный postgres-сервис, переменная задаётся в job env.
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

// Pool — общий pool для integration-тестов. Создаётся лениво ОДИН раз.
// Если TEST_DATABASE_URL не задан — возвращает nil; вызывающий тест
// должен сделать Skip.
//
// Безопасность: не использовать DATABASE_URL (это dev/prod-БД). Если
// случайно настроишь TEST_DATABASE_URL == DATABASE_URL — функция
// предупредит t.Log'ом и всё равно пропустит тесты, чтобы не было
// «упсики» с mess в dev. Чтобы прокинуть тот же DSN намеренно, ставь
// TEST_DATABASE_URL_ALLOW_DEV=1.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		// .env лежит в корне marketplace-api — поднимаемся на 2 уровня.
		_ = godotenv.Load("../../.env")
		_ = godotenv.Load("../../../.env")

		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			return
		}
		if devDSN := os.Getenv("DATABASE_URL"); devDSN != "" && devDSN == dsn &&
			os.Getenv("TEST_DATABASE_URL_ALLOW_DEV") != "1" {
			// Защита от dev-merge: тесты создают/удаляют объекты, дефолтная
			// dev-БД пострадает. Если ты ДЕЙСТВИТЕЛЬНО хочешь — ставь
			// TEST_DATABASE_URL_ALLOW_DEV=1 в env, тогда пройдёт.
			t.Logf("integration: TEST_DATABASE_URL == DATABASE_URL — small chance of trashing dev DB. " +
				"Set TEST_DATABASE_URL_ALLOW_DEV=1 to bypass.")
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
		t.Skip("TEST_DATABASE_URL not set (or равен DATABASE_URL без override) — integration tests skipped")
	}
	return pool
}
