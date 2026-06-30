package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// ─── stub media с реальным bucket-check ───────────────────────────
//
// stubMedia из multipart_test.go всегда возвращает "" из KeyFromURL —
// для photo-set'а это бы значило «URL не в нашем бакете → ErrInvalidInput».
// Тестам photo-set нужен stub, который сэмулирует bucket: всё что начинается
// с https://stub.test/ → возвращает остаток как key. Так же ведёт себя
// продакшен-реализация s3.Client.
type bucketMedia struct{}

func (bucketMedia) PresignPut(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "https://stub/put", nil
}
func (bucketMedia) PublicURL(key string) string { return "https://stub.test/" + key }
func (bucketMedia) ListObjects(_ context.Context, _ string, _ func(string, time.Time) bool) error {
	return nil
}
func (bucketMedia) RemoveObject(_ context.Context, _ string) error { return nil }
func (bucketMedia) KeyFromURL(raw string) string {
	const prefix = "https://stub.test/"
	if strings.HasPrefix(raw, prefix) {
		return strings.TrimPrefix(raw, prefix)
	}
	return ""
}
func (bucketMedia) NewMultipartUpload(_ context.Context, _, _ string) (string, error) {
	return "upl", nil
}
func (bucketMedia) PresignPart(_ context.Context, _, _ string, _ int, _ time.Duration) (string, error) {
	return "https://stub/part", nil
}
func (bucketMedia) CompleteMultipart(_ context.Context, _, _ string, _ []profiles.MultipartPart) error {
	return nil
}
func (bucketMedia) AbortMultipart(_ context.Context, _, _ string) error { return nil }

// ─── Helpers ──────────────────────────────────────────────────────

// makePhotoSetSpec — спец с профилем + одной категорией. Категория добавляется
// в specialist_categories (с is_primary=true), иначе AddPortfolioPhotoSet
// с непустыми category_codes отказывал бы «categories ∉ profile».
func makePhotoSetSpec(t *testing.T, pool *pgxpool.Pool, primaryCat string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`,
		"ps-"+uuid.NewString()+"@x").Scan(&uid); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO specialist_profiles (user_id, display_name, bio, is_freelance, is_published)
VALUES ($1, $2, $3, TRUE, FALSE)`,
		uid, "PS "+uid.String()[:6], "Bio for photo-set test specialist."); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if primaryCat != "" {
		if _, err := pool.Exec(ctx,
			`INSERT INTO specialist_categories (user_id, category_code, is_primary)
			 VALUES ($1, $2, TRUE)`, uid, primaryCat); err != nil {
			t.Fatalf("attach category: %v", err)
		}
	}
	return uid
}

// bucketURL — построитель валидного для bucketMedia URL'а (тест-data sec D6).
func bucketURL(uid uuid.UUID, name string) string {
	return "https://stub.test/images/" + uid.String() + "/" + name
}

// listPhotoSetImages — читает кадры photo-set'а напрямую из БД (минуя сервис).
// Удобно для проверки cover/cascade-эффектов после операций.
func listPhotoSetImages(t *testing.T, pool *pgxpool.Pool, itemID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT image_url FROM portfolio_images WHERE portfolio_item_id = $1 ORDER BY sort_order`, itemID)
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, u)
	}
	return out
}

func portfolioItemExists(t *testing.T, pool *pgxpool.Pool, itemID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM portfolio_items WHERE id = $1`, itemID).Scan(&n); err != nil {
		t.Fatalf("count item: %v", err)
	}
	return n == 1
}

