package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"marketpclce/tests/integration"
)

// Документы: шаблоны с версиями и документы конкретному человеку.
//
// Главное, что здесь стережётся: документ одного человека не виден
// соседу по проекту, выданное не меняется задним числом, и ничего не
// удаляется — только архив и отзыв.

func docItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func hasTitle(items []map[string]any, title string) bool {
	for _, it := range items {
		if it["title"] == title {
			return true
		}
	}
	return false
}

// dropTemplates — уборка: шаблоны живут вне проекта, и проект их с
// собой не уносит. Версии держат RESTRICT — сначала выданное, потом они.
func dropTemplates(t *testing.T, ids ...string) {
	t.Helper()
	pool := integration.Pool(t)
	ctx := context.Background()
	for _, id := range ids {
		_, _ = pool.Exec(ctx, `DELETE FROM user_documents WHERE template_version_id IN
			(SELECT id FROM document_template_versions WHERE template_id = $1)`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM document_template_versions WHERE template_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM document_templates WHERE id = $1`, id)
	}
}

func TestDocumentTemplatesVersionsAndArchive(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true, IsManager: true})
	defer cleanupAdmin()
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	code, tpl := h.Do(t, http.MethodPost, "/api/v1/admin/document_templates", h.Token(t, admin),
		map[string]any{
			"kind": "contract", "title": "Договор с креатором (тест)", "audience": "creators",
			"url": "https://docs.example.com/contract-v1",
		})
	if code != http.StatusCreated {
		t.Fatalf("создание шаблона: код %d, тело %v", code, tpl)
	}
	id := tpl["id"].(string)
	defer dropTemplates(t, id)

	// Ссылка не http(s) — отказ: она попадёт в интерфейс кликабельной.
	code, _ = h.Do(t, http.MethodPost, "/api/v1/admin/document_templates/"+id+"/versions",
		h.Token(t, admin), map[string]any{"url": "javascript:alert(1)"})
	if code != http.StatusBadRequest {
		t.Errorf("версия с javascript:-ссылкой: код %d, ожидался 400", code)
	}

	code, v2 := h.Do(t, http.MethodPost, "/api/v1/admin/document_templates/"+id+"/versions",
		h.Token(t, admin), map[string]any{"url": "https://docs.example.com/contract-v2", "note": "новый срок оплаты"})
	if code != http.StatusCreated || v2["version"] != float64(2) {
		t.Fatalf("новая версия: код %d, тело %v", code, v2)
	}

	// Менеджер видит действующую версию — вторую.
	code, list := h.Do(t, http.MethodGet, "/api/v1/manager/document_templates?audience=creators",
		h.Token(t, manager), nil)
	if code != http.StatusOK {
		t.Fatalf("шаблоны менеджеру: код %d", code)
	}
	found := false
	for _, it := range docItems(t, list) {
		if it["id"] == id {
			found = true
			if cur, _ := it["current"].(map[string]any); cur["version"] != float64(2) {
				t.Errorf("действующая версия %v, ожидалась 2", cur["version"])
			}
		}
	}
	if !found {
		t.Fatal("менеджер не видит действующий шаблон")
	}

	// Архив: из выбора уходит, версию к нему не выпустить.
	if code, _ := h.Do(t, http.MethodPost, "/api/v1/admin/document_templates/"+id+"/archive",
		h.Token(t, admin), nil); code != http.StatusNoContent {
		t.Fatalf("в архив: код %d", code)
	}
	_, list = h.Do(t, http.MethodGet, "/api/v1/manager/document_templates", h.Token(t, manager), nil)
	for _, it := range docItems(t, list) {
		if it["id"] == id {
			t.Error("архивный шаблон остался в выборе у менеджера")
		}
	}
	code, _ = h.Do(t, http.MethodPost, "/api/v1/admin/document_templates/"+id+"/versions",
		h.Token(t, admin), map[string]any{"url": "https://docs.example.com/contract-v3"})
	if code != http.StatusConflict {
		t.Errorf("версия архивного шаблона: код %d, ожидался 409", code)
	}

	// В админке архив виден отдельно, с историей версий; не удалён.
	_, all := h.Do(t, http.MethodGet, "/api/v1/admin/document_templates?archived=1", h.Token(t, admin), nil)
	for _, it := range docItems(t, all) {
		if it["id"] != id {
			continue
		}
		if it["archived_at"] == nil {
			t.Error("у архивного шаблона нет archived_at")
		}
		if vs, _ := it["versions"].([]any); len(vs) != 2 {
			t.Errorf("история версий: %d, ожидалось 2", len(vs))
		}
	}

	// Вернуть — и он снова в выборе.
	if code, _ := h.Do(t, http.MethodPost, "/api/v1/admin/document_templates/"+id+"/restore",
		h.Token(t, admin), nil); code != http.StatusNoContent {
		t.Fatalf("вернуть из архива: код %d", code)
	}
	_, list = h.Do(t, http.MethodGet, "/api/v1/manager/document_templates", h.Token(t, manager), nil)
	found = false
	for _, it := range docItems(t, list) {
		if it["id"] == id {
			found = true
		}
	}
	if !found {
		t.Error("возвращённый шаблон не вернулся в выбор")
	}

	// Менеджер шаблоны не заводит.
	code, _ = h.Do(t, http.MethodPost, "/api/v1/admin/document_templates", h.Token(t, manager),
		map[string]any{"kind": "act", "title": "x", "audience": "client", "url": "https://x.example.com"})
	if code != http.StatusForbidden && code != http.StatusNotFound {
		t.Errorf("менеджер завёл шаблон: код %d", code)
	}
}

func TestDocumentDeliveredToOnePersonOnly(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	admin, cleanupAdmin := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true, IsManager: true})
	defer cleanupAdmin()
	_, tpl := h.Do(t, http.MethodPost, "/api/v1/admin/document_templates", h.Token(t, admin),
		map[string]any{
			"kind": "contract", "title": "Договор (выдача)", "audience": "creators",
			"url": "https://docs.example.com/deal-v1",
		})
	tplID := tpl["id"].(string)
	defer dropTemplates(t, tplID)

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	if len(creators) < 2 {
		t.Skip("нужны двое креаторов")
	}
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	stranger, cleanupStranger := h.NewUser(t, userOpts{Kind: "specialist"})
	defer cleanupStranger()

	base := "/api/v1/manager/projects/" + projectID.String() + "/documents"

	// Своя ссылка — одному креатору.
	code, out := h.Do(t, http.MethodPost, base, h.Token(t, manager), map[string]any{
		"audience": "creators", "recipient_ids": []string{creators[0].String()},
		"kind": "act", "title": "Акт за сентябрь", "url": "https://docs.example.com/act-sep",
		"note": "подпишите и пришлите в комментарии",
	})
	if code != http.StatusCreated || len(docItems(t, out)) != 1 {
		t.Fatalf("выдача одному: код %d, тело %v", code, out)
	}
	docID := docItems(t, out)[0]["id"].(string)

	_, mine := h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, creators[0]), nil)
	if !hasTitle(docItems(t, mine), "Акт за сентябрь") {
		t.Error("адресат не видит выданный ему документ")
	}
	_, other := h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, creators[1]), nil)
	if hasTitle(docItems(t, other), "Акт за сентябрь") {
		t.Error("сосед по проекту видит чужой документ")
	}

	// Событие боту — одно, и адресат в нём один.
	var recipients []string
	if err := pool.QueryRow(ctx, `
SELECT ARRAY(SELECT jsonb_array_elements_text(payload->'recipient_ids'))
FROM outbox WHERE aggregate_id = $1 AND event_type = 'project.document_delivered'
ORDER BY id DESC LIMIT 1`, projectID.String()).Scan(&recipients); err != nil {
		t.Fatalf("событие выдачи: %v", err)
	}
	if len(recipients) != 1 || recipients[0] != creators[0].String() {
		t.Errorf("адресаты в событии %v, ожидался только %s", recipients, creators[0])
	}

	// Постороннему не выдать: id сам по себе права не даёт.
	code, _ = h.Do(t, http.MethodPost, base, h.Token(t, manager), map[string]any{
		"audience": "creators", "recipient_ids": []string{stranger.String()},
		"kind": "other", "title": "x", "url": "https://docs.example.com/x",
	})
	if code != http.StatusBadRequest {
		t.Errorf("выдача постороннему: код %d, ожидался 400", code)
	}

	// По шаблону — всему составу; новая версия выданное не трогает.
	code, out = h.Do(t, http.MethodPost, base, h.Token(t, manager), map[string]any{
		"audience": "creators", "template_id": tplID,
	})
	if code != http.StatusCreated || len(docItems(t, out)) != 2 {
		t.Fatalf("выдача по шаблону всем: код %d, тело %v", code, out)
	}
	h.Do(t, http.MethodPost, "/api/v1/admin/document_templates/"+tplID+"/versions",
		h.Token(t, admin), map[string]any{"url": "https://docs.example.com/deal-v2"})
	_, mine = h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, creators[1]), nil)
	for _, it := range docItems(t, mine) {
		if it["title"] == "Договор (выдача)" && it["url"] != "https://docs.example.com/deal-v1" {
			t.Errorf("выданный документ поменялся после новой версии: %v", it["url"])
		}
	}

	// Шаблон для другой стороны — отказ.
	code, _ = h.Do(t, http.MethodPost, base, h.Token(t, manager), map[string]any{
		"audience": "client", "template_id": tplID,
	})
	if code != http.StatusBadRequest {
		t.Errorf("креаторский шаблон заказчику: код %d, ожидался 400", code)
	}

	// «Открыл» — один раз; чужому не отметить.
	code, o1 := h.Do(t, http.MethodPost, "/api/v1/me/documents/"+docID+"/open", h.Token(t, creators[0]), nil)
	if code != http.StatusOK {
		t.Fatalf("открыл: код %d", code)
	}
	_, o2 := h.Do(t, http.MethodPost, "/api/v1/me/documents/"+docID+"/open", h.Token(t, creators[0]), nil)
	if o1["opened_at"] != o2["opened_at"] {
		t.Errorf("отметка «открыл» сдвинулась: %v → %v", o1["opened_at"], o2["opened_at"])
	}
	if code, _ := h.Do(t, http.MethodPost, "/api/v1/me/documents/"+docID+"/open",
		h.Token(t, creators[1]), nil); code != http.StatusNotFound {
		t.Errorf("чужой документ открыт: код %d, ожидался 404", code)
	}

	// Отзыв: у адресата пропадает, у менеджера остаётся с отметкой.
	if code, _ := h.Do(t, http.MethodPost, base+"/"+docID+"/revoke", h.Token(t, manager), nil); code != http.StatusNoContent {
		t.Fatalf("отзыв: код %d", code)
	}
	_, mine = h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, creators[0]), nil)
	if hasTitle(docItems(t, mine), "Акт за сентябрь") {
		t.Error("отозванный документ остался у адресата")
	}
	_, hist := h.Do(t, http.MethodGet, base, h.Token(t, manager), nil)
	revoked := false
	for _, it := range docItems(t, hist) {
		if it["id"] == docID && it["revoked_at"] != nil {
			revoked = true
		}
	}
	if !revoked {
		t.Error("в истории выдачи нет отозванного документа")
	}
	if code, _ := h.Do(t, http.MethodPost, base+"/"+docID+"/unrevoke", h.Token(t, manager), nil); code != http.StatusNoContent {
		t.Fatalf("вернуть: код %d", code)
	}
	_, mine = h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, creators[0]), nil)
	if !hasTitle(docItems(t, mine), "Акт за сентябрь") {
		t.Error("возвращённый документ не вернулся к адресату")
	}
}

func TestDocumentToClient(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	projectID, _, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	var clientID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT client_user_id FROM projects WHERE id = $1`, projectID).
		Scan(&clientID); err != nil {
		t.Fatalf("read client: %v", err)
	}
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	code, out := h.Do(t, http.MethodPost, "/api/v1/manager/projects/"+projectID.String()+"/documents",
		h.Token(t, manager), map[string]any{
			"audience": "client", "kind": "act", "title": "Акт для заказчика",
			"url": "https://docs.example.com/client-act",
		})
	if code != http.StatusCreated {
		t.Fatalf("выдача заказчику: код %d, тело %v", code, out)
	}
	items := docItems(t, out)
	if len(items) != 1 || items[0]["recipient_user_id"] != clientID.String() {
		t.Fatalf("адресат не заказчик проекта: %v", items)
	}
	_, mine := h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, clientID), nil)
	if !hasTitle(docItems(t, mine), "Акт для заказчика") {
		t.Error("заказчик не видит выданный ему документ")
	}
	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM outbox
