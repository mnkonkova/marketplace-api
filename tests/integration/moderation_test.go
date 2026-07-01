package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/admin"
	"marketpclce/internal/outbox"
	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// ─── Helpers ──────────────────────────────────────────────────────

// makeModerationSpecialist — спец с заполненными publish-required полями
// (bio, display_name, is_freelance=true). Возвращает user_id; чистка
// предоставляется caller'ом через DELETE FROM users.
func makeModerationSpecialist(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"mod-"+uuid.NewString()+"@x").Scan(&uid); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, bio, is_freelance, is_published)
VALUES ($1, $2, $3, TRUE, FALSE)`,
		uid, "Mod "+uid.String()[:6], "Я моду модерируюсь, минимум полей для publish."); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	return uid
}

// fetchModeration — читает текущий moderation_status профиля.
func fetchModeration(t *testing.T, pool *pgxpool.Pool, uid uuid.UUID) (status, reason string, updatedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
SELECT moderation_status, COALESCE(moderation_reason, ''), updated_at
FROM specialist_profiles WHERE user_id = $1`, uid).Scan(&status, &reason, &updatedAt); err != nil {
		t.Fatalf("fetch moderation: %v", err)
	}
	return status, reason, updatedAt
}

// countOutboxEvents — выборка событий по агрегату и типу. Без orderBy —
// в state-machine тестах хватает count'a.
func countOutboxEvents(t *testing.T, pool *pgxpool.Pool, aggregate, aggregateID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM outbox
WHERE aggregate = $1 AND aggregate_id = $2 AND event_type = $3`,
		aggregate, aggregateID, eventType).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

func cleanupSpec(t *testing.T, pool *pgxpool.Pool, uid uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, uid.String())
	pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid)
}

// ─── State machine: первый publish (pending → notify) ─────────────

func TestModerationFirstPublishEmitsPending(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	svc := profiles.NewService(profiles.NewRepo(pool))

	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("set published: %v", err)
	}
	status, _, _ := fetchModeration(t, pool, uid)
	if status != "pending_review" {
		t.Errorf("status after first publish = %q, want pending_review", status)
	}
	n := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)
	if n != 1 {
		t.Errorf("moderation_pending events = %d, want 1", n)
	}
}

// ─── State machine: approved → patch → pending + notify ───────────

func TestModerationApprovedPatchBumpsToPending(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	repo := profiles.NewRepo(pool)
	svc := profiles.NewService(repo)

	// Сначала publish (→ pending) и эмуляция админ-approve вручную.
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("set published: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE specialist_profiles SET moderation_status='approved', moderation_reviewed_at=now() WHERE user_id=$1`,
		uid); err != nil {
		t.Fatalf("manual approve: %v", err)
	}

	prevNotifyCount := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)

	// Спец редактирует bio через PatchFull.
	newBio := "новое описание, изменил"
	if _, err := svc.PatchFull(ctx, uid, profiles.PatchFullInput{Bio: &newBio}); err != nil {
		t.Fatalf("patch full: %v", err)
	}
	status, _, _ := fetchModeration(t, pool, uid)
	if status != "pending_review" {
		t.Errorf("approved+patch → status %q, want pending_review", status)
	}
	n := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)
	if n != prevNotifyCount+1 {
		t.Errorf("moderation_pending count после patch = %d, want %d", n, prevNotifyCount+1)
	}
}

// ─── State machine: approved → unpublish → status не трогается ────

