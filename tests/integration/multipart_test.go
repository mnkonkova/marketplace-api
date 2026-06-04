package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// stubMedia — реализация MediaStorage без реального S3. Записывает все
// вызовы в полях, чтобы тесты могли проверять что бэк позвал нужное.
type stubMedia struct {
	uploadID    string
	startedKey  string
	startedCT   string
	startCalls  int
	partCalls   int
	completeKey string
	completeID  string
	completePts []profiles.MultipartPart
	abortKey    string
	abortID     string
}

func (s *stubMedia) PresignPut(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "https://stub/put", nil
}
func (s *stubMedia) PublicURL(key string) string { return "https://stub.test/" + key }
func (s *stubMedia) ListObjects(_ context.Context, _ string, _ func(string, time.Time) bool) error {
	return nil
}
func (s *stubMedia) RemoveObject(_ context.Context, _ string) error { return nil }
func (s *stubMedia) KeyFromURL(_ string) string                     { return "" }

func (s *stubMedia) NewMultipartUpload(_ context.Context, key, ct string) (string, error) {
	s.startCalls++
	s.startedKey = key
	s.startedCT = ct
	if s.uploadID == "" {
		s.uploadID = "upl-" + uuid.NewString()
	}
	return s.uploadID, nil
}
func (s *stubMedia) PresignPart(_ context.Context, _, _ string, _ int, _ time.Duration) (string, error) {
	s.partCalls++
	return "https://stub/part", nil
}
func (s *stubMedia) CompleteMultipart(_ context.Context, key, uploadID string, parts []profiles.MultipartPart) error {
	s.completeKey = key
	s.completeID = uploadID
	s.completePts = parts
	return nil
}
func (s *stubMedia) AbortMultipart(_ context.Context, key, uploadID string) error {
	s.abortKey = key
	s.abortID = uploadID
	return nil
}

// makeClient — заводит юзера-клиента и возвращает его id + cleanup.
func makeClient(t *testing.T) (uuid.UUID, func()) {
	t.Helper()
	pool := integration.Pool(t)
	email := "mp-" + uuid.NewString() + "@example.com"
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, 'x', 'specialist', TRUE, now()) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id, func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	}
}

func newProfilesSvc(t *testing.T) (*profiles.Service, *stubMedia) {
	pool := integration.Pool(t)
	media := &stubMedia{}
	svc := profiles.NewService(profiles.NewRepo(pool)).WithMediaStorage(media)
	return svc, media
}

// ---- StartPortfolioMultipart ----

