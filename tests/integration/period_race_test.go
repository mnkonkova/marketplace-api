package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"marketpclce/internal/billing"
	"marketpclce/tests/integration"
)

// Периоды заводятся ровно один раз, сколько бы запросов ни пришло разом.
//
// Между «посмотреть последний период» и «вставить следующий» в
// EnsurePeriods не было ничего, а на (project_id, seq) стоит уникальный
// индекс. Два запроса к проекту без периодов оба видели пусто, оба
// вставляли seq=1, и один падал на дубликате ключа — наружу это уходило
// пятисоткой. На живом стенде шесть параллельных запросов к экрану
// биллинга давали пять отказов из шести, и воспроизводилось это каждый
// раз.
//
// Случай не выдуманный: экран менеджера дёргает /billing и
// /billing/periods разом, а по тем же проектам ходит фоновый
// LockDuePeriods — встреча тикера с открытым экраном гарантирована.
//
// Тест бьёт по той же двери восемью руками сразу. Проверяет две вещи:
// ни один вызов не вернул ошибку И периодов завелось столько, сколько
// нужно, а не восемь копий первого.
func TestEnsurePeriodsSurvivesParallelCalls(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := billing.NewService(billing.NewRepo(pool))
	if _, err := svc.SaveTerms(ctx, demoTerms(pid), creators[0]); err != nil {
		t.Fatalf("terms: %v", err)
	}
	// Период отсчитывается от первой публикации — без неё материализовать
	// нечего и гонки не будет.
	seedPublicationViews(t, pid, creators[0], pubDay(0), "race1", 100_000, true)

	now := time.Now().UTC()
	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // стартуем все разом, иначе гонки не поймать
			_, errs[i] = svc.Periods(ctx, pid, now)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("вызов %d вернул ошибку: %v — на экране это пятисотка", i, err)
		}
	}

	var got int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_periods WHERE project_id = $1`, pid).Scan(&got); err != nil {
		t.Fatalf("счёт периодов: %v", err)
	}
	// Сколько именно — зависит от того, как давно вышел ролик; важно, что
	// не ноль и не восемь копий одного и того же.
	if got == 0 {
		t.Fatal("периодов не завелось вовсе")
	}
	var maxSeq int
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM project_periods WHERE project_id = $1`, pid).Scan(&maxSeq); err != nil {
		t.Fatalf("максимальный seq: %v", err)
	}
	if got != maxSeq {
		t.Errorf("периодов %d при максимальном номере %d — номера разъехались", got, maxSeq)
	}
}
