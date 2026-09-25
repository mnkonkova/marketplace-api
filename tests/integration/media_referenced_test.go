package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/profiles"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Подметальщик S3 сносит из бакета всё, на что в базе нет ссылки, —
// поэтому список ссылок обязан быть полным.
//
// Стоимость ошибки здесь несимметрична: лишняя строка в списке означает
// «файл полежит дольше», пропущенная — «файл удалён навсегда, и никто
// не узнает, почему у проекта пропал бриф». Отдельного признака «наш
// файл» у объекта в бакете нет: единственный способ отличить нужное от
// мусора — этот запрос.
//
// Проверяем не текст SQL, а поведение: положили строку в таблицу с
// медиа — её адрес обязан оказаться в списке. Так тест ловит и новую
// таблицу, про которую забыли, если её добавят рядом.
func TestReferencedMediaCoversProjectMaterials(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	// Адрес НАШ — из бакета: именно такие файлы подметальщик и удаляет.
	// Внешняя ссылка ничего бы не доказала — KeyFromURL отсекает её
	// раньше сравнения, и тест был бы зелёным при пустом запросе.
	const url = "https://storage.yandexcloud.net/bucket/images/brief-" +
		"7f0d8d0e-0a7a-4f26-9a4a-f2f2f2f2f2f2.pdf"

	if _, err := publications.NewService(publications.NewRepo(pool)).AddMaterial(ctx,
		publications.AddMaterialInput{
			ProjectID: pid, Kind: publications.MaterialDoc, Title: "Бриф",
			URL: url, CreatedBy: creators[0],
		}); err != nil {
		t.Fatalf("add material: %v", err)
	}

	urls, err := profiles.NewRepo(pool).LoadReferencedMediaURLs(ctx)
	if err != nil {
		t.Fatalf("load referenced: %v", err)
	}
	for _, u := range urls {
		if u == url {
			return
		}
	}
	t.Fatalf("материал проекта не попал в список ссылок — подметальщик снесёт его через S3_ORPHAN_MIN_AGE (всего ссылок: %d)", len(urls))
}

// И контрольный: портфолио, ради которого запрос и писался, из него не
// выпало. Список общий, и правка «добавим свою таблицу» ломает соседей
// молча — UNION ALL прощает и лишнюю, и недостающую ветку.
func TestReferencedMediaStillCoversPortfolio(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"media-"+uuid.NewString()+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID) }()

	const avatar = "https://storage.yandexcloud.net/bucket/images/ava-1.jpg"
	const video = "https://storage.yandexcloud.net/bucket/portfolio/clip-1.mp4"
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, avatar_url)
VALUES ($1, 'Медиа', $2)
ON CONFLICT (user_id) DO UPDATE SET avatar_url = EXCLUDED.avatar_url`, userID, avatar); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO portfolio_items (user_id, title, video_url)
VALUES ($1, 'Ролик', $2)`, userID, video); err != nil {
		t.Fatalf("portfolio: %v", err)
	}

	urls, err := profiles.NewRepo(pool).LoadReferencedMediaURLs(ctx)
	if err != nil {
		t.Fatalf("load referenced: %v", err)
	}
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		seen[u] = true
	}
	for _, want := range []string{avatar, video} {
		if !seen[want] {
			t.Errorf("из списка выпал %s", want)
		}
	}
}
