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

	if got.DaySlot != 1 {
		t.Errorf("первый ролик дня: day_slot=%d, ожидался 1", got.DaySlot)
	}

	// Второй раз на тот же день — не отказ, а второй ролик: день съёмки
	// один, роликов из него выходит несколько, и раньше менеджеру
	// приходилось разносить их по датам, к работе отношения не имевшим.
	again, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		Day:           day,
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("второй ролик в тот же день: %v", err)
	}
	if again.DaySlot != 2 {
		t.Errorf("второй ролик дня: day_slot=%d, ожидался 2", again.DaySlot)
	}
	if again.ID == got.ID {
		t.Error("второй ролик вернулся тем же id — вставки не было")
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

// Несколько роликов в день.
//
// День съёмки один, роликов из него выходит три — и раньше план этого
// сказать не умел: уникальность стояла по паре «креатор + день», и
// менеджер разносил ролики по соседним датам, к работе отношения не
// имевшим. Отчёт после этого считал выработку по выдуманным дням.
//
// Теперь номер в дне — часть ключа, а пачка получает «сколько роликов в
// день». Главное свойство здесь и проверяется: ЧИСЛО — ЦЕЛЬ, А НЕ
// ПРИБАВКА. Повторная отправка той же формы (двойной клик, ретрай)
// приводит план к заданному числу, а не удваивает его.
func TestCreateBatchPerDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	day := pubDay(4)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{day},
		PerDay:         3,
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if res.Created != 3 {
		t.Fatalf("создано %d выкладок, ожидалось 3", res.Created)
	}
	slots := map[int]bool{}
	for _, p := range res.Items {
		slots[p.DaySlot] = true
		if !p.DueDate.Equal(day) {
			t.Errorf("дата %s, ожидалась %s", p.DueDate, day)
		}
	}
	for n := 1; n <= 3; n++ {
		if !slots[n] {
			t.Errorf("в дне нет ролика с номером %d: %v", n, slots)
		}
	}

	// Повтор той же пачки: план уже приведён к трём, добавлять нечего.
	again, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{day},
		PerDay:         3,
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("повторная пачка: %v", err)
	}
	if again.Created != 0 {
		t.Errorf("повтор добавил ещё %d выкладок — план растёт на каждое нажатие", again.Created)
	}

	// А увеличение числа догружает недостающие номера, не трогая
	// существующие: менеджер решил снимать по пять, а не по три.
	more, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{day},
		PerDay:         5,
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("пачка на пять: %v", err)
	}
	if more.Created != 2 {
		t.Errorf("догрузилось %d выкладок, ожидалось 2", more.Created)
	}
}

// Потолок дня. Десять роликов — уже не план, а опечатка в поле, и
// отказать на одиннадцатом дешевле, чем вычищать потом.
func TestAddPublicationRejectsFullDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	day := pubDay(5)

	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{day},
		PerDay:         10,
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("пачка на десять: %v", err)
	}

	_, err := svc.ManagerAddPublication(ctx, publications.AddPublicationInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		Day:           day,
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	})
	if !errors.Is(err, publications.ErrDayFull) {
		t.Errorf("одиннадцатый ролик дня: %v, ожидалось ErrDayFull", err)
	}

	// И в пачке потолок тот же, причём отказ приходит до записи.
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{pubDay(6)},
		PerDay:         11,
		CreatedBy:      creators[0],
	}); !errors.Is(err, publications.ErrInvalidInput) {
		t.Errorf("пачка на одиннадцать: %v, ожидалось ErrInvalidInput", err)
	}
}

// Перенос на занятый день кладёт ролик вторым номером, а не отказывает.
//
// Номер пересчитывается на новом месте: нести его с собой нельзя — на
// целевом дне второе место могло быть занято, а свободным оказаться
// первое.
func TestMoveDueDateOntoOccupiedDay(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	from, to := pubDay(7), pubDay(8)

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators[:1],
		Dates:          []time.Time{from, to},
		CreatedBy:      creators[0],
	})
	if err != nil || res.Created != 2 {
		t.Fatalf("CreateBatch: created=%d err=%v", res.Created, err)
	}
	var moving uuid.UUID
	for _, p := range res.Items {
		if p.DueDate.Equal(from) {
			moving = p.ID
		}
	}

	got, err := svc.MoveDueDate(ctx, publications.MoveDueDateInput{
		PublicationID: moving,
		Day:           to,
		ManagerUserID: creators[0],
		Now:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("MoveDueDate на занятый день: %v", err)
	}
	if !got.DueDate.Equal(to) {
		t.Errorf("дата %s, ожидалась %s", got.DueDate, to)
	}
	if got.DaySlot != 2 {
		t.Errorf("перенесённый ролик: day_slot=%d, ожидался 2", got.DaySlot)
	}
}
