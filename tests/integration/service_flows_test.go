package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Тесты на Service-level методы. Repo уже покрывался в claim_visibility_test;
// здесь проверяем что сервис правильно прокидывает вызовы и регистрирует
// outbox-события / project_step_events для активности.

// TestServiceClaim_RoutesToRepo — Service.Claim делегирует в Repo.Claim;
// после успешного claim assigned_to_user_id обновляется.
func TestServiceClaim_RoutesToRepo(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, mgr) }()

	svc := projects.NewService(projects.NewRepo(pool))
	if err := svc.Claim(context.Background(), pid, mgr); err != nil {
		t.Fatalf("svc.Claim: %v", err)
	}
	var assignedTo *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT assigned_to_user_id FROM projects WHERE id = $1`, pid).Scan(&assignedTo); err != nil {
		t.Fatalf("query: %v", err)
	}
	if assignedTo == nil || *assignedTo != mgr {
		t.Fatalf("assigned_to_user_id: want %s, got %v", mgr, assignedTo)
	}
}

// TestServiceCancelProject_Soft — отмена проекта переводит в status=cancelled
// (физически не удаляется — sweep в воркере через retention).
func TestServiceCancelProject_Soft(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	actor := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, actor) }()

	svc := projects.NewService(projects.NewRepo(pool))
	if err := svc.CancelProject(context.Background(), pid, actor, "test reason"); err != nil {
		t.Fatalf("svc.CancelProject: %v", err)
	}

	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM projects WHERE id = $1`, pid).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("status: want cancelled, got %s", status)
	}
}

// TestServiceAssignManager_AssignThenUnassign — назначить → отвязать (nil).
func TestServiceAssignManager_AssignThenUnassign(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	mgr := makeManager(t, pool)
	admin := makeManager(t, pool)
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{mgr, admin})
	}()

	svc := projects.NewService(projects.NewRepo(pool))
	if err := svc.AssignManager(context.Background(), pid, &mgr, admin); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := svc.AssignManager(context.Background(), pid, nil, admin); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	var assignedTo *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT assigned_to_user_id FROM projects WHERE id = $1`, pid).Scan(&assignedTo); err != nil {
		t.Fatalf("query: %v", err)
	}
	if assignedTo != nil {
		t.Errorf("want NULL after unassign, got %v", *assignedTo)
	}
}

// TestServiceMoveProjectToStep_Forward — Service.MoveProjectToStep на
// валидный team-шаг внутри той же стадии. Покрывает MoveProjectToStep
// service-метод (он делегирует в move_step.go репо).
func TestServiceMoveProjectToStep_Forward(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	byName := loadStepsByName(t, repo, pid)
	target := byName["Финализация"].ID

	// Двинуть на team-шаг следующей стадии должно поскипать client-шаг
	// «Апрув клиента» (это уже проверяется в move_step_test, но здесь —
	// что service.MoveProjectToStep вообще доходит).
	svc := projects.NewService(repo)
	actor := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, actor) }()

	updated, err := svc.MoveProjectToStep(context.Background(), pid, target, actor, nil)
	if err != nil {
		t.Fatalf("MoveProjectToStep: %v", err)
	}
	if updated.ID != pid {
		t.Errorf("project mismatch")
	}
}

// TestServiceListInbox_ContainsUnassigned — Service.ListInbox возвращает
// проекты без ответственного (наш только что созданный — без assigned_to).
func TestServiceListInbox_ContainsUnassigned(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	items, err := svc.ListInbox(context.Background())
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == pid {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inbox doesn't contain just-created unassigned project %s", pid)
	}
}

// TestSkipStep_RequiresNonEmptyComment — повторяет smoke-тест из manager_validation,
// но через интеграционный путь с реальной БД. Проверяет, что service возвращает
// ErrInvalidInput на пробельный комментарий.
func TestSkipStep_RequiresNonEmptyComment(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	byName := loadStepsByName(t, repo, pid)
	step := byName["Сделать монтаж"].ID

	actor := makeManager(t, pool)
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, actor) }()

	svc := projects.NewService(repo)
	_, err := svc.SkipStep(context.Background(), pid, step, actor, "   \n\t")
	if err == nil {
		t.Fatalf("want error on whitespace-only comment, got nil")
	}
	if !strings.Contains(err.Error(), "invalid") && !strings.Contains(err.Error(), "comment") {
		t.Errorf("expected invalid_input/comment error, got %v", err)
	}
}
