package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// mountPublications — те же пути, что в internal/httpapi/router.go, но без
// middleware: проверку роли делает роутер, а здесь проверяется то, что
// роутер сделать не может — что ручка берёт креатора из токена, а не из
// параметров запроса, и что ошибки домена превращаются в понятный HTTP.
func mountPublications(h *publications.Handler, actor uuid.UUID) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), actor)))
		})
	})
	r.Get("/me/creator/projects/{id}/publications", h.CreatorList)
	r.Post("/me/creator/publications/{pub_id}/links", h.CreatorSubmitLinks)
	r.Post("/me/creator/publications/{pub_id}/date_request", h.CreatorRequestDateChange)
	r.Get("/manager/projects/{id}/publications", h.ManagerList)
	r.Post("/manager/projects/{id}/publications/batch", h.ManagerCreateBatch)
	r.Post("/manager/projects/{id}/publications/preview", h.ManagerPreviewBatch)
	r.Post("/manager/publications/{pub_id}/close", h.ManagerClosePublication)
	return r
}

func doJSON(t *testing.T, srv http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// Ключевая проверка границы доступа на уровне HTTP: креатор берётся из
// токена. Даже зная id проекта, чужие выкладки через эту ручку не достать.
func TestHTTPCreatorListUsesTokenNotParams(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creators,
		Dates:          []time.Time{pubDay(0), pubDay(1)},
		CreatedBy:      creators[0],
	}); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	h := publications.NewHandler(svc)

	// Первый креатор видит только свои две выкладки из четырёх.
	code, body := doJSON(t, mountPublications(h, creators[0]), http.MethodGet,
		"/me/creator/projects/"+projectID.String()+"/publications", nil)
	if code != http.StatusOK {
		t.Fatalf("GET publications: код %d, тело %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("креатор получил %d выкладок, ожидалось 2", len(items))
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["creator_user_id"] != creators[0].String() {
			t.Errorf("в ответе чужая выкладка: %v", m["creator_user_id"])
		}
	}

	// Менеджерская ручка на том же проекте отдаёт все четыре.
	code, body = doJSON(t, mountPublications(h, creators[0]), http.MethodGet,
		"/manager/projects/"+projectID.String()+"/publications", nil)
	if code != http.StatusOK {
		t.Fatalf("GET manager publications: код %d", code)
	}
	if all, _ := body["items"].([]any); len(all) != 4 {
		t.Fatalf("менеджер получил %d выкладок, ожидалось 4", len(all))
	}
}

// Чужая выкладка отдаёт 404, а не 403: сообщать «она есть, но не ваша» —
// значит подтверждать её существование постороннему.
func TestHTTPSubmitForeignPublicationIs404(t *testing.T) {
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
	h := publications.NewHandler(svc)

	// Сдаёт второй креатор — тот, на кого выкладка не заведена.
	code, body := doJSON(t, mountPublications(h, creators[1]), http.MethodPost,
		"/me/creator/publications/"+res.Items[0].ID.String()+"/links",
		map[string]any{"urls": []string{"https://www.tiktok.com/@u/video/1"}})
	if code != http.StatusNotFound {
		t.Fatalf("чужая выкладка: код %d, ожидался 404. тело %v", code, body)
	}
	if body["error"] != "not_found" {
		t.Errorf("код ошибки %v, ожидался not_found", body["error"])
	}
}