WHERE aggregate_id = $1 AND event_type = 'project.client_document_delivered'`,
		projectID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("событие заказчику: %d (%v), ожидалось 1", n, err)
	}
}

// Материал проекта удаляется мягко: из списков пропадает, в базе
// остаётся с отметкой, кто убрал.
func TestMaterialSoftDelete(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	base := "/api/v1/manager/projects/" + projectID.String() + "/materials"
	code, m := h.Do(t, http.MethodPost, base, h.Token(t, manager), map[string]any{
		"kind": "contract", "title": "Договор проекта", "url": "https://docs.example.com/p",
	})
	if code != http.StatusCreated {
		t.Fatalf("материал: код %d, тело %v", code, m)
	}
	mid := m["id"].(string)
	if code, _ := h.Do(t, http.MethodDelete, base+"/"+mid, h.Token(t, manager), nil); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("удаление: код %d", code)
	}
	_, list := h.Do(t, http.MethodGet, base, h.Token(t, manager), nil)
	if hasTitle(docItems(t, list), "Договор проекта") {
		t.Error("удалённый материал остался в списке")
	}
	_, mine := h.Do(t, http.MethodGet, "/api/v1/me/documents", h.Token(t, creators[0]), nil)
	if hasTitle(docItems(t, mine), "Договор проекта") {
		t.Error("удалённый договор остался в «Моих документах»")
	}
	var deletedBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT deleted_by FROM project_materials WHERE id = $1`, mid).
		Scan(&deletedBy); err != nil {
		t.Fatalf("строка материала стёрта, а не помечена: %v", err)
	}
	if deletedBy == nil || *deletedBy != manager {
		t.Errorf("deleted_by = %v, ожидался менеджер", deletedBy)
	}
}