func TestModerationApprovedUnpublishKeepsApproved(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	svc := profiles.NewService(profiles.NewRepo(pool))
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE specialist_profiles SET moderation_status='approved' WHERE user_id=$1`, uid); err != nil {
		t.Fatalf("manual approve: %v", err)
	}

	if _, err := svc.SetPublished(ctx, uid, false); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	status, _, _ := fetchModeration(t, pool, uid)
	if status != "approved" {
		t.Errorf("unpublish изменил status: %q, want approved", status)
	}
}

// ─── State machine: rejected → publish → pending + notify ────────

func TestModerationRejectedRepublishGoesToPending(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	svc := profiles.NewService(profiles.NewRepo(pool))

	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Имитируем reject.
	if _, err := pool.Exec(ctx, `
UPDATE specialist_profiles SET is_published=FALSE, moderation_status='rejected',
       moderation_reason='тестовая причина' WHERE user_id=$1`, uid); err != nil {
		t.Fatalf("manual reject: %v", err)
	}

	prev := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)

	// Спец повторно публикует.
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("republish: %v", err)
	}
	status, _, _ := fetchModeration(t, pool, uid)
	if status != "pending_review" {
		t.Errorf("rejected+publish → status %q, want pending_review", status)
	}
	n := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)
	if n != prev+1 {
		t.Errorf("moderation_pending events после republish = %d, want %d", n, prev+1)
	}
}

// ─── Admin Approve успешно (с optimistic lock) ────────────────────

func TestModerationAdminApproveOK(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	profilesRepo := profiles.NewRepo(pool)
	svc := profiles.NewService(profilesRepo)
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	adminSvc := admin.NewService(admin.NewRepo(pool), nil, "http://x", time.Hour).
		WithProfilesRepo(profilesRepo)

	_, _, updatedAt := fetchModeration(t, pool, uid)
	actorID := uuid.New()
	if err := adminSvc.ApproveSpecialist(ctx, uid, actorID, &updatedAt); err != nil {
		t.Fatalf("approve: %v", err)
	}
	status, _, _ := fetchModeration(t, pool, uid)
	if status != "approved" {
		t.Errorf("approve → status %q, want approved", status)
	}
}

// ─── Admin Approve с устаревшим updated_at → 409 ──────────────────

func TestModerationAdminApproveConflictOnStaleVersion(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	profilesRepo := profiles.NewRepo(pool)
	svc := profiles.NewService(profilesRepo)
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	adminSvc := admin.NewService(admin.NewRepo(pool), nil, "http://x", time.Hour).
		WithProfilesRepo(profilesRepo)

	// admin "увидел" профиль здесь
	_, _, oldUpdatedAt := fetchModeration(t, pool, uid)

	// Спец параллельно патчит профиль — updated_at сдвигается
	newBio := "успел исправить пока админ открывал"
	if _, err := svc.PatchFull(ctx, uid, profiles.PatchFullInput{Bio: &newBio}); err != nil {
		t.Fatalf("user patch: %v", err)
	}

	// admin шлёт approve со старым updated_at — должен получить conflict
	actorID := uuid.New()
	err := adminSvc.ApproveSpecialist(ctx, uid, actorID, &oldUpdatedAt)
	if !errors.Is(err, profiles.ErrConflict) {
		t.Errorf("approve со стэйл updated_at: want ErrConflict, got %v", err)
	}
	status, _, _ := fetchModeration(t, pool, uid)
	if status != "pending_review" {
		t.Errorf("status после conflict = %q, want pending_review (не изменён)", status)
	}
}

// ─── Admin Reject требует причину ─────────────────────────────────

func TestModerationAdminRejectRequiresReason(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	profilesRepo := profiles.NewRepo(pool)
	svc := profiles.NewService(profilesRepo)
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	adminSvc := admin.NewService(admin.NewRepo(pool), nil, "http://x", time.Hour).
		WithProfilesRepo(profilesRepo)

	// Создаём реального admin'а — moderation_reviewed_by REFERENCES users(id),
	// uuid.New() не сработает (23503 FK violation после migration 00021).
	var adminID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_admin, is_approved, email_verified_at)
VALUES ($1, 'x', 'client', TRUE, TRUE, now()) RETURNING id`,
		"admin-"+uuid.NewString()+"@x").Scan(&adminID); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, adminID)

	if err := adminSvc.RejectSpecialist(ctx, uid, adminID, "  ", nil); !errors.Is(err, admin.ErrModerationReasonRequired) {
		t.Errorf("reject с пустой причиной: want ErrModerationReasonRequired, got %v", err)
	}
	if err := adminSvc.RejectSpecialist(ctx, uid, adminID, "плохо", nil); err != nil {
		t.Fatalf("reject с валидной причиной: %v", err)
	}
	status, reason, _ := fetchModeration(t, pool, uid)
	if status != "rejected" {
		t.Errorf("status после reject = %q, want rejected", status)
	}
	if reason != "плохо" {
		t.Errorf("reason = %q, want 'плохо'", reason)
	}
}