func portfolioItemCover(t *testing.T, pool *pgxpool.Pool, itemID uuid.UUID) string {
	t.Helper()
	var cover string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(thumbnail_url, '') FROM portfolio_items WHERE id = $1`, itemID).Scan(&cover); err != nil {
		t.Fatalf("get cover: %v", err)
	}
	return cover
}

func newPhotoSetSvc(t *testing.T) *profiles.Service {
	pool := integration.Pool(t)
	return profiles.NewService(profiles.NewRepo(pool)).WithMediaStorage(bucketMedia{})
}

// ─── AddPortfolioPhotoSet ─────────────────────────────────────────

func TestPhotoSet_Create_HappyPath(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	img1, img2, img3 := bucketURL(uid, "a.jpg"), bucketURL(uid, "b.jpg"), bucketURL(uid, "c.jpg")
	item, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Test set",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: img1},
			{ImageURL: img2},
			{ImageURL: img3},
		},
	})
	if err != nil {
		t.Fatalf("AddPortfolioPhotoSet: %v", err)
	}
	if item.Kind != "image" {
		t.Errorf("kind = %q, want image", item.Kind)
	}
	if got := len(item.Images); got != 3 {
		t.Fatalf("images count = %d, want 3", got)
	}
	// Cover = первое фото (sort_order=0).
	if item.ThumbnailURL != img1 {
		t.Errorf("cover = %q, want %q", item.ThumbnailURL, img1)
	}
	// БД-инвариант: sort_order проставлен 0..N-1, в порядке передачи.
	got := listPhotoSetImages(t, pool, item.ID)
	want := []string{img1, img2, img3}
	if len(got) != len(want) {
		t.Fatalf("db images count: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %s want %s", i, got[i], want[i])
		}
	}
}

func TestPhotoSet_Create_RejectsExternalURL(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	_, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "External",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: "https://evil.example.com/phishing.jpg"},
		},
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for external URL, got %v", err)
	}
}

func TestPhotoSet_Create_RejectsZeroImages(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	_, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Empty",
		Images: nil,
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for 0 images, got %v", err)
	}
}

func TestPhotoSet_Create_RejectsTooManyImages(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	imgs := make([]profiles.PortfolioPhotoRef, 11)
	for i := range imgs {
		imgs[i] = profiles.PortfolioPhotoRef{ImageURL: bucketURL(uid, fmt.Sprintf("p%d.jpg", i))}
	}
	_, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Too many",
		Images: imgs,
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for 11 images, got %v", err)
	}
}

func TestPhotoSet_Create_RejectsCategoryNotInProfile(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	_, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title:         "Cross-cat",
		CategoryCodes: []string{"photographer"}, // у спеца только editor
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(uid, "a.jpg")},
		},
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for foreign category, got %v", err)
	}
}

func TestPhotoSet_Create_RespectsProfileCategoriesFromRequest(t *testing.T) {
	// Регистрационный сценарий: спец только что выбрал category в форме,
	// в БД пусто. ProfileCategories в запросе должен разрешать эту категорию.
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	item, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title:             "Pre-save",
		CategoryCodes:     []string{"editor"},
		ProfileCategories: []string{"editor"}, // form-state
		Images:            []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("AddPortfolioPhotoSet: %v", err)
	}
	if !contains(item.CategoryCodes, "editor") {
		t.Errorf("category not persisted: %v", item.CategoryCodes)
	}
}

func TestPhotoSet_Create_HardLimit20PerUser(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if _, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
			Title:  fmt.Sprintf("Set %d", i),
			Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, fmt.Sprintf("s%d.jpg", i))}},
		}); err != nil {
			t.Fatalf("add #%d: %v", i+1, err)
		}
	}
	_, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "21st",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "21.jpg")}},
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for 21st set, got %v", err)
	}
}

func TestPhotoSet_Create_RejectsEmptyImageURL(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	_, err := svc.AddPortfolioPhotoSet(context.Background(), uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Empty url",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: ""},
		},
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for empty image_url, got %v", err)
	}
}

// ─── DeletePortfolioImage ─────────────────────────────────────────

func TestPhotoSet_DeleteImage_HappyPath(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	img1, img2, img3 := bucketURL(uid, "a.jpg"), bucketURL(uid, "b.jpg"), bucketURL(uid, "c.jpg")
	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Three",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: img1}, {ImageURL: img2}, {ImageURL: img3},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Удаляем middle (img2): cover (=img1) не меняется, остаются 2 фото.
	if err := svc.DeletePortfolioImage(ctx, uid, item.Images[1].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !portfolioItemExists(t, pool, item.ID) {
		t.Fatal("parent removed after middle delete")
	}
	left := listPhotoSetImages(t, pool, item.ID)
	if len(left) != 2 {
		t.Fatalf("left count = %d, want 2", len(left))
	}
	if portfolioItemCover(t, pool, item.ID) != img1 {
		t.Errorf("cover should stay img1, got %q", portfolioItemCover(t, pool, item.ID))
	}
}

func TestPhotoSet_DeleteImage_UpdatesCoverWhenCoverRemoved(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	img1, img2 := bucketURL(uid, "a.jpg"), bucketURL(uid, "b.jpg")
	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Two",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: img1}, {ImageURL: img2}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Удаляем cover (img1) — thumbnail_url должен обновиться на img2.
	if err := svc.DeletePortfolioImage(ctx, uid, item.Images[0].ID); err != nil {
		t.Fatalf("delete cover: %v", err)
	}
	if cover := portfolioItemCover(t, pool, item.ID); cover != img2 {
		t.Errorf("cover = %q, want %q (img2)", cover, img2)
	}
}

func TestPhotoSet_DeleteImage_LastImageCascadesParent(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Lonely",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "lonely.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeletePortfolioImage(ctx, uid, item.Images[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if portfolioItemExists(t, pool, item.ID) {
		t.Fatal("parent should be removed when last image deleted")
	}
}

func TestPhotoSet_DeleteImage_ForeignNotFound(t *testing.T) {
	pool := integration.Pool(t)
	owner := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, owner)
	intruder := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, intruder)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, owner, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Mine",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(owner, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// intruder пытается удалить чужой кадр — должен получить ErrNotFound.
	err = svc.DeletePortfolioImage(ctx, intruder, item.Images[0].ID)
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-user delete, got %v", err)
	}
	// Кадр на месте у владельца.
	if got := listPhotoSetImages(t, pool, item.ID); len(got) != 1 {
		t.Errorf("image was deleted by intruder: count=%d", len(got))
	}
}

func TestPhotoSet_DeleteImage_UnknownIDReturnsNotFound(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)

	err := svc.DeletePortfolioImage(context.Background(), uid, uuid.New())
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestPhotoSet_DeleteImage_ConcurrentLastTwoRemovesParent — regression
// для concurrency-бага: параллельное удаление последних двух фото оба
// раза увидит count=1 в своём snapshot'е и оба сделают UPDATE cover
// вместо DELETE parent. Без row-lock на профиль сет останется в БД
// с 0 фото. С lock'ом — одна tx ждёт, потом видит count=0 → DELETE parent.
func TestPhotoSet_DeleteImage_ConcurrentLastTwoRemovesParent(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Race",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(uid, "a.jpg")},
			{ImageURL: bucketURL(uid, "b.jpg")},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id1, id2 := item.Images[0].ID, item.Images[1].ID

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	for _, imgID := range []uuid.UUID{id1, id2} {
		go func(id uuid.UUID) {
			defer wg.Done()
			if err := svc.DeletePortfolioImage(ctx, uid, id); err != nil {
				errs <- err
			}
		}(imgID)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent delete: %v", e)
	}

	// Главное: parent НЕ должен висеть пустым.
	if portfolioItemExists(t, pool, item.ID) {
		left := listPhotoSetImages(t, pool, item.ID)
		t.Errorf("parent still exists after both images deleted (left=%d images) — race not fixed",
			len(left))
	}
}

// helper, дубль локального contains() (DedupStrings помощник не подходит)
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ─── AppendPhotosToSet ────────────────────────────────────────────

func TestPhotoSet_Append_HappyPath(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Initial",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	origUpdatedAt := item.UpdatedAt

	// Wait чуть-чуть чтобы updated_at гарантированно изменился
	// (postgres now() имеет microsecond precision, но на быстром CI бывает совпадение).
	time.Sleep(2 * time.Millisecond)

	res, err := svc.AppendPhotosToSet(ctx, uid, item.ID, []profiles.PortfolioPhotoRef{
		{ImageURL: bucketURL(uid, "b.jpg")},
		{ImageURL: bucketURL(uid, "c.jpg")},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(res.Images) != 3 {
		t.Errorf("images count = %d, want 3", len(res.Images))
	}
	// updated_at parent должен обновиться — frontend держит optimistic-lock
	// snapshot, без bump'a следующий PATCH meta получит 409.
	if !res.UpdatedAt.After(origUpdatedAt) {
		t.Errorf("parent.updated_at not bumped: orig=%v new=%v", origUpdatedAt, res.UpdatedAt)
	}
	// sort_order последовательный — append добавил после существующих.
	dbImages := listPhotoSetImages(t, pool, item.ID)
	want := []string{bucketURL(uid, "a.jpg"), bucketURL(uid, "b.jpg"), bucketURL(uid, "c.jpg")}
	for i := range want {
		if dbImages[i] != want[i] {
			t.Errorf("[%d]: got %s want %s", i, dbImages[i], want[i])
		}
	}
}

func TestPhotoSet_Append_RejectsExternalURL(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "X",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.AppendPhotosToSet(ctx, uid, item.ID, []profiles.PortfolioPhotoRef{
		{ImageURL: "https://evil.example.com/x.jpg"},
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for external URL, got %v", err)
	}
}

func TestPhotoSet_Append_RejectsEmptyImages(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "X",
		Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.AppendPhotosToSet(ctx, uid, item.ID, nil)
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for empty, got %v", err)
	}
}

func TestPhotoSet_Append_LimitExceeded(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	// Создаём set с 10 фото (лимит).
	imgs := make([]profiles.PortfolioPhotoRef, 10)
	for i := range imgs {
		imgs[i] = profiles.PortfolioPhotoRef{ImageURL: bucketURL(uid, fmt.Sprintf("p%d.jpg", i))}
	}
	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Full", Images: imgs,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.AppendPhotosToSet(ctx, uid, item.ID, []profiles.PortfolioPhotoRef{
		{ImageURL: bucketURL(uid, "extra.jpg")},
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for 11th photo, got %v", err)
	}
}

func TestPhotoSet_Append_RejectsForeignOwner(t *testing.T) {
	pool := integration.Pool(t)
	owner := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, owner)
	intruder := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, intruder)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, owner, profiles.PortfolioPhotoSetCreateInput{
		Title: "Mine", Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(owner, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.AppendPhotosToSet(ctx, intruder, item.ID, []profiles.PortfolioPhotoRef{
		{ImageURL: bucketURL(intruder, "x.jpg")},
	})
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-user append, got %v", err)
	}
}

func TestPhotoSet_Append_RejectsOnVideoItem(t *testing.T) {
	// Append работает только для kind='image' — на видео-айтеме отказ.
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	video, err := svc.AddPortfolioVideo(ctx, uid, profiles.PortfolioCreateInput{
		VideoURL: bucketURL(uid, "v.mp4"),
		Title:    "Video",
	})
	if err != nil {
		t.Fatalf("create video: %v", err)
	}
	_, err = svc.AppendPhotosToSet(ctx, uid, video.ID, []profiles.PortfolioPhotoRef{
		{ImageURL: bucketURL(uid, "a.jpg")},
	})
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound для kind=video, got %v", err)
	}
}

// ─── ReorderSetPhotos ─────────────────────────────────────────────

func TestPhotoSet_Reorder_HappyPath(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "ABC",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(uid, "a.jpg")},
			{ImageURL: bucketURL(uid, "b.jpg")},
			{ImageURL: bucketURL(uid, "c.jpg")},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	origUpdatedAt := item.UpdatedAt
	a, b, c := item.Images[0].ID, item.Images[1].ID, item.Images[2].ID

	time.Sleep(2 * time.Millisecond)
	// Реверсим: [c, b, a]
	res, err := svc.ReorderSetPhotos(ctx, uid, item.ID, []uuid.UUID{c, b, a})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	// Sort_order: c=0, b=1, a=2
	if res.Images[0].ID != c || res.Images[1].ID != b || res.Images[2].ID != a {
		t.Errorf("order wrong: %v %v %v", res.Images[0].ID, res.Images[1].ID, res.Images[2].ID)
	}
	// Cover thumbnail обновлён на новое первое (c.jpg).
	cover := portfolioItemCover(t, pool, item.ID)
	if cover != bucketURL(uid, "c.jpg") {
		t.Errorf("cover = %q, want %q", cover, bucketURL(uid, "c.jpg"))
	}
	// updated_at бумпнулся.
	if !res.UpdatedAt.After(origUpdatedAt) {
		t.Errorf("updated_at not bumped: orig=%v new=%v", origUpdatedAt, res.UpdatedAt)
	}
}

func TestPhotoSet_Reorder_RejectsLengthMismatch(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Two",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(uid, "a.jpg")},
			{ImageURL: bucketURL(uid, "b.jpg")},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Только 1 id вместо 2 — defense против потери кадра в reorder.
	_, err = svc.ReorderSetPhotos(ctx, uid, item.ID, []uuid.UUID{item.Images[0].ID})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for length mismatch, got %v", err)
	}
}

func TestPhotoSet_Reorder_RejectsForeignImageID(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "Two",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(uid, "a.jpg")},
			{ImageURL: bucketURL(uid, "b.jpg")},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 2 ID правильной длины, но второй — чужой UUID.
	_, err = svc.ReorderSetPhotos(ctx, uid, item.ID, []uuid.UUID{item.Images[0].ID, uuid.New()})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for foreign image_id, got %v", err)
	}
}

func TestPhotoSet_Reorder_RejectsForeignOwner(t *testing.T) {
	pool := integration.Pool(t)
	owner := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, owner)
	intruder := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, intruder)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, owner, profiles.PortfolioPhotoSetCreateInput{
		Title: "Two",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(owner, "a.jpg")},
			{ImageURL: bucketURL(owner, "b.jpg")},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.ReorderSetPhotos(ctx, intruder, item.ID, []uuid.UUID{item.Images[1].ID, item.Images[0].ID})
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-user reorder, got %v", err)
	}
}

// TestPhotoSet_Race_DeleteVsReorder — параллельные delete и reorder одного
// сета должны сериализоваться через FOR UPDATE на parent_item. Без этого
// reorder мог пытаться SET sort_order на уже удалённую запись (0 rows update)
// и оставлять пропуски в sort_order.
func TestPhotoSet_Race_DeleteVsReorder(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title: "ABCD",
		Images: []profiles.PortfolioPhotoRef{
			{ImageURL: bucketURL(uid, "a.jpg")},
			{ImageURL: bucketURL(uid, "b.jpg")},
			{ImageURL: bucketURL(uid, "c.jpg")},
			{ImageURL: bucketURL(uid, "d.jpg")},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a, b, c, d := item.Images[0].ID, item.Images[1].ID, item.Images[2].ID, item.Images[3].ID

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	// Тx1: delete B
	go func() {
		defer wg.Done()
		if err := svc.DeletePortfolioImage(ctx, uid, b); err != nil {
			errs <- fmt.Errorf("delete: %w", err)
		}
	}()
	// Тx2: reorder включая B (которое могут удалить параллельно)
	go func() {
		defer wg.Done()
		_, err := svc.ReorderSetPhotos(ctx, uid, item.ID, []uuid.UUID{d, c, b, a})
		// Допустимы оба исхода:
		//  • reorder выполнился ПЕРВЫМ → потом delete снёс B
		//  • delete первым → reorder увидел длину 3 vs 4 ids → ErrInvalidInput
		if err != nil && !errors.Is(err, profiles.ErrInvalidInput) {
			errs <- fmt.Errorf("reorder: %w", err)
		}
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("race produced unexpected err: %v", e)
	}
	// Invariants:
	//   • 3 фото осталось (после delete B из 4)
	//   • sort_order строго возрастает (нет дубликатов/инверсий)
	//   • parent_item не удалён (≥1 фото остаётся)
	// Дыры в sort_order (например [0,1,3]) допустимы — фронт читает
	// ORDER BY sort_order и видит правильный relative order. Renumeration
	// после каждого delete была бы избыточной.
	rows, err := pool.Query(ctx,
		`SELECT sort_order FROM portfolio_images WHERE portfolio_item_id = $1 ORDER BY sort_order`,
		item.ID)
	if err != nil {
		t.Fatalf("query sort_order: %v", err)
	}
	defer rows.Close()
	var orders []int
	for rows.Next() {
		var o int
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		orders = append(orders, o)
	}
	if len(orders) != 3 {
		t.Errorf("expected 3 images after delete of 1, got %d (orders=%v)", len(orders), orders)
	}
	for i := 1; i < len(orders); i++ {
		if orders[i] <= orders[i-1] {
			t.Errorf("sort_order not strictly increasing at index %d: %v", i, orders)
			break
		}
	}
}

// ─── UpdatePortfolioMeta ──────────────────────────────────────────

func TestPhotoSet_UpdateMeta_HappyPath(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:       "Old",
		Description: "Old desc",
		Images:      []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	newTitle := "New title"
	newDesc := "New description"
	updated, err := svc.UpdatePortfolio(ctx, uid, item.ID, profiles.PortfolioPatchInput{
		Title: &newTitle, Description: &newDesc,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Title != newTitle {
		t.Errorf("title = %q, want %q", updated.Title, newTitle)
	}
	if updated.Description != newDesc {
		t.Errorf("desc = %q, want %q", updated.Description, newDesc)
	}
}

func TestPhotoSet_UpdateMeta_RejectsEmptyTitle(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "T", Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	empty := "   " // trim → пустая строка
	_, err = svc.UpdatePortfolio(ctx, uid, item.ID, profiles.PortfolioPatchInput{Title: &empty})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for empty title, got %v", err)
	}
}

func TestPhotoSet_UpdateMeta_RejectsForeignOwner(t *testing.T) {
	pool := integration.Pool(t)
	owner := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, owner)
	intruder := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, intruder)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, owner, profiles.PortfolioPhotoSetCreateInput{
		Title: "Owner's", Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(owner, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	hijack := "Hacked"
	_, err = svc.UpdatePortfolio(ctx, intruder, item.ID, profiles.PortfolioPatchInput{Title: &hijack})
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-user, got %v", err)
	}
}

func TestPhotoSet_UpdateMeta_NoFieldsNoop(t *testing.T) {
	// Без полей — сервис возвращает текущий item без UPDATE'а в БД.
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := newPhotoSetSvc(t)
	ctx := context.Background()

	item, err := svc.AddPortfolioPhotoSet(ctx, uid, profiles.PortfolioPhotoSetCreateInput{
		Title:  "Same", Images: []profiles.PortfolioPhotoRef{{ImageURL: bucketURL(uid, "a.jpg")}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.UpdatePortfolio(ctx, uid, item.ID, profiles.PortfolioPatchInput{})
	if err != nil {
		t.Fatalf("noop update: %v", err)
	}
	if got.Title != item.Title || got.UpdatedAt != item.UpdatedAt {
		t.Errorf("noop changed item: orig=%+v got=%+v", item, got)
	}
}

// ─── Username + handle resolver ───────────────────────────────────

func TestUsername_Validate(t *testing.T) {
	cases := []struct {
		in   string
		want error
	}{
		{"foxy", nil},
		{"foo_bar-1", nil},
		{"abc", nil},
		{"ab", profiles.ErrInvalidInput},
		{"a_too_long_string_more_than_30_chars_xxxxxxxx", profiles.ErrInvalidInput},
		{"WithCaps", profiles.ErrInvalidInput}, // не lowercase
		{"with spaces", profiles.ErrInvalidInput},
		{"with.dot", profiles.ErrInvalidInput},
		{"with@at", profiles.ErrInvalidInput},
		{"admin", profiles.ErrInvalidInput}, // reserved
		{"api", profiles.ErrInvalidInput},
		{"specialist", profiles.ErrInvalidInput},
		{"me", profiles.ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := profiles.ValidateUsername(tc.in)
			if tc.want == nil {
				if err != nil {
					t.Errorf("want nil, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestUsername_ResolveByHandle(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := profiles.NewService(profiles.NewRepo(pool))
	ctx := context.Background()

	// Сначала задаём username через PatchFull.
	handle := "fox-" + uuid.New().String()[:6]
	if _, err := svc.PatchFull(ctx, uid, profiles.PatchFullInput{Username: &handle}); err != nil {
		t.Fatalf("set username: %v", err)
	}
	// Resolve работает.
	got, err := svc.ResolveUserIDByUsername(ctx, handle)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != uid {
		t.Errorf("resolved uid = %s, want %s", got, uid)
	}
	// Case-insensitive — пишем в БД lowercase, ищем как угодно.
	got2, err := svc.ResolveUserIDByUsername(ctx, strings.ToUpper(handle))
	if err != nil {
		t.Fatalf("resolve upper: %v", err)
	}
	if got2 != uid {
		t.Errorf("resolve case-insensitive failed: got %s, want %s", got2, uid)
	}
}

func TestUsername_ResolveNotFound(t *testing.T) {
	pool := integration.Pool(t)
	_ = pool
	svc := profiles.NewService(profiles.NewRepo(integration.Pool(t)))
	_, err := svc.ResolveUserIDByUsername(context.Background(), "nope-"+uuid.New().String()[:8])
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUsername_UniqueConflict(t *testing.T) {
	pool := integration.Pool(t)
	uid1 := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid1)
	uid2 := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid2)
	svc := profiles.NewService(profiles.NewRepo(pool))
	ctx := context.Background()

	handle := "shared-" + uuid.New().String()[:6]
	if _, err := svc.PatchFull(ctx, uid1, profiles.PatchFullInput{Username: &handle}); err != nil {
		t.Fatalf("set #1: %v", err)
	}
	_, err := svc.PatchFull(ctx, uid2, profiles.PatchFullInput{Username: &handle})
	if !errors.Is(err, profiles.ErrUsernameTaken) {
		t.Fatalf("want ErrUsernameTaken, got %v", err)
	}
}

func TestUsername_ResetToEmpty(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := profiles.NewService(profiles.NewRepo(pool))
	ctx := context.Background()

	handle := "fox-" + uuid.New().String()[:6]
	if _, err := svc.PatchFull(ctx, uid, profiles.PatchFullInput{Username: &handle}); err != nil {
		t.Fatalf("set: %v", err)
	}
	empty := ""
	p, err := svc.PatchFull(ctx, uid, profiles.PatchFullInput{Username: &empty})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if p.Username != "" {
		t.Errorf("username after reset = %q, want empty", p.Username)
	}
	// Resolve по старому handle больше не находит.
	if _, err := svc.ResolveUserIDByUsername(ctx, handle); !errors.Is(err, profiles.ErrNotFound) {
		t.Errorf("old handle still resolves: %v", err)
	}
}

// ─── Owner-preview vs public ──────────────────────────────────────

func TestGetPublic_StrictHidesUnpublished(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := profiles.NewService(profiles.NewRepo(pool))

	// makePhotoSetSpec не публикует профиль → strict 404.
	_, err := svc.GetPublic(context.Background(), uid)
	if !errors.Is(err, profiles.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetPublicForOwner_ReturnsUnpublished(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := profiles.NewService(profiles.NewRepo(pool))

	// Owner-mode возвращает профиль независимо от is_published/moderation.
	p, err := svc.GetPublicForOwner(context.Background(), uid)
	if err != nil {
		t.Fatalf("owner preview: %v", err)
	}
	if p.UserID != uid {
		t.Errorf("uid = %s, want %s", p.UserID, uid)
	}
	if p.IsPublished {
		t.Errorf("IsPublished = true, want false (только что созданный профиль)")
	}
}

func TestGetPublic_ShowsApproved(t *testing.T) {
	pool := integration.Pool(t)
	uid := makePhotoSetSpec(t, pool, "editor")
	defer cleanupSpec(t, pool, uid)
	svc := profiles.NewService(profiles.NewRepo(pool))
	ctx := context.Background()

	// Эмулируем publish + admin approve.
	if _, err := svc.SetPublished(ctx, uid, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE specialist_profiles SET moderation_status='approved', moderation_reviewed_at=now() WHERE user_id=$1`,
		uid); err != nil {
		t.Fatalf("approve: %v", err)
	}
	p, err := svc.GetPublic(ctx, uid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.UserID != uid {
		t.Errorf("uid mismatch")
	}
}

