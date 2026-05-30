package projects

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stubReviewWriter — для тестов SubmitReview: запоминает входы,
// возвращает фиксированный UUID. Не пишет в БД.
type stubReviewWriter struct {
	mu       sync.Mutex
	captured []ProjectReviewInput
}

func (s *stubReviewWriter) CreateProjectReview(ctx context.Context, in ProjectReviewInput) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, in)
	return uuid.New(), nil
}

// helper: проводит проект до шага client_review со статусом waiting_client.
// Делает только: start payment → complete payment → ... все шаги до
// client_review, последний переводит в waiting_client (team отдал клиенту).
//
// Возвращает projectID, stepIDs.
func advanceToClientReview(t *testing.T, ctx context.Context, svc *Service, repo *Repo, projectID, actor uuid.UUID) uuid.UUID {
	t.Helper()
	// Все шаги проекта в плоском списке.
	steps, err := repo.ListStepsWithStage(ctx, projectID, VisibilityNoFilter)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}

	var clientReviewID uuid.UUID
	for _, s := range steps {
		if s.Code == "client_review" {
			clientReviewID = s.ID
		}
	}
	if clientReviewID == uuid.Nil {
		t.Fatal("client_review step not found")
	}

	// Идём по шагам в sort_order и доводим до client_review:
	// start → complete для всех «не client_review» шагов до него.
	for _, s := range steps {
		if s.Code == "client_review" {
			// Переводим только в in_progress → waiting_client (team отдал).
			if _, err := svc.StartStep(ctx, projectID, s.ID, actor, nil); err != nil {
				t.Fatalf("start client_review: %v", err)
			}
			// in_progress → waiting_client напрямую через runTransition.
			if _, err := svc.runTransition(ctx, transitionContext{
				ProjectID:   projectID,
				StepID:      s.ID,
				To:          StepWaitingClient,
				ActorUserID: actor,
				ActorType:   "human",
			}); err != nil {
				t.Fatalf("waiting_client: %v", err)
			}
			break
		}
		// «Прыгающие» через нескольких step'ов невозможны (pending→done запрещён).
		// Делаем start → complete.
		if _, err := svc.StartStep(ctx, projectID, s.ID, actor, nil); err != nil {
			t.Fatalf("start step %s: %v", s.Code, err)
		}
		if _, err := svc.CompleteStep(ctx, projectID, s.ID, actor, nil); err != nil {
			t.Fatalf("complete step %s: %v", s.Code, err)
		}
	}
	return clientReviewID
}

// TestRevisionCycle: 2 раунда правок → dispute.
func TestRevisionCycle(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "rev-client")
	spec := mkSpecialist(t, ctx, repo, "rev-spec")
	actor := mkClient(t, ctx, repo, "rev-actor")

	created, err := svc.StartProjectManual(ctx, CreateManualInput{
		ClientUserID:     client,
		SpecialistUserID: &spec,
		TemplateCode:     "video_production",
		TemplateVersion:  1,
		Title:            "Revision cycle",
		Source:           SourceManual,
	}, actor)
	if err != nil {
		t.Fatalf("StartProjectManual: %v", err)
	}

	clientReviewID := advanceToClientReview(t, ctx, svc, repo, created.ID, actor)

	// Раунд 1: RequestRevision → step rejected, revisions_used=1, final_cut → in_progress.
	if _, err := svc.RequestRevision(ctx, created.ID, clientReviewID, client, "правки нужны 1", nil); err != nil {
		t.Fatalf("RequestRevision round 1: %v", err)
	}
	cur, err := repo.GetAdminByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAdmin: %v", err)
	}
	if cur.RevisionsUsed != 1 {
		t.Errorf("revisions_used = %d, want 1", cur.RevisionsUsed)
	}
	if cur.Status != StatusActive {
		t.Errorf("status = %s, want active", cur.Status)
	}

	// По брифу §4.2 при revision переоткрываются только final_cut и
	// client_review; internal_approval остаётся done. Команда переделывает
	// final_cut → done, client_review → in_progress → waiting_client.
	steps, _ := repo.ListStepsWithStage(ctx, created.ID, VisibilityNoFilter)
	var finalCutID uuid.UUID
	for _, s := range steps {
		if s.Code == "final_cut" {
			finalCutID = s.ID
		}
	}
	if _, err := svc.CompleteStep(ctx, created.ID, finalCutID, actor, nil); err != nil {
		t.Fatalf("complete final_cut round 1: %v", err)
	}
	// client_review был в rejected → разрешён rejected → in_progress.
	if _, err := svc.StartStep(ctx, created.ID, clientReviewID, actor, nil); err != nil {
		t.Fatalf("start client_review round 2: %v", err)
	}
	if _, err := svc.runTransition(ctx, transitionContext{
		ProjectID: created.ID, StepID: clientReviewID, To: StepWaitingClient,
		ActorUserID: actor, ActorType: "human",
	}); err != nil {
		t.Fatalf("waiting_client round 2: %v", err)
	}

	// Раунд 2: RequestRevision → revisions_used=2 (== included), последняя
	// доступная итерация.
	if _, err := svc.RequestRevision(ctx, created.ID, clientReviewID, client, "правки 2", nil); err != nil {
		t.Fatalf("RequestRevision round 2: %v", err)
	}
	cur, _ = repo.GetAdminByID(ctx, created.ID)
	if cur.RevisionsUsed != 2 {
		t.Errorf("revisions_used = %d, want 2", cur.RevisionsUsed)
	}

	// Команда снова переделывает final_cut. internal_approval не трогаем.
	if _, err := svc.CompleteStep(ctx, created.ID, finalCutID, actor, nil); err != nil {
		t.Fatalf("complete final_cut round 2: %v", err)
	}
	if _, err := svc.StartStep(ctx, created.ID, clientReviewID, actor, nil); err != nil {
		t.Fatalf("start client_review r3: %v", err)
	}
	if _, err := svc.runTransition(ctx, transitionContext{
		ProjectID: created.ID, StepID: clientReviewID, To: StepWaitingClient,
		ActorUserID: actor, ActorType: "human",
	}); err != nil {
		t.Fatalf("waiting_client round 3: %v", err)
	}

	// Раунд 3: лимит исчерпан → dispute.
	if _, err := svc.RequestRevision(ctx, created.ID, clientReviewID, client, "правки 3", nil); err != nil {
		t.Fatalf("RequestRevision round 3: %v", err)
	}
	cur, _ = repo.GetAdminByID(ctx, created.ID)
	if cur.Status != StatusDispute {
		t.Errorf("project status = %s, want dispute", cur.Status)
	}
}

