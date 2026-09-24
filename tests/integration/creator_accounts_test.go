package integration_test

import (
	"context"
	"net/http"
	"testing"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// «Мои аккаунты» креатора — аккаунты ПРОЕКТА, а не личная страница.
//
// Под проект человек заводит отдельные аккаунты и ведёт их сам: менеджер
// не знает, с какого именно он решил выкладывать, и переписывание этого
// через чат — ровно тот шаг, ради устранения которого блок и нужен.
func TestCreatorManagesOwnProjectAccounts(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	token := h.Token(t, creators[0])
	base := "/api/v1/me/creator/projects/" + projectID.String() + "/accounts"

	code, body := h.Do(t, http.MethodPost, base, token, map[string]any{
		"platform": "tiktok",
		"title":    "Рабочий",
		"url":      "https://www.tiktok.com/@work",
		"login":    "work",
	})
	if code != http.StatusCreated {
		t.Fatalf("завести аккаунт: код %d, тело %v", code, body)
	}
	accountID, _ := body["id"].(string)
	if accountID == "" {
		t.Fatalf("сервер не вернул id: %v", body)
	}
	// Владелец проставляется сам: завести строку от чужого имени нельзя.
	if body["creator_user_id"] != creators[0].String() {
		t.Errorf("владелец %v, ожидался сам креатор", body["creator_user_id"])
	}

	code, body = h.Do(t, http.MethodGet, base, token, nil)
	if code != http.StatusOK {
		t.Fatalf("список: код %d, тело %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("аккаунтов %d, ожидался один: %v", len(items), body)
	}

	code, body = h.Do(t, http.MethodPut, base+"/"+accountID, token, map[string]any{
		"platform": "tiktok",
		"title":    "Рабочий",
		"url":      "https://www.tiktok.com/@work2",
		"login":    "work2",
	})
	if code != http.StatusOK {
		t.Fatalf("правка: код %d, тело %v", code, body)
	}
	if body["url"] != "https://www.tiktok.com/@work2" {
		t.Errorf("адрес не сохранился: %v", body["url"])
	}

	code, _ = h.Do(t, http.MethodDelete, base+"/"+accountID, token, nil)
	if code != http.StatusNoContent {
		t.Fatalf("удаление: код %d", code)
	}
}

// Чужой и брендовый доступ креатору не видны и не правятся.
//
// «Нет такой» и «не ваша» отвечают одинаково: разные ответы подтвердили
// бы постороннему, что доступ существует, — а в доступах лежат пароли
// заказчика.
func TestCreatorSeesOnlyOwnAccounts(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()
	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	// Брендовый доступ без владельца и аккаунт второго креатора.
	if _, err := svc.ManagerAddAccount(ctx, projectID, creators[0], publications.AccountInput{
		Platform: "other", Title: "Почта бренда", Login: "brand@example.com",
	}); err != nil {
		t.Fatalf("брендовый доступ: %v", err)
	}
	foreign, err := svc.ManagerAddAccount(ctx, projectID, creators[0], publications.AccountInput{
		CreatorUserID: &creators[1],
		Platform:      "vk", Title: "Лёва", URL: "https://vk.com/lev",
	})
	if err != nil {
		t.Fatalf("чужой доступ: %v", err)
	}

	token := h.Token(t, creators[0])
	base := "/api/v1/me/creator/projects/" + projectID.String() + "/accounts"

	code, body := h.Do(t, http.MethodGet, base, token, nil)
	if code != http.StatusOK {
		t.Fatalf("список: код %d", code)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("в «моих аккаунтах» чужое или брендовое: %v", items)
	}

	code, _ = h.Do(t, http.MethodPut, base+"/"+foreign.ID.String(), token, map[string]any{
		"platform": "vk", "url": "https://vk.com/hack",
	})
	if code != http.StatusNotFound {
		t.Errorf("правка чужого доступа: код %d, ожидался 404", code)
	}

	code, _ = h.Do(t, http.MethodGet, base+"/"+foreign.ID.String()+"/secret", token, nil)
	if code != http.StatusNotFound {
		t.Errorf("чужой пароль: код %d, ожидался 404", code)
	}
}

// Не в составе проекта — аккаунтов проекта не видно вовсе.
func TestCreatorOutsideProjectHasNoAccounts(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	projectID, _, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "specialist"})
	defer cleanupStranger()

	code, _ := h.Do(t, http.MethodGet,
		"/api/v1/me/creator/projects/"+projectID.String()+"/accounts", h.Token(t, stranger), nil)
	if code != http.StatusNotFound {
		t.Errorf("посторонний: код %d, ожидался 404", code)
	}
}