func TestStartMultipart_RejectsUnknownContentType(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	_, err := svc.StartPortfolioMultipart(context.Background(), uid, profiles.PortfolioMultipartStartInput{
		Filename:    "x.mp4",
		ContentType: "image/png", // не video/*
		SizeBytes:   10 * 1024 * 1024,
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for image/png, got %v", err)
	}
}

func TestStartMultipart_RejectsZeroSize(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	_, err := svc.StartPortfolioMultipart(context.Background(), uid, profiles.PortfolioMultipartStartInput{
		Filename:    "x.mp4",
		ContentType: "video/mp4",
		SizeBytes:   0,
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for size=0, got %v", err)
	}
}

func TestStartMultipart_RejectsOversize(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	_, err := svc.StartPortfolioMultipart(context.Background(), uid, profiles.PortfolioMultipartStartInput{
		Filename:    "huge.mp4",
		ContentType: "video/mp4",
		SizeBytes:   500 * 1024 * 1024, // > 200 MB cap
	})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for oversize, got %v", err)
	}
}

func TestStartMultipart_HappyPath(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, media := newProfilesSvc(t)

	out, err := svc.StartPortfolioMultipart(context.Background(), uid, profiles.PortfolioMultipartStartInput{
		Filename:    "promo.mp4",
		ContentType: "video/mp4",
		SizeBytes:   30 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if out.UploadID == "" || out.Key == "" || out.PublicURL == "" {
		t.Errorf("empty fields: %+v", out)
	}
	if out.PartSize <= 0 {
		t.Errorf("part_size should be > 0, got %d", out.PartSize)
	}
	if media.startCalls != 1 {
		t.Errorf("want NewMultipartUpload called once, got %d", media.startCalls)
	}
	// Ключ должен начинаться с portfolio/{user_id}/ — это база для assertOwnedKey.
	wantPrefix := "portfolio/" + uid.String() + "/"
	if !strings.HasPrefix(out.Key, wantPrefix) {
		t.Errorf("key %q must start with %q", out.Key, wantPrefix)
	}
	if !strings.HasSuffix(out.Key, ".mp4") {
		t.Errorf("key %q must end with .mp4 (extension from content_type)", out.Key)
	}
	if media.startedCT != "video/mp4" {
		t.Errorf("content_type not passed to S3: %s", media.startedCT)
	}
}

// ---- PortfolioMultipartPartURL ----

func TestPartURL_RejectsForeignKey(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	other := uuid.New()
	_, err := svc.PortfolioMultipartPartURL(context.Background(), uid,
		profiles.PortfolioMultipartPartURLInput{
			Key:        "portfolio/" + other.String() + "/x.mp4",
			UploadID:   "abc",
			PartNumber: 1,
		})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for foreign key prefix, got %v", err)
	}
}

func TestPartURL_RejectsEmptyUploadID(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	_, err := svc.PortfolioMultipartPartURL(context.Background(), uid,
		profiles.PortfolioMultipartPartURLInput{
			Key:        "portfolio/" + uid.String() + "/x.mp4",
			UploadID:   "",
			PartNumber: 1,
		})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for empty upload_id, got %v", err)
	}
}

func TestPartURL_RejectsBadPartNumber(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	cases := []int{0, -1, 10001}
	for _, p := range cases {
		_, err := svc.PortfolioMultipartPartURL(context.Background(), uid,
			profiles.PortfolioMultipartPartURLInput{
				Key:        "portfolio/" + uid.String() + "/x.mp4",
				UploadID:   "abc",
				PartNumber: p,
			})
		if !errors.Is(err, profiles.ErrInvalidInput) {
			t.Errorf("partNumber=%d: want ErrInvalidInput, got %v", p, err)
		}
	}
}

func TestPartURL_HappyPath(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, media := newProfilesSvc(t)

	out, err := svc.PortfolioMultipartPartURL(context.Background(), uid,
		profiles.PortfolioMultipartPartURLInput{
			Key:        "portfolio/" + uid.String() + "/x.mp4",
			UploadID:   "abc",
			PartNumber: 3,
		})
	if err != nil {
		t.Fatalf("PartURL: %v", err)
	}
	if out.UploadURL == "" {
		t.Errorf("empty upload_url")
	}
	if media.partCalls != 1 {
		t.Errorf("PresignPart calls: want 1, got %d", media.partCalls)
	}
}

// ---- CompletePortfolioMultipart ----

func TestComplete_RejectsForeignKey(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	other := uuid.New()
	err := svc.CompletePortfolioMultipart(context.Background(), uid,
		profiles.PortfolioMultipartCompleteInput{
			Key:      "portfolio/" + other.String() + "/x.mp4",
			UploadID: "abc",
			Parts:    []profiles.PortfolioMultipartPart{{PartNumber: 1, ETag: "et"}},
		})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestComplete_RejectsEmptyParts(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	err := svc.CompletePortfolioMultipart(context.Background(), uid,
		profiles.PortfolioMultipartCompleteInput{
			Key:      "portfolio/" + uid.String() + "/x.mp4",
			UploadID: "abc",
			Parts:    nil,
		})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput for empty parts, got %v", err)
	}
}

func TestComplete_RejectsBadPartInList(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	cases := []struct {
		name  string
		parts []profiles.PortfolioMultipartPart
	}{
		{"empty etag", []profiles.PortfolioMultipartPart{{PartNumber: 1, ETag: ""}}},
		{"partNumber 0", []profiles.PortfolioMultipartPart{{PartNumber: 0, ETag: "et"}}},
		{"partNumber 10001", []profiles.PortfolioMultipartPart{{PartNumber: 10001, ETag: "et"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.CompletePortfolioMultipart(context.Background(), uid,
				profiles.PortfolioMultipartCompleteInput{
					Key:      "portfolio/" + uid.String() + "/x.mp4",
					UploadID: "abc",
					Parts:    tc.parts,
				})
			if !errors.Is(err, profiles.ErrInvalidInput) {
				t.Errorf("want ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestComplete_HappyPath(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, media := newProfilesSvc(t)

	parts := []profiles.PortfolioMultipartPart{
		{PartNumber: 1, ETag: "\"a\""},
		{PartNumber: 2, ETag: "\"b\""},
	}
	err := svc.CompletePortfolioMultipart(context.Background(), uid,
		profiles.PortfolioMultipartCompleteInput{
			Key:      "portfolio/" + uid.String() + "/x.mp4",
			UploadID: "upl",
			Parts:    parts,
		})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if media.completeID != "upl" {
		t.Errorf("upload_id: want upl, got %s", media.completeID)
	}
	if len(media.completePts) != 2 {
		t.Errorf("parts count: want 2, got %d", len(media.completePts))
	}
}

// ---- AbortPortfolioMultipart ----

func TestAbort_RejectsForeignKey(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, _ := newProfilesSvc(t)

	other := uuid.New()
	err := svc.AbortPortfolioMultipart(context.Background(), uid,
		profiles.PortfolioMultipartAbortInput{
			Key:      "portfolio/" + other.String() + "/x.mp4",
			UploadID: "abc",
		})
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestAbort_HappyPath(t *testing.T) {
	uid, cleanup := makeClient(t)
	defer cleanup()
	svc, media := newProfilesSvc(t)

	err := svc.AbortPortfolioMultipart(context.Background(), uid,
		profiles.PortfolioMultipartAbortInput{
			Key:      "portfolio/" + uid.String() + "/x.mp4",
			UploadID: "upl",
		})
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if media.abortID != "upl" {
		t.Errorf("abort upload_id: want upl, got %s", media.abortID)
	}
}