// TestSubmitReview: вызывает stubReviewWriter, шаг review → done.
func TestSubmitReview(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	stub := &stubReviewWriter{}
	svc := NewService(repo).WithReviewWriter(stub)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "rev-write-client")
	spec := mkSpecialist(t, ctx, repo, "rev-write-spec")
	actor := mkClient(t, ctx, repo, "rev-write-actor")

	created, err := svc.StartProjectManual(ctx, CreateManualInput{
		ClientUserID:     client,
		SpecialistUserID: &spec,
		TemplateCode:     "video_production",
		TemplateVersion:  1,
		Title:            "Submit review",
		Source:           SourceManual,
	}, actor)
	if err != nil {
		t.Fatalf("StartProjectManual: %v", err)
	}

	// Найти шаг review и довести его до in_progress (через start → straight
	// до done через SubmitReview).
	steps, _ := repo.ListStepsWithStage(ctx, created.ID, VisibilityNoFilter)
	var reviewID uuid.UUID
	for _, s := range steps {
		if s.Code == "review" {
			reviewID = s.ID
		}
	}
	if _, err := svc.StartStep(ctx, created.ID, reviewID, actor, nil); err != nil {
		t.Fatalf("start review step: %v", err)
	}

	view, returnedReviewID, err := svc.SubmitReview(ctx, created.ID, reviewID, client, 5, "Отличная работа", nil)
	if err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if view.Status != StepDone {
		t.Errorf("review step status = %s, want done", view.Status)
	}
	if returnedReviewID == uuid.Nil {
		t.Error("review_id is nil")
	}
	if len(stub.captured) != 1 {
		t.Fatalf("stub captures = %d, want 1", len(stub.captured))
	}
	got := stub.captured[0]
	if got.ProjectID != created.ID {
		t.Errorf("captured project_id mismatch")
	}
	if got.AuthorUserID != client || got.TargetUserID != spec {
		t.Errorf("author/target mismatch")
	}
	if got.Rating != 5 {
		t.Errorf("rating = %d, want 5", got.Rating)
	}
}

// TestAdminPatchOptimisticLock: PATCH /admin/projects с правильным
// updated_at → 200; с устаревшим → ErrConflict (= 409 на хендлере).
func TestAdminPatchOptimisticLock(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "patch-client")
	actor := mkClient(t, ctx, repo, "patch-actor")

	created, err := svc.StartProjectManual(ctx, CreateManualInput{
		ClientUserID:    client,
		TemplateCode:    "video_production",
		TemplateVersion: 1,
		Title:           "Patch test",
		Source:          SourceManual,
	}, actor)
	if err != nil {
		t.Fatalf("StartProjectManual: %v", err)
	}

	// Корректный PATCH.
	newTitle := "Updated title"
	updated, err := svc.UpdateAdmin(ctx, created.ID, PatchAdminInput{
		Title:     &newTitle,
		UpdatedAt: created.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("UpdateAdmin: %v", err)
	}
	if updated.Title != newTitle {
		t.Errorf("title = %q, want %q", updated.Title, newTitle)
	}

	// Stale PATCH → ErrConflict (используем старый updated_at).
	staleTitle := "Stale title"
	_, err = svc.UpdateAdmin(ctx, created.ID, PatchAdminInput{
		Title:     &staleTitle,
		UpdatedAt: created.UpdatedAt, // тот же старый — уже устарел
	})
	if err == nil || err.Error() != ErrConflict.Error() {
		t.Errorf("expected ErrConflict, got %v", err)
	}
}
