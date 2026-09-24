package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Правка плана по одной строке.
//
// Пачкой ставят месяц вперёд, и это основной путь. Но дальше месяц
// живёт: креатор заболел и дату двигают, вместо выбывшего берут нового и
// ставят ему три дня, один день снимают совсем. До этих трёх ручек
// ничего из этого сделать было нельзя — только завести новую пачку из
// одного креатора и одной даты.
//
// Главное правило, которое здесь и проверяется: СДАННОЕ ПРАВКА ПЛАНА НЕ
// ТРОГАЕТ. У выкладки со ссылками ролик уже вышел, и «перенос» переписал
// бы историю периода задним числом.

func TestManagerAddPublicationOneDate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	day := pubDay(3)

	got, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		Day:           day,
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ManagerAddPublication: %v", err)
	}
	if got.Status != publications.StatusPlanned {
		t.Errorf("новая выкладка должна быть planned, а она %s", got.Status)
	}
	// Одиночная простановка — правка плана, а не пачка: «отменить пачку»
	// её задеть не должно.
	if got.BatchID != nil {
		t.Error("одиночная выкладка не должна принадлежать пачке")
	}
	if !got.DueDate.Equal(day) {
		t.Errorf("дата %s, ожидалась %s", got.DueDate, day)
	}

	// Второй раз на тот же день — отказ понятным словом, а не пятисоткой
	// из драйвера.
	_, err = svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		Day:           day,
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	})
	if !errors.Is(err, publications.ErrDayTaken) {
		t.Errorf("повтор дня: %v, ожидалось ErrDayTaken", err)
	}
}

func TestManagerAddPublicationRejectsOutsiderAndPast(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))

	// Задним числом выкладки не заводят: период считается по факту
	// выхода, и дата в прошлом чинила бы уже посчитанное.
	if _, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		Day:           pubDay(-1),
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	}); !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("дата в прошлом: %v, ожидалось ErrInvalidInput", err)
	}

	// Чужого креатора в проект через эту ручку не протащить.
	if _, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: uuid.New(),
		Day:           pubDay(2),
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	}); !errors.Is(err, publications.ErrCreatorNotInProject) {
		t.Errorf("чужой креатор: %v, ожидалось ErrCreatorNotInProject", err)
	}
}

func TestMoveDueDate(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	from, to := pubDay(3), pubDay(9)

	created, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID: projectID, CreatorUserID: creators[0], Day: from,
		ManagerUserID: creators[0], Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// Креатор попросил перенести — и просьба закрывается тем же
	// действием: менеджер на неё ответил делом.
	if _, err := svc.RequestDateChange(ctx, created.ID, creators[0], to, "заболел"); err != nil {
		t.Fatalf("date request: %v", err)
	}

	moved, err := svc.MoveDueDate(ctx, publications.MoveDueDateInput{
		PublicationID: created.ID,
		ManagerUserID: creators[0],
		Day:           to,
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("MoveDueDate: %v", err)
	}
	if !moved.DueDate.Equal(to) {
		t.Errorf("дата %s, ожидалась %s", moved.DueDate, to)
	}
	if moved.PendingDateRequest != nil {
		t.Error("просьба о переносе осталась висеть — креатору кажется, что его не услышали")
	}
}

func TestMoveDueDateRefusesSubmitted(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	created, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID: projectID, CreatorUserID: creators[0], Day: pubDay(1),
		ManagerUserID: creators[0], Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: created.ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/7300000000000000001"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Ролик уже вышел: двигать его дату — переписывать историю периода.
	if _, err := svc.MoveDueDate(ctx, publications.MoveDueDateInput{
		PublicationID: created.ID, ManagerUserID: creators[0],
		Day: pubDay(6), Now: time.Now().UTC(),
	}); !errors.Is(err, publications.ErrPublicationStarted) {
		t.Errorf("перенос сданной: %v, ожидалось ErrPublicationStarted", err)
	}

	// И снять её тоже нельзя: работа креатора не исчезает из-за правки
	// плана.
	if _, err := svc.CancelPublication(ctx, created.ID, creators[0], "передумали"); !errors.Is(
		err, publications.ErrPublicationStarted) {
		t.Errorf("снятие сданной: %v, ожидалось ErrPublicationStarted", err)
	}
}

func TestCancelPublication(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	created, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID: projectID, CreatorUserID: creators[0], Day: pubDay(4),
		ManagerUserID: creators[0], Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := svc.CancelPublication(ctx, created.ID, creators[0], "перенесли съёмку")
	if err != nil {
		t.Fatalf("CancelPublication: %v", err)
	}
	if got.Status != publications.StatusCancelled {
		t.Errorf("статус %s, ожидался cancelled", got.Status)
	}
	if got.CloseReason != "перенесли съёмку" {
		t.Errorf("причина %q — по ней потом видно, почему в плане дыра", got.CloseReason)
	}

	// День освободился: на него можно поставить снова.
	if _, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID: projectID, CreatorUserID: creators[0], Day: pubDay(4),
		ManagerUserID: creators[0], Now: time.Now().UTC(),
	}); err != nil {
		t.Errorf("день после отмены должен быть свободен, а вышло: %v", err)
	}
}
