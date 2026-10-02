package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

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

// Пинг работает и по НЕПОЛНОЙ выкладке.
//
// Стояло `p.status = 'done'` — то есть «сданы все пять площадок». Полных
// сдач меньшинство, и из-за этого пинг молчал почти всегда: ролик вышел
// в двух местах, не пошёл, и узнавали об этом из отчёта в конце периода.
func TestWeakVideoPingCoversPartial(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	// Одна площадка из пяти — статус partial: ролик вышел, но сдан не
	// везде. Именно такие и составляют большинство.
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      pid,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{nextDue()},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID, ActorUserID: creators[0],
		URLs: []string{"https://www.tiktok.com/@a/video/partial01"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	published := time.Now().AddDate(0, 0, -publications.WeakVideoAfterDays)
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx, `
UPDATE publication_links
SET published_at = $2, submitted_at = $2, last_collected_at = now()
WHERE publication_id = $1 RETURNING id`, pubID, published).Scan(&linkID); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments)
VALUES ($1, CURRENT_DATE, 30, 1, 0)`, linkID); err != nil {
		t.Fatalf("замер: %v", err)
	}

	due, err := repo.DueWeakVideos(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	for _, w := range due {
		if w.PublicationID == pubID {
			return
		}
	}
	t.Fatal("неполная выкладка в выборку не попала — а ролик уже вышел и не пошёл")
}

// Выключатель автопинга гасит и этот вид тоже.
//
// Пинг существовал, но управлять им было нечем: ни тумблера, ни
// упоминания в настройках. Менеджеру проекта, где ролики заведомо
// малотиражные, оставалось отключать сводку целиком.
func TestWeakVideoPingRespectsAutopingToggle(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	pubID := seedPublicationViews(t, pid, creators[0], nextDue(), "weakoff01", 10, true)
	published := time.Now().AddDate(0, 0, -publications.WeakVideoAfterDays)
	if _, err := pool.Exec(ctx, `
UPDATE publication_links
SET published_at = $2, submitted_at = $2, last_collected_at = now()
WHERE publication_id = $1`, pubID, published); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}

	if _, err := repo.SaveReminderPrefs(ctx, publications.ReminderPrefs{
		ProjectID: pid, DueToday: true, Overdue: true, Incomplete: true,
		ManagerDigest: true, LowViews: false,
	}, creators[0]); err != nil {
		t.Fatalf("SaveReminderPrefs: %v", err)
	}

	due, err := repo.DueWeakVideos(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	for _, w := range due {
		if w.PublicationID == pubID {
			t.Fatal("пинг ушёл при выключенном автопинге")
		}
	}
}

// Старые ролики не окликаем: окно, а не «всё, что старше двух суток».
//
// Пинг смотрит в окно от двух до семи дней. Без верхней границы первый
// же проход на живом проекте разослал бы письма по всей истории —
// сорок «у тебя ноль» за одно утро, часть полугодовой давности.
func TestWeakVideoPingHasHorizon(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	pubID := seedPublicationViews(t, pid, creators[0], nextDue(), "oldweak01", 5, true)
	old := time.Now().AddDate(0, 0, -(publications.WeakVideoHorizonDays + 3))
	if _, err := pool.Exec(ctx, `
UPDATE publication_links
SET published_at = $2, submitted_at = $2, last_collected_at = now()
WHERE publication_id = $1`, pubID, old); err != nil {
		t.Fatalf("дата публикации: %v", err)
	}

	due, err := repo.DueWeakVideos(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("выборка: %v", err)
	}
	for _, w := range due {
		if w.PublicationID == pubID {
			t.Fatal("окликнули ролик старше недели — переснимать его уже некому")
		}
	}
}