// ─── ListModerationQueue: pending видны, approved нет ─────────────

func TestModerationListQueueShowsPending(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	profilesRepo := profiles.NewRepo(pool)
	svc := profiles.NewService(profilesRepo)
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	items, total, err := profilesRepo.ListModerationQueue(ctx, "pending_review", 100, 0)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if total < 1 {
		t.Errorf("total = %d, want >=1", total)
	}
	found := false
	for _, it := range items {
		if it.UserID == uid {
			found = true
			if it.Status != "pending_review" {
				t.Errorf("item status = %q", it.Status)
			}
		}
	}
	if !found {
		t.Errorf("свой спец не в pending-очереди")
	}

	// approve и убедись что больше его нет в pending
	if _, err := pool.Exec(ctx, `
UPDATE specialist_profiles SET moderation_status='approved' WHERE user_id=$1`, uid); err != nil {
		t.Fatalf("manual approve: %v", err)
	}
	items, _, err = profilesRepo.ListModerationQueue(ctx, "pending_review", 100, 0)
	if err != nil {
		t.Fatalf("list after approve: %v", err)
	}
	for _, it := range items {
		if it.UserID == uid {
			t.Errorf("approved спец остался в pending-очереди")
		}
	}
}

// ─── Шторм правок: notify только на первой (approved→pending) ────

func TestModerationStormOfEditsEmitsOnceFromApproved(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	profilesRepo := profiles.NewRepo(pool)
	svc := profiles.NewService(profilesRepo)

	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE specialist_profiles SET moderation_status='approved' WHERE user_id=$1`, uid); err != nil {
		t.Fatalf("approve: %v", err)
	}
	prev := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)

	// 5 последовательных правок: первая approved→pending, остальные pending→pending.
	for i := 0; i < 5; i++ {
		bio := "edit " + time.Now().Format("150405.000")
		if _, err := svc.PatchFull(ctx, uid, profiles.PatchFullInput{Bio: &bio}); err != nil {
			t.Fatalf("patch %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond) // чтобы updated_at гарантированно сдвигался
	}
	n := countOutboxEvents(t, pool, outbox.AggregateModeration, uid.String(), outbox.EventModerationSpecialistPending)
	if got := n - prev; got != 1 {
		t.Errorf("notify events после шторма правок = %d, want 1 (только первая approved→pending)", got)
	}
}

// ─── Search фильтр: pending не идёт в OS-doc ─────────────────────

func TestModerationLoadDocFiltersPending(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	uid := makeModerationSpecialist(t, pool)
	defer cleanupSpec(t, pool, uid)

	profilesRepo := profiles.NewRepo(pool)
	svc := profiles.NewService(profilesRepo)
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// pending — Reconcile должен снести из ES. На уровне repo: LoadDoc вернёт
	// поле moderation_status='pending_review' (или approved — после).
	prof, err := profilesRepo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if prof.ModerationStatus != "pending_review" {
		t.Errorf("profile.ModerationStatus = %q, want pending_review", prof.ModerationStatus)
	}

	// GetPublic не должен видеть pending'нутый профиль
	if _, err := profilesRepo.GetPublic(ctx, uid); !errors.Is(err, profiles.ErrNotFound) {
		t.Errorf("GetPublic для pending вернул %v, want ErrNotFound", err)
	}

	// approve → GetPublic возвращает профиль
	if _, err := pool.Exec(ctx, `
UPDATE specialist_profiles SET moderation_status='approved' WHERE user_id=$1`, uid); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := profilesRepo.GetPublic(ctx, uid); err != nil {
		t.Errorf("GetPublic для approved: %v, want nil", err)
	}
}
