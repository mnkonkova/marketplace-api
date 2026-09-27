package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/audit"
	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Передача дел и возврат проекта.
//
// Оба сценария про одно: админ делает необратимое действие, и цена
// ошибки — проект, о котором все забыли. Снятие роли с занятого
// менеджера оставляло проекты за тем, кто их больше не видит; отмена
// проекта не оставляла следа, по которому её можно было бы отменить.

// Снять роль с менеджера, на котором висят проекты, нельзя: сначала
// передайте их, и ответ говорит, какие именно.
func TestRevokeManagerWithActiveProjectsReturns409WithList(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	manager := s.user(t, userOpts{Kind: "client", IsManager: true})
	client := s.user(t, userOpts{Kind: "client"})
	first := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, AssignedToUserID: &manager, Title: "R2 передача 1",
	})
	second := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, AssignedToUserID: &manager, Title: "R2 передача 2",
	})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, manager.String())
	})

	code, body := s.post(t, "/api/v1/admin/managers/"+manager.String()+"/revoke", nil)
	if code != http.StatusConflict {
		t.Fatalf("снятие роли с занятого менеджера: код %d, ожидали 409 (тело %v)", code, body)
	}
	if body["error"] != "has_active_projects" {
		t.Errorf("код ошибки %v, ожидали has_active_projects", body["error"])
	}
	if msg, _ := body["message"].(string); msg == "" {
		t.Error("отказ без объяснения — админ не поймёт, что делать дальше")
	}
	listed := map[string]bool{}
	for _, raw := range list(t, body, "projects") {
		p, _ := raw.(map[string]any)
		id, _ := p["id"].(string)
		listed[id] = true
		if p["title"] == "" {
			t.Error("в списке проект без названия — по нему не решить, кому передавать")
		}
	}
	if !listed[first.String()] || !listed[second.String()] {
		t.Errorf("в 409 не оба проекта: %v", body["projects"])
	}

	// Роль осталась: отказ не должен быть «наполовину выполнен».
	if !isManager(t, pool, manager) {
		t.Fatal("роль снялась, несмотря на отказ")
	}
	if n := auditCount(t, pool, audit.ObjectUser, manager.String()); n != 0 {
		t.Errorf("отказ оставил %d записей в журнале", n)
	}
}

// Передача — одна транзакция и событие на каждый проект: n8n рассылает
// уведомления по проектам, и одно событие на пачку означало бы, что
// участники остальных ничего не узнали.
func TestTransferProjectsMovesEverythingAtOnce(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	from := s.user(t, userOpts{Kind: "client", IsManager: true})
	to := s.user(t, userOpts{Kind: "client", IsManager: true})
	client := s.user(t, userOpts{Kind: "client"})
	first := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, AssignedToUserID: &from, Title: "R2 сдача дел 1",
	})
	second := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, AssignedToUserID: &from, Title: "R2 сдача дел 2",
	})
	// Завершённый проект передавать не нужно: по нему нечего делать.
	done := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, AssignedToUserID: &from, Title: "R2 сдача дел (завершён)",
	})
	if _, err := pool.Exec(ctx, `UPDATE projects SET status = 'done' WHERE id = $1`, done); err != nil {
		t.Fatalf("завершить проект: %v", err)
	}
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, from.String())
	})

	// Принимающий не менеджер — не передаём ничего и ничего не меняем.
	outsider := s.user(t, userOpts{Kind: "client"})
	code, body := s.post(t, "/api/v1/admin/managers/"+from.String()+"/transfer_projects",
		map[string]any{"to_user_id": outsider.String()})
	if code != http.StatusBadRequest {
		t.Fatalf("передача постороннему: код %d, ожидали 400 (тело %v)", code, body)
	}
	if got := assignedTo(t, pool, first); got != from {
		t.Fatalf("отказ всё-таки передвинул проект: он у %s", got)
	}

	code, body = s.post(t, "/api/v1/admin/managers/"+from.String()+"/transfer_projects",
		map[string]any{"to_user_id": to.String()})
	if code != http.StatusOK {
		t.Fatalf("передача: код %d, тело %v", code, body)
	}
	if got := num(t, body, "transferred"); got != 2 {
		t.Fatalf("передано %d проектов, ожидали 2 (завершённый не в счёт): %v", got, body)
	}
	for _, id := range []uuid.UUID{first, second} {
		if got := assignedTo(t, pool, id); got != to {
			t.Errorf("проект %s остался у %s", id, got)
		}
		if n := outboxCount(t, pool, id, "project.assigned"); n != 1 {
			t.Errorf("по проекту %s событий о назначении %d, ожидали 1", id, n)
		}
		if n := auditCount(t, pool, audit.ObjectProject, id.String()); n != 1 {
			t.Errorf("по проекту %s записей журнала %d, ожидали 1", id, n)
		}
	}
	if got := assignedTo(t, pool, done); got != from {
		t.Errorf("завершённый проект передали зря: он у %s", got)
	}

	// Сдача дел читается в журнале как одно действие, а не только как
	// два одинаковых назначения.
	var batch int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM admin_audit_log
