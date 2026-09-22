package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Правка сданной ссылки менеджером.
//
// Ссылку сдаёт креатор, и ошибается в ней он же: чужой ролик, ссылка на
// профиль вместо видео, мобильный домен с обрезанным id. Исправить это
// мог только он сам — менеджер видел, что цифры не собираются, и писал
// в чат. Здесь проверяется, что правка работает и не склеивает историю
// двух разных роликов в одну линию.
func TestManagerEditLink(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID

	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: pubID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@u/video/111"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	// Замер по сданной ссылке: он и должен исчезнуть, когда ролик
	// подменят на другой.
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM publication_links WHERE publication_id = $1 AND platform = 'tiktok'`,
		pubID).Scan(&linkID); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO video_stat_daily (link_id, stat_date, views, likes, comments)
VALUES ($1, current_date, 1000, 10, 1)`, linkID); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	t.Run("другой ролик — ссылка меняется, замеры сбрасываются", func(t *testing.T) {
		got, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
			PublicationID: pubID,
			ManagerUserID: creators[0],
			Platform:      "tiktok",
			URL:           "https://www.tiktok.com/@u/video/222?is_from_webapp=1",
		})
		if err != nil {
			t.Fatalf("ManagerEditLink: %v", err)
		}
		if len(got.Links) != 1 || got.Links[0].Platform != "tiktok" {
			t.Fatalf("ожидали одну ссылку tiktok, получили %+v", got.Links)
		}

		var canonical string
		if err := pool.QueryRow(ctx,
			`SELECT url_canonical FROM publication_links WHERE id = $1`, linkID).Scan(&canonical); err != nil {
			t.Fatalf("read canonical: %v", err)
		}
		if canonical == "" || canonical == "https://www.tiktok.com/@u/video/111" {
			t.Fatalf("канонический адрес не обновился: %q", canonical)
		}

		var stats int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM video_stat_daily WHERE link_id = $1`, linkID).Scan(&stats); err != nil {
			t.Fatalf("count stats: %v", err)
		}
		if stats != 0 {
			t.Fatalf("замеры прежнего ролика остались: %d строк", stats)
		}
	})

	t.Run("ссылка другой площадки в чужой слот не ложится", func(t *testing.T) {
		_, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
			PublicationID: pubID,
			ManagerUserID: creators[0],
			Platform:      "tiktok",
			URL:           "https://youtu.be/dQw4w9WgXcQ",
		})
		if !errors.Is(err, publications.ErrInvalidInput) {
			t.Fatalf("ожидали ErrInvalidInput, получили %v", err)
		}
	})

	t.Run("мусор вместо ссылки не принимается", func(t *testing.T) {
		if _, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
			PublicationID: pubID,
			ManagerUserID: creators[0],
			Platform:      "tiktok",
			URL:           "не ссылка",
		}); err == nil {
			t.Fatal("ожидали ошибку разбора ссылки")
		}
	})

	t.Run("пустой url снимает ссылку, выкладка снова ждёт сдачи", func(t *testing.T) {
		got, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
			PublicationID: pubID,
			ManagerUserID: creators[0],
			Platform:      "tiktok",
			URL:           "",
		})
		if err != nil {
			t.Fatalf("ManagerEditLink(снять): %v", err)
		}
		if len(got.Links) != 0 {
			t.Fatalf("ссылка осталась: %+v", got.Links)
		}
		if got.Status != publications.StatusPlanned {
			t.Fatalf("статус %q, ожидали planned", got.Status)
		}
	})

	t.Run("снимать нечего — not_found", func(t *testing.T) {
		_, err := svc.ManagerEditLink(ctx, publications.ManagerEditLinkInput{
			PublicationID: pubID,
			ManagerUserID: creators[0],
			Platform:      "vk",
			URL:           "",
		})
		if !errors.Is(err, publications.ErrNotFound) {
			t.Fatalf("ожидали ErrNotFound, получили %v", err)
		}
	})

	t.Run("правка пишется в историю проекта", func(t *testing.T) {
		var events int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM outbox
WHERE aggregate = 'project' AND aggregate_id = $1 AND event_type = 'project.publication_link_edited'`,
			projectID.String()).Scan(&events); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if events == 0 {
			t.Fatal("событие правки ссылки в outbox не записано")
		}
	})
}