// «Мои документы» по одному проекту и после ухода из проекта.
//
// project_id сужает список на сервере: карточке проекта незачем тянуть
// документы всех проектов человека. Ушедшему из состава личные
// документы проекта не показываются и открытыми не отмечаются — как и
// договоры из материалов.
func TestMyDocumentsByProjectAndAfterLeaving(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()
	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()

	code, out := h.Do(t, http.MethodPost, "/api/v1/manager/projects/"+projectID.String()+"/documents",
		h.Token(t, manager), map[string]any{
			"audience": "creators", "recipient_ids": []string{creators[0].String()},
			"kind": "nda", "title": "NDA проекта", "url": "https://docs.example.com/nda",
		})
	if code != http.StatusCreated {
		t.Fatalf("выдача: код %d, тело %v", code, out)
	}
	docID := docItems(t, out)[0]["id"].(string)
	tok := h.Token(t, creators[0])

	_, mine := h.Do(t, http.MethodGet, "/api/v1/me/documents?project_id="+projectID.String(), tok, nil)
	if !hasTitle(docItems(t, mine), "NDA проекта") {
		t.Error("с project_id своего проекта документа нет")
	}
	_, other := h.Do(t, http.MethodGet, "/api/v1/me/documents?project_id="+uuid.NewString(), tok, nil)
	if len(docItems(t, other)) != 0 {
		t.Errorf("по чужому project_id отдали документы: %v", docItems(t, other))
	}
	if code, _ := h.Do(t, http.MethodGet, "/api/v1/me/documents?project_id=abc", tok, nil); code != http.StatusBadRequest {
		t.Errorf("project_id не uuid: код %d, ожидался 400", code)
	}

	// Договор проекта в материалах — старше выданного NDA, но в карточке
	// проекта стоит первым: он один на проект, и ищут его чаще.
	if _, err := pool.Exec(ctx, `
INSERT INTO project_materials (project_id, kind, title, url, audience, created_at)
VALUES ($1, 'contract', 'Договор проекта', 'https://docs.example.com/c', 'creators',
        now() - interval '1 day')`, projectID); err != nil {
		t.Fatalf("договор в материалы: %v", err)
	}
	_, mine = h.Do(t, http.MethodGet, "/api/v1/me/documents?project_id="+projectID.String(), tok, nil)
	if items := docItems(t, mine); len(items) < 2 || items[0]["title"] != "Договор проекта" {
		t.Errorf("в карточке проекта договор не первым: %v", items)
	}
	// source=personal — без договоров из материалов: у заказчика они
	// и так стоят рядом, в материалах проекта.
	_, mine = h.Do(t, http.MethodGet,
		"/api/v1/me/documents?source=personal&project_id="+projectID.String(), tok, nil)
	if items := docItems(t, mine); hasTitle(items, "Договор проекта") || !hasTitle(items, "NDA проекта") {
		t.Errorf("source=personal: %v", items)
	}
	if code, _ := h.Do(t, http.MethodGet, "/api/v1/me/documents?source=all", tok, nil); code != http.StatusBadRequest {
		t.Errorf("неизвестный source: код %d, ожидался 400", code)
	}

	if _, err := pool.Exec(ctx, `
UPDATE project_creators SET removed_at = now()
WHERE project_id = $1 AND creator_user_id = $2`, projectID, creators[0]); err != nil {
		t.Fatalf("убрать из состава: %v", err)
	}
	_, mine = h.Do(t, http.MethodGet, "/api/v1/me/documents", tok, nil)
	if hasTitle(docItems(t, mine), "NDA проекта") {
		t.Error("ушедший из проекта по-прежнему видит его документ")
	}
	if code, _ := h.Do(t, http.MethodPost, "/api/v1/me/documents/"+docID+"/open", tok, nil); code != http.StatusNotFound {
		t.Errorf("ушедший отметил документ открытым: код %d, ожидался 404", code)
	}
}
