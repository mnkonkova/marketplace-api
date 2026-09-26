package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Заявка заказчика на следующий месяц.
//
// Прикидка «во сколько обойдётся месяц» была справочной: посмотрел и
// закрыл, а дальше человек шёл писать менеджеру словами. Кнопка
// «Заказать» превращает её в заявку — плашку у менеджера и сообщение в
// общий чат. Проверяем то, что ломается тихо:
//
//   - заявка ОДНА на проект: второе нажатие уточняет ту же просьбу, а
//     не заводит вторую. Иначе у менеджера копится стопка одинаковых
//     плашек, и разбирать он будет верхнюю;
//   - потолок хранится СНИМКОМ: разговор пойдёт о той сумме, которую
//     человеку показали, а прайс к тому времени может смениться;
//   - разобранная заявка гаснет, но остаётся историей разговора;
//   - чужой проект заявку не принимает.
func TestMonthRequestIsSingleAndUpdatable(t *testing.T) {
	pool := integration.Pool(t)
	pid, _, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	var client uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT client_user_id FROM projects WHERE id = $1`, pid).Scan(&client); err != nil {
		t.Fatalf("client of project: %v", err)
	}

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	first, err := svc.AskMonth(ctx, publications.AskMonthInput{
		ProjectID: pid, ClientID: client, Creators: 2, Videos: 30, Ceiling: 4_000_000,
	}, now)
	if err != nil {
		t.Fatalf("ask month: %v", err)
	}
	// Месяц не присылали — просят следующий.
	if got := first.Month.Format("2006-01"); got != "2026-10" {
		t.Errorf("месяц заявки %s, а просили следующий", got)
	}
	if first.Ceiling != 4_000_000 {
		t.Errorf("потолок не сохранён снимком: %d", first.Ceiling)
	}

	// Передумал: три креатора и шестьдесят роликов. Это та же просьба.
	second, err := svc.AskMonth(ctx, publications.AskMonthInput{
		ProjectID: pid, ClientID: client, Creators: 3, Videos: 60, Ceiling: 7_500_000,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ask month again: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("второе нажатие завело вторую заявку: %s и %s", first.ID, second.ID)
	}

	var open int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM project_month_requests WHERE project_id = $1 AND handled_at IS NULL`,
		pid).Scan(&open); err != nil {
		t.Fatalf("count: %v", err)
	}
	if open != 1 {
		t.Fatalf("открытых заявок %d, а должна быть одна", open)
	}

	got, err := svc.OpenMonthRequest(ctx, pid)
	if err != nil {
		t.Fatalf("open request: %v", err)
	}
	if got == nil {
		t.Fatal("карточка менеджера заявки не увидит: открытой нет")
	}
	if got.Creators != 3 || got.Videos != 60 || got.Ceiling != 7_500_000 {
		t.Errorf("заявка не обновилась: %+v", got)
	}

	// Событие в чат — ровно одно на обе просьбы: вторая переписала
	// первую, но сообщение о просьбе всё равно уходит.
	var events int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = 'project.client_month_request'`,
		pid.String()).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 2 {
		t.Errorf("событий о просьбе %d, а нажатий было два", events)
	}

	// Разобрали: плашка гаснет, строка остаётся.
	if err := svc.HandleMonthRequest(ctx, pid, client, now.Add(time.Hour)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	after, err := svc.OpenMonthRequest(ctx, pid)
	if err != nil {
		t.Fatalf("open after handle: %v", err)
	}
	if after != nil {
		t.Errorf("разобранная заявка всё ещё горит: %+v", after)
	}
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_month_requests WHERE project_id = $1`, pid).Scan(&total); err != nil {
		t.Fatalf("count all: %v", err)
	}
	if total != 1 {
		t.Errorf("история разговора потерялась: строк %d", total)
	}

	// Повторный разбор ничего не ломает: кнопку жмут дважды чаще, чем
	// кажется.
	if err := svc.HandleMonthRequest(ctx, pid, client, now.Add(2*time.Hour)); err != nil {
		t.Errorf("повторный разбор обязан быть безобидным: %v", err)
	}
}

func TestMonthRequestRejectsStranger(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	// Креатор проекта — не его заказчик, и заявку от его имени принимать
	// нельзя: плашка у менеджера означала бы просьбу, которой не было.
	if _, err := svc.AskMonth(ctx, publications.AskMonthInput{
		ProjectID: pid, ClientID: creators[0], Creators: 1, Videos: 10, Ceiling: 1_000_000,
	}, time.Now()); err == nil {
		t.Fatal("чужая заявка принята")
	}
}
