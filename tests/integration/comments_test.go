package integration_test

import (
	"context"
	"testing"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: CreateComment создаёт запись + событие в ленте ----

func TestCreateCommentEmitsEvent(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	c, err := repo.CreateComment(ctx, pid, clientID, "Привет, как дела?", "plain")
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	if c.Body != "Привет, как дела?" {
		t.Errorf("body: %s", c.Body)
	}

	// В project_step_events должна быть запись event_kind=comment.
	var eventKind string
	var payload []byte
	if err := pool.QueryRow(ctx, `
SELECT event_kind, payload FROM project_step_events
WHERE project_id = $1 AND event_kind = 'comment'
ORDER BY created_at DESC LIMIT 1`, pid).Scan(&eventKind, &payload); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if eventKind != "comment" {
		t.Errorf("event kind: %s", eventKind)
	}
	// payload должен содержать comment_id (валидный JSON)
	if len(payload) == 0 {
		t.Errorf("payload empty")
	}
}

// ---- ТЕСТ: ListComments возвращает только не-удалённые ----

func TestListCommentsSkipsDeleted(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	c1, _ := repo.CreateComment(ctx, pid, clientID, "первый", "plain")
	c2, _ := repo.CreateComment(ctx, pid, clientID, "второй", "plain")

	// Soft-delete первого.
	if _, err := pool.Exec(ctx,
		`UPDATE project_comments SET deleted_at = now() WHERE id = $1`,
		c1.ID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	items, err := repo.ListComments(ctx, pid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("want 1 comment after soft-delete, got %d", len(items))
	}
	if len(items) > 0 && items[0].ID != c2.ID {
		t.Errorf("wrong comment returned")
	}
	_ = c2 // силенс linter
}
