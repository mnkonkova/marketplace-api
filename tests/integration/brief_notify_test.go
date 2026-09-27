package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Задание креатора изменилось — креатор об этом узнаёт.
//
// Механизм был написан целиком: вид уведомления, журнал с дедупом по
// дню, событие в outbox. Не работало ровно одно место — условие
// «материал для креаторов»: в коде стояло `audience == "creator" ||
// "all"`, а легальные значения — `creators` и `client` (CHECK в
// migrations/00036_materials_autoping.sql). Условие не выполнялось
// НИКОГДА, и уведомление не ушло ни разу.
//
// Поймать это можно было только так — проверкой следа, а не вызова:
// функция notifyBrief честно работала, её просто не звали. Поэтому тест
// смотрит в journal и в outbox, а не в моки.
//
// И второй след той же ошибки: счётчик материалов в самом сообщении
// («в задании три материала») считался тем же выдуманным значением и
// приезжал нулём при полном проекте материалов.

// briefEvents — события задания по проекту, свежие сверху.
func briefEvents(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, kind string) []publications.BriefUpdate {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT payload FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = $2
ORDER BY created_at DESC`, projectID.String(), "project."+kind)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()

	out := make([]publications.BriefUpdate, 0, 2)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		var u publications.BriefUpdate
		if err := json.Unmarshal(raw, &u); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		out = append(out, u)
	}
	return out
}

// logged — сколько строк журнала по этому виду и проекту.
func logged(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM notification_log WHERE kind = $1 AND subject_id = $2`,
		kind, projectID).Scan(&n); err != nil {
		t.Fatalf("read notification log: %v", err)
	}
	return n
}

func TestMaterialForCreatorsNotifiesThem(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	if _, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialDoc, Title: "Бренд-гайд",
		URL: "https://cdn.example.com/guide.pdf", CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("add material: %v", err)
	}

	// В составе двое — строка журнала на каждого: дедуп идёт по
	// человеку, а не по проекту.
	if n := logged(t, pool, pid, publications.ReminderMaterialsUpdated); n != len(creators) {
		t.Fatalf("строк журнала %d, а в составе %d — уведомление не ушло", n, len(creators))
	}

	events := briefEvents(t, pool, pid, publications.ReminderMaterialsUpdated)
	if len(events) != 1 {
		t.Fatalf("событий об изменении задания: %d, ожидалось одно", len(events))
	}
	got := events[0]
	if len(got.CreatorIDs) != len(creators) {
		t.Errorf("получателей в событии %d, в составе %d", len(got.CreatorIDs), len(creators))
	}
	// Число в самом сообщении: «в задании один материал». Ноль здесь —
	// это тот же рассинхрон аудитории, только в SQL брифа.
	if got.Materials != 1 {
		t.Errorf("материалов в сообщении %d, а добавили один", got.Materials)
	}
}

func TestMaterialForClientLeavesCreatorsAlone(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	// Материал заказчику — это не изменение задания креатора, и будить
	// его нечем. Проверка ровно против «починили условие наотмашь».
	if _, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialLink, Title: "Готовый ролик",
		URL:      "https://disk.example.com/final.mp4",
		Audience: publications.AudienceClient, CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("add client material: %v", err)
	}

	if n := logged(t, pool, pid, publications.ReminderMaterialsUpdated); n != 0 {
		t.Errorf("креаторов разбудили материалом заказчика: строк журнала %d", n)
	}
}

func TestMaterialRemovedForCreatorsNotifiesThem(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	svc := publications.NewService(publications.NewRepo(pool))

	m, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialDoc, Title: "Старый гайд",
		URL: "https://cdn.example.com/old.pdf", CreatedBy: creators[0],
	})
	if err != nil {
		t.Fatalf("add material: %v", err)
	}
	// Добавление уже разбудило состав, и журнал гасит повтор в тот же
	// день — поэтому смотрим не на журнал, а на то, что удаление вообще
	// дошло до notifyBrief: событие об изменении задания одно, и после
	// удаления материалов в нём ноль.
	if err := svc.DeleteMaterial(ctx, pid, m.ID); err != nil {
		t.Fatalf("delete material: %v", err)
	}

	events := briefEvents(t, pool, pid, publications.ReminderMaterialsUpdated)
	if len(events) != 1 {
		t.Fatalf("событий %d: удаление в тот же день — та же новость, что и добавление", len(events))
	}
}

// Новому креатору задание приезжает приветствием — и только если оно
// есть. Счётчик материалов здесь из того же SQL, что и в брифе.
func TestNewCreatorGetsBriefWithMaterialCount(t *testing.T) {
	pool := integration.Pool(t)
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	ctx := context.Background()
	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)

	if _, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialDoc, Title: "Бренд-гайд",
		URL: "https://cdn.example.com/guide.pdf", CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("add material: %v", err)
	}

	var newbie uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"creator-"+uuid.NewString()+"@example.com").Scan(&newbie); err != nil {
		t.Fatalf("create creator: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, newbie) }()

	if err := repo.AddCreator(ctx, pid, newbie, creators[0]); err != nil {
		t.Fatalf("add creator: %v", err)
	}

	events := briefEvents(t, pool, pid, publications.ReminderCreatorBriefed)
	if len(events) != 1 {
		t.Fatalf("приветствий новому креатору: %d", len(events))
	}
	if got := events[0]; got.Materials != 1 {
		t.Errorf("в приветствии материалов %d, а в проекте один", got.Materials)
	}
	if len(events[0].CreatorIDs) != 1 || events[0].CreatorIDs[0] != newbie {
		t.Errorf("приветствие ушло не тому: %+v", events[0].CreatorIDs)
	}
}