// Карта ошибок домена в HTTP. Забытая ветка отдаёт 500 там, где человеку
// нужно понятное объяснение, — поэтому проверяем и статус, и код.
func TestHTTPErrorMapping(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	repo := publications.NewRepo(pool)
	svc := publications.NewService(repo)
	h := publications.NewHandler(svc)
	srv := mountPublications(h, creators[0])

	tplID := createChecklistTemplate(t, pool)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, tplID) }()
	if _, err := svc.SnapshotChecklist(ctx, projectID, tplID, creators[0]); err != nil {
		t.Fatalf("SnapshotChecklist: %v", err)
	}

	res, err := svc.CreateBatch(ctx, publications.CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: []uuid.UUID{creators[0]},
		Dates:          []time.Time{pubDay(0)},
		CreatedBy:      creators[0],
	})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	pubID := res.Items[0].ID.String()

	cases := []struct {
		name     string
		method   string
		path     string
		body     any
		wantCode int
		wantErr  string
	}{
		{
			name:   "чеклист не отмечен",
			method: http.MethodPost, path: "/me/creator/publications/" + pubID + "/links",
			body:     map[string]any{"urls": []string{"https://www.tiktok.com/@u/video/1"}},
			wantCode: http.StatusUnprocessableEntity, wantErr: "checklist_incomplete",
		},
		{
			name:   "ссылка не с той площадки",
			method: http.MethodPost, path: "/me/creator/publications/" + pubID + "/links",
			body:     map[string]any{"urls": []string{"https://rutube.ru/video/abc/"}},
			wantCode: http.StatusBadRequest, wantErr: "unknown_platform",
		},
		{
			name:   "ни одной ссылки",
			method: http.MethodPost, path: "/me/creator/publications/" + pubID + "/links",
			body:     map[string]any{"urls": []string{}},
			wantCode: http.StatusBadRequest, wantErr: "no_links",
		},
		{
			name:   "закрытие без причины",
			method: http.MethodPost, path: "/manager/publications/" + pubID + "/close",
			body:     map[string]any{"reason": "  "},
			wantCode: http.StatusBadRequest, wantErr: "invalid_input",
		},
		{
			name:   "перенос в прошлое",
			method: http.MethodPost, path: "/me/creator/publications/" + pubID + "/date_request",
			body:     map[string]any{"requested_date": "2020-01-01", "reason": "так вышло"},
			wantCode: http.StatusBadRequest, wantErr: "invalid_input",
		},
		{
			name:   "неизвестная схема дат",
			method: http.MethodPost, path: "/manager/projects/" + projectID.String() + "/publications/preview",
			body:     map[string]any{"scheme": "по настроению", "from": "2026-09-01", "to": "2026-09-30"},
			wantCode: http.StatusBadRequest, wantErr: "bad_schedule",
		},
		{
			name:   "битая дата в диапазоне",
			method: http.MethodPost, path: "/manager/projects/" + projectID.String() + "/publications/preview",
			body:     map[string]any{"scheme": "daily", "from": "01.09.2026", "to": "2026-09-30"},
			wantCode: http.StatusBadRequest, wantErr: "invalid_input",
		},
		{
			name:   "выкладки не найдено",
			method: http.MethodPost, path: "/me/creator/publications/" + uuid.NewString() + "/links",
			body:     map[string]any{"urls": []string{"https://www.tiktok.com/@u/video/1"}},
			wantCode: http.StatusNotFound, wantErr: "not_found",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := doJSON(t, srv, c.method, c.path, c.body)
			if code != c.wantCode {
				t.Errorf("код %d, ожидался %d. тело: %v", code, c.wantCode, body)
			}
			if got, _ := body["error"].(string); got != c.wantErr {
				t.Errorf("код ошибки %q, ожидался %q", got, c.wantErr)
			}
			if msg, _ := body["message"].(string); msg == "" {
				t.Error("в ответе нет человеческого объяснения")
			}
		})
	}
}

// Предпросмотр обязан показывать ровно то, что создаст батч: иначе он
// бесполезен как защита от ошибки в схеме.
func TestHTTPPreviewMatchesBatch(t *testing.T) {
	pool := integration.Pool(t)
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	h := publications.NewHandler(publications.NewService(publications.NewRepo(pool)))
	srv := mountPublications(h, creators[0])

	req := map[string]any{
		"creator_user_ids": []string{creators[0].String(), creators[1].String()},
		"scheme":           "tue_thu",
		"from":             "2026-09-01",
		"to":               "2026-09-30",
	}

	code, body := doJSON(t, srv, http.MethodPost,
		"/manager/projects/"+projectID.String()+"/publications/preview", req)
	if code != http.StatusOK {
		t.Fatalf("preview: код %d, тело %v", code, body)
	}
	previewTotal, _ := body["total"].(float64)
	previewDates, _ := body["dates"].([]any)
	if len(previewDates) != 9 || int(previewTotal) != 18 {
		t.Fatalf("предпросмотр: %d дат, всего %v; ожидалось 9 и 18",
			len(previewDates), previewTotal)
	}

	code, body = doJSON(t, srv, http.MethodPost,
		"/manager/projects/"+projectID.String()+"/publications/batch", req)
	if code != http.StatusCreated {
		t.Fatalf("batch: код %d, тело %v", code, body)
	}
	created, _ := body["created"].(float64)
	if int(created) != int(previewTotal) {
		t.Errorf("создано %v, а предпросмотр обещал %v", created, previewTotal)
	}
}
