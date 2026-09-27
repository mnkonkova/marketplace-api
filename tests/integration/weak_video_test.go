package integration_test

import (
	"context"
	"testing"
	"time"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Ролик, который не пошёл, спрашивают у креатора на вторые сутки.
//
// Не в первый день: ролик тогда только расходится, и ноль в нём ничего
// не значит. И один раз на ролик, а не каждый день: повторять «твой
// ролик не пошёл» ежедневно — это упрёк, а не напоминание.
func TestWeakVideoPingOnSecondDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	// Ролик вышел позавчера и набрал 40 просмотров — меньше сотни.
	published := time.Now().AddDate(0, 0, -publications.WeakVideoAfterDays)
	pubID := seedPublicationViews(t, pid, creators[0], nextDue(), "weak01", 40, true)
	if _, err := pool.Exec(ctx, `
UPDATE publication_links
SET published_at = $2, submitted_at = $2, last_collected_at = now()
WHERE publication_id = $1`, pubID, published); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}

	// Вчера — рано.
	early, err := repo.DueWeakVideos(ctx, time.Now().AddDate(0, 0, -1), 0)
	if err != nil {
		t.Fatalf("выборка (рано): %v", err)
	}
	for _, w := range early {
		if w.PublicationID == pubID {
			t.Fatal("спросили на первые сутки — ролик ещё расходится")
		}
	}

	due, err := repo.DueWeakVideos(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	var found *publications.WeakVideo
	for i := range due {
		if due[i].PublicationID == pubID {
			found = &due[i]
		}
	}
	if found == nil {
		t.Fatal("ролик с 40 просмотрами на вторые сутки не попал в выборку")
	}
	if found.Kind() != publications.ReminderWeakVideo {
		t.Errorf("вид напоминания %q, ожидался «мало просмотров»", found.Kind())
	}

	sent, err := svc.RunWeakVideos(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if sent == 0 {
		t.Fatal("ничего не отправлено")
	}

	// Второй проход в тот же день и на следующий — молчит.
	again, err := svc.RunWeakVideos(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if again != 0 {
		t.Errorf("повторно отправлено %d — должно быть один раз на ролик", again)
	}
}
