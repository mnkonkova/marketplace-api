package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/projects"
	"marketpclce/tests/integration"
)

// Переписка в проекте разъезжается на три ветки, и разделены они не
// оформлением, а правом читать: клиент не видит переписку менеджера с
// креаторами, креатор — переписку клиента и ветки соседей.

// addCreator — завести креатора и включить его в состав проекта.
func addCreator(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"creator-"+uuid.NewString()+"@example.com").Scan(&id); err != nil {
		t.Fatalf("create creator: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO specialist_profiles (user_id, display_name, is_published) VALUES ($1, $2, TRUE)`,
		id, name); err != nil {
		t.Fatalf("create creator profile: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_creators (project_id, creator_user_id) VALUES ($1, $2)`,
		projectID, id); err != nil {
		t.Fatalf("add to project: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id) })
	return id
}

// assignManager — назначить проекту менеджера. Он участник обеих веток и
// кандидат в упоминания.
func assignManager(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_manager, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, TRUE, now()) RETURNING id`,
		"mgr-"+uuid.NewString()+"@example.com").Scan(&id); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE projects SET assigned_to_user_id = $2 WHERE id = $1`, projectID, id); err != nil {
		t.Fatalf("assign manager: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func bodies(items []projects.Comment) string {
	parts := make([]string, 0, len(items))
	for _, c := range items {
		parts = append(parts, c.BodyText)
	}
	return strings.Join(parts, " | ")
}

// ---- ТЕСТ: ветки не видят друг друга ----

func TestThreadsAreIsolated(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	managerID := assignManager(t, pool, pid)
	creatorA := addCreator(t, pool, pid, "Креатор А")
	creatorB := addCreator(t, pool, pid, "Креатор Б")

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	mustComment(t, svc, projects.CommentRequest{
		ProjectID: pid, AuthorID: clientID, Thread: projects.ThreadClient,
		Body: "клиент менеджеру",
	})
	mustComment(t, svc, projects.CommentRequest{
		ProjectID: pid, AuthorID: creatorA, Thread: projects.ThreadCreator,
		ThreadUserID: &creatorA, Body: "креатор А менеджеру",
	})
	mustComment(t, svc, projects.CommentRequest{
		ProjectID: pid, AuthorID: creatorB, Thread: projects.ThreadCreator,
		ThreadUserID: &creatorB, Body: "креатор Б менеджеру",
	})
	mustComment(t, svc, projects.CommentRequest{
		ProjectID: pid, AuthorID: managerID, Thread: projects.ThreadInternal,
		Body: "внутренняя заметка",
	})

	t.Run("клиент видит только свою ветку", func(t *testing.T) {
		got, err := svc.ListThread(ctx, pid, projects.ThreadClient, nil)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || got[0].BodyText != "клиент менеджеру" {
			t.Errorf("клиенту видно лишнее: %s", bodies(got))
		}
	})

	t.Run("креатор видит только свою ветку", func(t *testing.T) {
		got, err := svc.ListThread(ctx, pid, projects.ThreadCreator, &creatorA)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || got[0].BodyText != "креатор А менеджеру" {
			t.Errorf("креатору А видно лишнее: %s", bodies(got))
		}
	})

	t.Run("менеджер видит всё", func(t *testing.T) {
		got, err := svc.ListAllComments(ctx, pid)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 4 {
			t.Errorf("менеджеру должно быть видно 4 сообщения, видно %d: %s", len(got), bodies(got))
		}
	})
}

// Ветка креатора адресная: запрос без указания, чья она, вернул бы
// переписку всех креаторов разом.
func TestCreatorThreadRequiresUser(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	if _, err := svc.ListThread(context.Background(), pid, projects.ThreadCreator, nil); err == nil {
		t.Errorf("ветка креатора без креатора не должна читаться")
	}
}

// ---- ТЕСТ: кому какая ветка принадлежит ----

func TestResolveCreatorThread(t *testing.T) {
	pool := integration.Pool(t)
	_, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	creatorID := addCreator(t, pool, pid, "Креатор")
	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	t.Run("креатор под ключ — своя адресная ветка", func(t *testing.T) {
		thread, user, err := svc.ResolveCreatorThread(ctx, pid, creatorID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if thread != projects.ThreadCreator || user == nil || *user != creatorID {
			t.Errorf("got thread=%s user=%v", thread, user)
		}
	})

	t.Run("посторонний — не найдено", func(t *testing.T) {
		if _, _, err := svc.ResolveCreatorThread(ctx, pid, uuid.New()); err == nil {
			t.Errorf("посторонний получил ветку в чужом проекте")
		}
	})

	t.Run("выбывший креатор ветку теряет", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE project_creators SET removed_at = now()
			 WHERE project_id = $1 AND creator_user_id = $2`, pid, creatorID); err != nil {
			t.Fatalf("remove creator: %v", err)
		}
		defer pool.Exec(ctx,
			`UPDATE project_creators SET removed_at = NULL
			 WHERE project_id = $1 AND creator_user_id = $2`, pid, creatorID)

		if _, _, err := svc.ResolveCreatorThread(ctx, pid, creatorID); err == nil {
			t.Errorf("убранный из проекта креатор всё ещё пишет в него")
		}
	})
}

// В общем проекте менеджера нет вовсе, и исполнителю не с кем вести
// отдельную ветку: он говорит с клиентом в клиентской.
func TestGeneralPerformerTalksInClientThread(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, err := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	thread, user, err := svc.ResolveCreatorThread(ctx, p.ID, specID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if thread != projects.ThreadClient || user != nil {
		t.Fatalf("исполнитель общего проекта должен писать в клиентскую ветку, получено %s/%v", thread, user)
	}

	mustComment(t, svc, projects.CommentRequest{
		ProjectID: p.ID, AuthorID: specID, Thread: thread, Body: "сдам к пятнице",
	})
	got, err := svc.ListThread(ctx, p.ID, projects.ThreadClient, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("клиент не видит сообщение исполнителя: %s", bodies(got))
	}
}

// ---- ТЕСТ: разметка чистится на записи ----

func TestHTMLCommentSanitizedOnWrite(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	c := mustComment(t, svc, projects.CommentRequest{
		ProjectID: pid, AuthorID: clientID, Thread: projects.ThreadClient,
		Format: projects.FormatHTML,
		Body:   `<p>Правки <strong>срочные</strong></p><script>alert(1)</script><a href="javascript:alert(2)">клик</a>`,
	})

	if strings.Contains(strings.ToLower(c.Body), "script") || strings.Contains(strings.ToLower(c.Body), "javascript") {
		t.Errorf("опасная разметка сохранилась: %s", c.Body)
	}
	if !strings.Contains(c.Body, "<strong>срочные</strong>") {
		t.Errorf("форматирование потерялось: %s", c.Body)
	}
	if strings.Contains(c.BodyText, "<") {
		t.Errorf("body_text должен быть без тегов: %q", c.BodyText)
	}

	// В базе лежит уже очищенное: читателю не приходится чистить самому.
	var stored, storedText, format string
	if err := pool.QueryRow(ctx,
		`SELECT body, body_text, body_format FROM project_comments WHERE id = $1`,
		c.ID).Scan(&stored, &storedText, &format); err != nil {
		t.Fatalf("query: %v", err)
	}
	if strings.Contains(strings.ToLower(stored), "script") {
		t.Errorf("в базе осталась опасная разметка: %s", stored)
	}
	if format != projects.FormatHTML {
		t.Errorf("body_format: %s", format)
	}
	if storedText != c.BodyText {
		t.Errorf("body_text в базе разошёлся с ответом: %q vs %q", storedText, c.BodyText)
	}
}

// ---- ТЕСТ: упоминания ----

func TestMentionsLimitedToThreadParticipants(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	managerID := assignManager(t, pool, pid)
	creatorID := addCreator(t, pool, pid, "Креатор")

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	mention := func(id uuid.UUID, name string) string {
		return `<span ` + "data-mention-user-id" + `="` + id.String() + `">@` + name + `</span>`
	}

	// Клиент упоминает менеджера — участника своей ветки, и заодно
	// креатора, которого в клиентской ветке нет.
	c := mustComment(t, svc, projects.CommentRequest{
		ProjectID: pid, AuthorID: clientID, Thread: projects.ThreadClient,
		Format: projects.FormatHTML,
		Body:   `Привет ` + mention(managerID, "Менеджер") + ` и ` + mention(creatorID, "Креатор"),
	})

	if len(c.Mentions) != 1 || c.Mentions[0] != managerID {
		t.Errorf("упомянуться должен только менеджер, получено %v", c.Mentions)
	}
	// Текст обоих упоминаний остаётся: чистка не портит написанное.
	if !strings.Contains(c.BodyText, "@Креатор") {
		t.Errorf("текст запрещённого упоминания пропал: %q", c.BodyText)
	}

	var stored int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM comment_mentions WHERE comment_id = $1`, c.ID).Scan(&stored); err != nil {
		t.Fatalf("query mentions: %v", err)
	}
	if stored != 1 {
		t.Errorf("в comment_mentions должна быть одна запись, их %d", stored)
	}

	// Упоминания доезжают до читателя вместе с сообщением.
	items, err := svc.ListThread(ctx, pid, projects.ThreadClient, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || len(items[0].Mentions) != 1 || items[0].Mentions[0] != managerID {
		t.Errorf("упоминания не вернулись при чтении: %+v", items)
	}
}

func TestMentionCandidatesByThread(t *testing.T) {
	pool := integration.Pool(t)
	clientID, _, pid, cleanup := setupPipelineAndProject(t, pool)
	defer cleanup()

	managerID := assignManager(t, pool, pid)
	creatorID := addCreator(t, pool, pid, "Креатор")

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	ids := func(ps []projects.Participant) map[uuid.UUID]string {
		m := make(map[uuid.UUID]string, len(ps))
		for _, p := range ps {
			m[p.UserID] = p.Role
		}
		return m
	}

	client, err := svc.ThreadParticipants(ctx, pid, projects.ThreadClient, nil)
	if err != nil {
		t.Fatalf("client participants: %v", err)
	}
	got := ids(client)
	if _, ok := got[clientID]; !ok {
		t.Errorf("клиента нет среди участников своей ветки: %v", got)
	}
	if _, ok := got[managerID]; !ok {
		t.Errorf("менеджера нет среди участников клиентской ветки: %v", got)
	}
	if _, ok := got[creatorID]; ok {
		t.Errorf("креатор попал в кандидаты клиентской ветки: %v", got)
	}

	creator, err := svc.ThreadParticipants(ctx, pid, projects.ThreadCreator, &creatorID)
	if err != nil {
		t.Fatalf("creator participants: %v", err)
	}
	got = ids(creator)
	if _, ok := got[creatorID]; !ok {
		t.Errorf("креатора нет в своей же ветке: %v", got)
	}
	if _, ok := got[clientID]; ok {
		t.Errorf("клиент попал в кандидаты креаторской ветки: %v", got)
	}
}

func mustComment(t *testing.T, svc *projects.Service, req projects.CommentRequest) projects.Comment {
	t.Helper()
	c, err := svc.CreateComment(context.Background(), req)
	if err != nil {
		t.Fatalf("create comment (%s): %v", req.Thread, err)
	}
	return c
}

// Клиентские ручки переписки авторизуются через GetClientProject, а у
// общего проекта нет ни одной стадии. Проверяем, что пустая воронка не
// мешает клиенту читать собственную переписку.
func TestGeneralClientCanReadOwnThread(t *testing.T) {
	pool := integration.Pool(t)
	clientID, specID, cleanup := setupGeneralActors(t, pool)
	defer cleanup()

	svc := projects.NewService(projects.NewRepo(pool))
	ctx := context.Background()

	p, err := svc.CreateGeneral(ctx, generalInput(clientID, specID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.GetClientProject(ctx, p.ID, clientID); err != nil {
		t.Fatalf("клиент не может открыть свой общий проект: %v", err)
	}
	mustComment(t, svc, projects.CommentRequest{
		ProjectID: p.ID, AuthorID: clientID, Thread: projects.ThreadClient, Body: "когда покажете?",
	})
	got, err := svc.ListThread(ctx, p.ID, projects.ThreadClient, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("клиент не видит своё сообщение: %s", bodies(got))
	}
}
