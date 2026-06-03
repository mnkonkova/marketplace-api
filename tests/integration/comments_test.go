package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

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

	c, err := repo.CreateComment(ctx, pid, clientID, "Привет, как дела?", "plain", false)
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

	c1, _ := repo.CreateComment(ctx, pid, clientID, "первый", "plain", false)
	c2, _ := repo.CreateComment(ctx, pid, clientID, "второй", "plain", false)

	// Soft-delete первого.
	if _, err := pool.Exec(ctx,
		`UPDATE project_comments SET deleted_at = now() WHERE id = $1`,
		c1.ID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	items, err := repo.ListComments(ctx, pid, true)
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

// ---- ТЕСТ: is_internal скрывается из клиентской выдачи ----

func TestInternalCommentsHiddenFromClient(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	if _, err := repo.CreateComment(ctx, pid, clientID, "виден всем", "plain", false); err != nil {
		t.Fatalf("create public: %v", err)
	}
	if _, err := repo.CreateComment(ctx, pid, clientID, "только команда", "plain", true); err != nil {
		t.Fatalf("create internal: %v", err)
	}

	client, err := repo.ListComments(ctx, pid, false)
	if err != nil {
		t.Fatalf("list client: %v", err)
	}
	if len(client) != 1 {
		t.Errorf("client must see 1, got %d", len(client))
	}
	if len(client) > 0 && client[0].IsInternal {
		t.Errorf("client got internal comment")
	}

	staff, err := repo.ListComments(ctx, pid, true)
	if err != nil {
		t.Fatalf("list staff: %v", err)
	}
	if len(staff) != 2 {
		t.Errorf("staff must see 2, got %d", len(staff))
	}

	// Внутренний не должен попасть в ленту событий — иначе клиент его прочитает через events.
	var eventCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM project_step_events WHERE project_id=$1 AND event_kind='comment'`,
		pid).Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("expected 1 event for non-internal comment, got %d", eventCount)
	}
}

// ---- ТЕСТ: author_name fallback ладдер sp.display_name → cp.display_name → email-префикс ----

func TestCommentAuthorNameFallback(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	repo := projects.NewRepo(pool)
	ctx := context.Background()

	// 1) автор только с email — должен резолвиться в email-префикс.
	var emailOnly uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_manager, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, TRUE, now()) RETURNING id`,
		"only-email-"+uuid.NewString()+"@example.com").Scan(&emailOnly)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, emailOnly)

	// 2) автор с client_profile.display_name = "Иван-Клиент"
	var withCP uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, now()) RETURNING id`,
		"cp-"+uuid.NewString()+"@example.com").Scan(&withCP)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, withCP)
	if _, err := pool.Exec(ctx,
		`INSERT INTO client_profiles (user_id, display_name) VALUES ($1, 'Иван-Клиент')`,
		withCP); err != nil {
		t.Fatalf("insert cp: %v", err)
	}

	c1, _ := repo.CreateComment(ctx, pid, emailOnly, "от менеджера без профиля", "plain", false)
	c2, _ := repo.CreateComment(ctx, pid, withCP, "от клиента с профилем", "plain", false)

	if c1.AuthorName == "" || c1.AuthorName == "Без имени" {
		t.Errorf("ожидался префикс email, получено %q", c1.AuthorName)
	}
	if c2.AuthorName != "Иван-Клиент" {
		t.Errorf("ожидалось Иван-Клиент, получено %q", c2.AuthorName)
	}

	// И через ListComments — то же.
	items, err := repo.ListComments(ctx, pid, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := map[uuid.UUID]string{}
	for _, c := range items {
		names[c.ID] = c.AuthorName
	}
	if names[c1.ID] == "" || names[c1.ID] == "Без имени" {
		t.Errorf("ListComments: ожидался email-префикс, получено %q", names[c1.ID])
	}
	if names[c2.ID] != "Иван-Клиент" {
		t.Errorf("ListComments: ожидалось Иван-Клиент, получено %q", names[c2.ID])
	}
}