WHERE action = $1 AND object_id = $2`, audit.ActionProjectTransferBatch, from.String()).Scan(&batch); err != nil {
		t.Fatalf("считать журнал: %v", err)
	}
	if batch != 1 {
		t.Errorf("записей о сдаче дел %d, ожидали 1", batch)
	}

	// Освободившегося менеджера теперь можно отпустить.
	if code, body := s.post(t, "/api/v1/admin/managers/"+from.String()+"/revoke", nil); code != http.StatusNoContent {
		t.Fatalf("снятие роли после передачи: код %d, тело %v", code, body)
	}
	if isManager(t, pool, from) {
		t.Error("роль не снялась")
	}

	// Передавать самому себе бессмысленно — это промах в списке.
	code, _ = s.post(t, "/api/v1/admin/managers/"+to.String()+"/transfer_projects",
		map[string]any{"to_user_id": to.String()})
	if code != http.StatusBadRequest {
		t.Errorf("передача самому себе: код %d, ожидали 400", code)
	}
}

// Возврат проекта ставит тот статус, который был до отмены. Он лежит
// только в payload события отмены — больше его взять негде.
func TestRestoreProjectReturnsStatusFromCancelEvent(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	client := s.user(t, userOpts{Kind: "client"})
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 возврат",
	})
	// Отменяем не черновик: возврат в draft ничего не доказывает — это
	// и так значение по умолчанию.
	if _, err := pool.Exec(ctx, `UPDATE projects SET status = 'on_hold' WHERE id = $1`, pid); err != nil {
		t.Fatalf("перевести в on_hold: %v", err)
	}

	code, body := s.h.Do(t, http.MethodDelete, "/api/v1/admin/projects/"+pid.String(), s.token,
		map[string]any{"reason": "клиент передумал"})
	if code != http.StatusNoContent {
		t.Fatalf("отмена: код %d, тело %v", code, body)
	}
	if got := projectStatus(t, pool, pid); got != "cancelled" {
		t.Fatalf("после отмены статус %q", got)
	}

	code, body = s.post(t, "/api/v1/admin/projects/"+pid.String()+"/restore", nil)
	if code != http.StatusOK {
		t.Fatalf("возврат: код %d, тело %v", code, body)
	}
	if body["status"] != "on_hold" {
		t.Errorf("вернули в статус %v, ожидали on_hold", body["status"])
	}
	if got := projectStatus(t, pool, pid); got != "on_hold" {
		t.Errorf("в базе статус %q, ожидали on_hold", got)
	}
	// Событие и outbox — как у отмены: иначе в чужих системах проект
	// останется отменённым.
	if n := eventCount(t, pool, pid, "project_restored"); n != 1 {
		t.Errorf("событий возврата %d, ожидали 1", n)
	}
	if n := outboxCount(t, pool, pid, "project.restored"); n != 1 {
		t.Errorf("записей в outbox %d, ожидали 1", n)
	}
	if n := auditCountAction(t, pool, audit.ActionProjectRestore, pid.String()); n != 1 {
		t.Errorf("записей в журнале %d, ожидали 1", n)
	}

	// Второй возврат нечего возвращать.
	code, body = s.post(t, "/api/v1/admin/projects/"+pid.String()+"/restore", nil)
	if code != http.StatusConflict || body["error"] != "not_cancelled" {
		t.Errorf("повторный возврат: код %d, ошибка %v — ожидали 409 not_cancelled", code, body["error"])
	}
}

// Если статус до отмены неизвестен (проект отменили мимо ручки), молча
// угадывать нельзя: не тот статус меняет, кого проект ждёт.
func TestRestoreRefusesWithoutCancelEvent(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	client := s.user(t, userOpts{Kind: "client"})
	pid := s.project(t, projects.StartProjectInput{
		ClientUserID: &client, Title: "R2 возврат без следа",
	})
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET status = 'cancelled' WHERE id = $1`, pid); err != nil {
		t.Fatalf("отменить мимо ручки: %v", err)
	}

	code, body := s.post(t, "/api/v1/admin/projects/"+pid.String()+"/restore", nil)
	if code != http.StatusConflict || body["error"] != "no_cancel_event" {
		t.Fatalf("возврат без события: код %d, ошибка %v — ожидали 409 no_cancel_event", code, body["error"])
	}
	if got := projectStatus(t, pool, pid); got != "cancelled" {
		t.Errorf("отказ изменил статус на %q", got)
	}
}

// ---- помощники ----

func isManager(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(),
		`SELECT is_manager FROM users WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("роль пользователя: %v", err)
	}
	return v
}

func assignedTo(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var id *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT assigned_to_user_id FROM projects WHERE id = $1`, projectID).Scan(&id); err != nil {
		t.Fatalf("ответственный по проекту: %v", err)
	}
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func projectStatus(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) string {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(),
		`SELECT status::text FROM projects WHERE id = $1`, projectID).Scan(&st); err != nil {
		t.Fatalf("статус проекта: %v", err)
	}
	return st
}

func eventCount(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM project_step_events WHERE project_id = $1 AND event_kind = $2`,
		projectID, kind).Scan(&n); err != nil {
		t.Fatalf("считать события: %v", err)
	}
	return n
}

func outboxCount(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox
		 WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = $2`,
		projectID.String(), eventType).Scan(&n); err != nil {
		t.Fatalf("считать outbox: %v", err)
	}
	return n
}

func auditCountAction(t *testing.T, pool *pgxpool.Pool, action, objectID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM admin_audit_log WHERE action = $1 AND object_id = $2`,
		action, objectID).Scan(&n); err != nil {
		t.Fatalf("считать журнал: %v", err)
	}
	return n
}
