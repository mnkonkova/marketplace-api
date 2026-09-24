package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// «Это ваш ролик?» — находка сама ничего не меняет.
//
// Сервис видит ролик на аккаунте раньше, чем человек успевает вставить
// ссылку. Привязать его молча нельзя: одна ошибка сопоставления — и
// креатору приписана чужая работа, о чём узнают из счёта. Поэтому
// находка ждёт ответа, а «да, мой» идёт той же дорогой, что и ссылка,
// вставленная руками.
func TestSuggestionLinkedByCreatorBecomesRealLink(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	batch, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("выкладок %d, ожидалась одна", len(batch.Items))
	}

	published := time.Now().UTC().Add(-24 * time.Hour)
	found, err := svc.AddSuggestion(ctx, publications.AddSuggestionInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		URL:           "https://www.tiktok.com/@anya.kim/video/7300000000000000001?utm_source=x",
		Title:         "Раф без сахара — можно?",
		AuthorHandle:  "@anya.kim",
		PublishedAt:   &published,
	})
	if err != nil {
		t.Fatalf("AddSuggestion: %v", err)
	}
	if found.SuggestedPublicationID == nil || *found.SuggestedPublicationID != batch.Items[0].ID {
		t.Fatalf("подсказка указала на %v, ожидалась выкладка %s",
			found.SuggestedPublicationID, batch.Items[0].ID)
	}
	if found.Platform != "tiktok" {
		t.Errorf("площадка %q, ожидался tiktok", found.Platform)
	}

	token := h.Token(t, creators[0])
	code, body := h.Do(t, http.MethodGet, "/api/v1/me/creator/suggestions", token, nil)
	if code != http.StatusOK {
		t.Fatalf("список находок: код %d, тело %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("находок %d, ожидалась одна: %v", len(items), body)
	}

	code, body = h.Do(t, http.MethodPost,
		"/api/v1/me/creator/suggestions/"+found.ID.String()+"/link", token, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("привязка: код %d, тело %v", code, body)
	}

	// Ссылка сдана по-настоящему, с канонизацией: рекламный хвост снят.
	var canonical string
	if err := pool.QueryRow(ctx, `
SELECT url_canonical FROM publication_links WHERE publication_id = $1 AND platform = 'tiktok'`,
		batch.Items[0].ID).Scan(&canonical); err != nil {
		t.Fatalf("ссылка не сдана: %v", err)
	}
	if canonical != "https://www.tiktok.com/@anya.kim/video/7300000000000000001" {
		t.Errorf("канонический адрес %q — хвост не снят", canonical)
	}

	// Находка ушла из списка: отвечать на неё второй раз не надо.
	code, body = h.Do(t, http.MethodGet, "/api/v1/me/creator/suggestions", token, nil)
	if code != http.StatusOK {
		t.Fatalf("список после привязки: код %d", code)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("привязанная находка осталась в списке: %v", items)
	}
}

// «Не мой» держится: обход аккаунта идёт каждый день, и отвергнутая
// находка не должна возвращаться завтра той же карточкой.
func TestDismissedSuggestionDoesNotComeBack(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	in := publications.AddSuggestionInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		URL:           "https://www.tiktok.com/@anya.kim/video/7300000000000000002",
	}
	found, err := svc.AddSuggestion(ctx, in)
	if err != nil {
		t.Fatalf("AddSuggestion: %v", err)
	}

	token := h.Token(t, creators[0])
	code, body := h.Do(t, http.MethodDelete,
		"/api/v1/me/creator/suggestions/"+found.ID.String(), token, nil)
	if code != http.StatusNoContent {
		t.Fatalf("«не мой»: код %d, тело %v", code, body)
	}

	// Тот же ролик на следующем обходе.
	again, err := svc.AddSuggestion(ctx, in)
	if err != nil {
		t.Fatalf("повторная находка: %v", err)
	}
	if again.ID != found.ID {
		t.Errorf("повторный обход завёл вторую карточку: %s вместо %s", again.ID, found.ID)
	}
	code, body = h.Do(t, http.MethodGet, "/api/v1/me/creator/suggestions", token, nil)
	if code != http.StatusOK {
		t.Fatalf("список: код %d", code)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("отвергнутая находка вернулась: %v", items)
	}

	// И ответить на неё второй раз нельзя.
	code, _ = h.Do(t, http.MethodDelete,
		"/api/v1/me/creator/suggestions/"+found.ID.String(), token, nil)
	if code != http.StatusConflict {
		t.Errorf("повторный отказ: код %d, ожидался 409", code)
	}
}

// Чужая находка — «не найдено», а не «нельзя»: 403 подтвердил бы
// постороннему, что такая карточка существует.
func TestForeignSuggestionIsNotFound(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	found, err := svc.AddSuggestion(ctx, publications.AddSuggestionInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		URL:           "https://www.tiktok.com/@anya.kim/video/7300000000000000003",
	})
	if err != nil {
		t.Fatalf("AddSuggestion: %v", err)
	}

	code, body := h.Do(t, http.MethodDelete,
		"/api/v1/me/creator/suggestions/"+found.ID.String(), h.Token(t, creators[1]), nil)
	if code != http.StatusNotFound {
		t.Fatalf("чужая находка: код %d, ожидался 404. тело: %v", code, body)
	}
}

// Площадка уже сдана — предлагать к этой выкладке нечего: подсказка
// отдаётся без кандидата, а не указывает на занятый слот.
func TestSuggestionSkipsPublicationWithThatPlatform(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	batch, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := svc.SubmitLinks(ctx, publications.SubmitLinksInput{
		PublicationID: batch.Items[0].ID,
		ActorUserID:   creators[0],
		URLs:          []string{"https://www.tiktok.com/@anya.kim/video/7300000000000000010"},
	}); err != nil {
		t.Fatalf("SubmitLinks: %v", err)
	}

	found, err := svc.AddSuggestion(ctx, publications.AddSuggestionInput{
		ProjectID:     projectID,
		CreatorUserID: creators[0],
		URL:           "https://www.tiktok.com/@anya.kim/video/7300000000000000011",
	})
	if err != nil {
		t.Fatalf("AddSuggestion: %v", err)
	}
	if found.SuggestedPublicationID != nil {
		t.Errorf("подсказка указала на выкладку, где TikTok уже сдан: %v",
			found.SuggestedPublicationID)
	}
}
