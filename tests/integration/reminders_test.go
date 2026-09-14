package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// countOutbox — сколько событий такого типа лежит по проекту.
func countOutbox(t *testing.T, projectID uuid.UUID, eventType string) int {
	t.Helper()
	pool := integration.Pool(t)
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = $2`,
		projectID.String(), eventType).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// Главное требование М3: одно и то же напоминание не приходит дважды за
// день, даже если что-то перезапустилось. Проверяется повторным проходом.
func TestRemindersAreNotSentTwiceADay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0), pubDay(-2)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	now := time.Now().UTC()
	first, err := svc.RunReminders(ctx, now)
	if err != nil {
		t.Fatalf("RunReminders: %v", err)
	}
	if first.Considered != 2 || first.Sent != 2 {
		t.Fatalf("первый проход: рассмотрено %d, отправлено %d; ожидалось 2 и 2 (%+v)",
			first.Considered, first.Sent, first)
	}
	if first.Digests != 1 {
		t.Errorf("сводок %d, ожидалась одна на проект", first.Digests)
	}

	// Повторный проход — воркер перезапустился.
	second, err := svc.RunReminders(ctx, now)
	if err != nil {
		t.Fatalf("RunReminders (повтор): %v", err)
	}
	if second.Sent != 0 {
		t.Errorf("повторный проход отправил %d напоминаний, должен был ноль", second.Sent)
	}
	if second.Skipped != 2 {
		t.Errorf("пропущено %d, ожидалось 2", second.Skipped)
	}
	if second.Digests != 0 {
		t.Errorf("сводка ушла второй раз (%d)", second.Digests)
	}

	// В очереди тоже не должно быть дублей: событие пишется только когда
	// строка журнала действительно вставилась.
	if n := countOutbox(t, projectID, "project."+publications.ReminderDueToday); n != 1 {
		t.Errorf("событий due_today %d, ожидалось 1", n)
	}
	if n := countOutbox(t, projectID, "project."+publications.ReminderManagerDigest); n != 1 {
		t.Errorf("событий сводки %d, ожидалось 1", n)
	}
}

// Три ветки текста: срок сегодня, просрочено, вышло но не все площадки.
// Последняя важна отдельно: писать «вы просрочили» человеку, который уже
// выложил ролик, — неправильно.
func TestRemindersClassifyKinds(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0), pubDay(-3), pubDay(-1)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	// По третьей выкладке ролик вышел, но площадок меньше пяти.
	var partialID uuid.UUID
	for _, p := range res.Items {
		if p.DueDate.Equal(pubDay(-1)) {
			partialID = p.ID
		}
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: partialID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/1", "https://youtu.be/dQw4w9WgXcQ"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	rems, err := repo.DueReminders(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("DueReminders: %v", err)
	}
	if len(rems) != 3 {
		t.Fatalf("напоминаний %d, ожидалось 3", len(rems))
	}

	byKind := map[string]publications.Reminder{}
	for _, r := range rems {
		byKind[r.Kind] = r
	}
	for _, want := range []string{
		publications.ReminderDueToday,
		publications.ReminderOverdue,
		publications.ReminderIncomplete,
	} {
		if _, ok := byKind[want]; !ok {
			t.Errorf("не нашлось напоминания вида %s", want)
		}
	}

	if inc, ok := byKind[publications.ReminderIncomplete]; ok {
		if len(inc.MissingPlatforms) != 3 {
			t.Errorf("не хватает %v, ожидалось три площадки", inc.MissingPlatforms)
		}
	}
	if od, ok := byKind[publications.ReminderOverdue]; ok && od.DaysOverdue != 3 {
		t.Errorf("просрочка %d дней, ожидалось 3", od.DaysOverdue)
	}
}

// Пока висит непринятая просьба о переносе, напоминания не идут: писать
// человеку, который уже предупредил, — способ научить его не предупреждать.
func TestRemindersSkipPendingDateRequest(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-2)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	before, err := svc.RunReminders(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("RunReminders: %v", err)
	}
	if before.Sent != 1 {
		t.Fatalf("до просьбы отправлено %d, ожидалось 1", before.Sent)
	}

	if _, err := svc.RequestDateChange(ctx, res.Items[0].ID, creators[0],
		pubDay(3), "съёмка сорвалась"); err != nil {
		t.Fatalf("RequestDateChange: %v", err)
	}

	// Следующий день: напоминание не должно уйти.
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	after, err := svc.RunReminders(ctx, tomorrow)
	if err != nil {
		t.Fatalf("RunReminders (завтра): %v", err)
	}
	if after.Considered != 0 {
		t.Errorf("с открытой просьбой о переносе рассмотрено %d выкладок, ожидалось 0", after.Considered)
	}
}

// Сводка в общий чат: одна на проект в день, с разбивкой и списком тех,
// по кому горит.
func TestRemindersDigestCountsPerProject(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators,
		Dates:          []time.Time{pubDay(0), pubDay(-4)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	rems, err := repo.DueReminders(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("DueReminders: %v", err)
	}
	digests, err := repo.Digests(ctx, rems)
	if err != nil {
		t.Fatalf("Digests: %v", err)
	}
	if len(digests) != 1 {
		t.Fatalf("сводок %d, ожидалась одна на проект", len(digests))
	}
	d := digests[0]
	if d.DueToday != 2 || d.Overdue != 2 {
		t.Errorf("в сводке сегодня %d и просрочено %d, ожидалось 2 и 2", d.DueToday, d.Overdue)
	}
	if len(d.Creators) != 2 {
		t.Errorf("в сводке %d креаторов, ожидалось 2 — «по кому горит»", len(d.Creators))
	}
	if d.ProjectTitle == "" {
		t.Error("в сводке нет названия проекта — менеджер не поймёт, о чём речь")
	}
}

// Ручное напоминание менеджера не должно гаситься дедупом уже ушедшего
// автоматического — но и нажимать кнопку двадцать раз подряд нельзя.
func TestRemindNowIsSeparateFromAutomatic(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID
	now := time.Now().UTC()

	if _, err := svc.RunReminders(ctx, now); err != nil {
		t.Fatalf("RunReminders: %v", err)
	}

	sent, err := svc.RemindNow(ctx, pubID, now)
	if err != nil {
		t.Fatalf("RemindNow: %v", err)
	}
	if !sent {
		t.Fatal("ручное напоминание погасло дедупом автоматического")
	}

	again, err := svc.RemindNow(ctx, pubID, now)
	if err != nil {
		t.Fatalf("RemindNow (повтор): %v", err)
	}
	if again {
		t.Error("вторая кнопка в тот же день ушла — креатор получит два одинаковых сообщения")
	}
}

// Выкладка, по которой всё сдано, из напоминаний уходит.
func TestRemindersStopAfterFullSubmit(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(-1)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: res.Items[0].ID,
		ActorUserID:   creators[0],
		URLs: []string{
			"https://www.tiktok.com/@u/video/1",
			"https://youtu.be/dQw4w9WgXcQ",
			"https://www.instagram.com/reel/C8xYzAbCdEf/",
			"https://vk.com/clip-99_11",
			"https://likee.video/v/AbCdEf",
		},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	rems, err := repo.DueReminders(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("DueReminders: %v", err)
	}
	if len(rems) != 0 {
		t.Errorf("по закрытой выкладке всё ещё %d напоминаний", len(rems))
	}
}

// Выкладка остаётся planned, пока её кто-то не закроет. Без горизонта
// выборка росла бы вечно, а креатор получал бы напоминание о дедлайне
// полугодовой давности каждый день.
func TestRemindersStopAfterOverdueHorizon(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	horizon := publications.OverdueHorizonDays
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates: []time.Time{
			pubDay(-(horizon - 1)), // в горизонте
			pubDay(-(horizon + 5)), // давно за горизонтом
		},
		CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

	rems, err := repo.DueReminders(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("DueReminders: %v", err)
	}
	if len(rems) != 1 {
		t.Fatalf("напоминаний %d, ожидалось 1: за горизонтом напоминать бессмысленно", len(rems))
	}
	if rems[0].DaysOverdue >= horizon {
		t.Errorf("в выборку попала выкладка с просрочкой %d дней, горизонт %d",
			rems[0].DaysOverdue, horizon)
	}
}
