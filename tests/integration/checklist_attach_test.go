package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Чек-лист новому проекту подключается САМ.
//
// До этого его ставили руками, а вспоминали о нём в момент первой сдачи
// — то есть когда креатор уже снял ролик по своим представлениям о
// требованиях. Менеджер заводит проект и уходит собирать команду;
// чек-лист в этот момент не на первом месте ни у кого.
//
// Правило выбора шаблона здесь же и главное в нём — отказ выбирать:
// действующих шаблонов в библиотеке может быть несколько («UGC» и
// «Продакшн»), и подставленный наугад менеджер не заметит, а креатор
// получит требования чужого проекта.

// startViaService — создание проекта ТЕМ ЖЕ путём, что в бою: через
// сервис с подключённой библиотекой. Мимо сервиса (repo.StartProject)
// автоподключения нет вовсе, и тест на нём был бы зелёным всегда.
func startViaService(t *testing.T, pool *pgxpool.Pool, in projects.StartProjectInput) uuid.UUID {
	t.Helper()
	svc := projects.NewService(projects.NewRepo(pool)).
		WithChecklistAttacher(publications.NewService(publications.NewRepo(pool)))
	if in.Kind == "" {
		in.Kind = projects.KindCreatorsTurnkey
	}
	in.Source = projects.SourceManual
	id, err := svc.StartProject(context.Background(), in)
	if err != nil {
		t.Fatalf("создать проект %q: %v", in.Title, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate = 'project' AND aggregate_id = $1`, id.String())
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// hideTemplates — погасить всё, что уже лежит в библиотеке.
//
// Тест обязан сам задать, сколько действующих шаблонов в мире: правило
// зависит ровно от этого числа, а в общей базе его определяет тот, кто
// прогонял тесты до нас.
func hideTemplates(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var restored []uuid.UUID
	rows, err := pool.Query(ctx, `SELECT id FROM checklist_templates WHERE is_active`)
	if err != nil {
		t.Fatalf("прочитать библиотеку: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		restored = append(restored, id)
	}
	rows.Close()
	if _, err := pool.Exec(ctx, `UPDATE checklist_templates SET is_active = FALSE WHERE is_active`); err != nil {
		t.Fatalf("погасить шаблоны: %v", err)
	}
	t.Cleanup(func() {
		for _, id := range restored {
			_, _ = pool.Exec(context.Background(),
				`UPDATE checklist_templates SET is_active = TRUE WHERE id = $1`, id)
		}
	})
}

// template — действующий шаблон с одним пунктом.
func template(t *testing.T, pool *pgxpool.Pool, name, item string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO checklist_templates (name, version, is_active) VALUES ($1, 1, TRUE) RETURNING id`,
		name+" "+uuid.NewString()[:8]).Scan(&id); err != nil {
		t.Fatalf("завести шаблон: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO checklist_template_items (template_id, text, is_required, sort_order)
		 VALUES ($1, $2, TRUE, 1)`, id, item); err != nil {
		t.Fatalf("пункт шаблона: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM checklist_templates WHERE id = $1`, id)
	})
	return id
}

func checklistTexts(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT text FROM project_checklist_items WHERE project_id = $1 ORDER BY sort_order`, projectID)
	if err != nil {
		t.Fatalf("прочитать чек-лист проекта: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func TestNewCreatorsProjectGetsActiveChecklist(t *testing.T) {
	pool := integration.Pool(t)
	hideTemplates(t, pool)
	template(t, pool, "Действующий", "Логотип в первые 3 секунды")

	h := integration.NewAPIHarness(t, pool)
	client, cleanup := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanup)

	pid := startViaService(t, pool, projects.StartProjectInput{
		ClientUserID: &client, Title: "Проект с автоподключением",
	})

	got := checklistTexts(t, pool, pid)
	if len(got) != 1 || got[0] != "Логотип в первые 3 секунды" {
		t.Fatalf("чек-лист проекта: %v, ожидали один пункт из действующего шаблона", got)
	}
}

// Два действующих шаблона — и подключать перестаём вовсе.
//
// Пустой чек-лист менеджер увидит и подключит нужный сам. Подставленный
// наугад он не увидит: экран выглядит заполненным, а креатор получает
// требования от чужого проекта.
func TestSecondActiveTemplateStopsAutoAttach(t *testing.T) {
	pool := integration.Pool(t)
	hideTemplates(t, pool)
	template(t, pool, "UGC", "Логотип в первые 3 секунды")
	template(t, pool, "Продакшн", "Съёмка на две камеры")

	h := integration.NewAPIHarness(t, pool)
	client, cleanup := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanup)

	pid := startViaService(t, pool, projects.StartProjectInput{
		ClientUserID: &client, Title: "Проект при двух шаблонах",
	})

	if got := checklistTexts(t, pool, pid); len(got) != 0 {
		t.Fatalf("выбрали шаблон за человека: %v", got)
	}
}

// Чек-лист — про выкладки. У продакшна их нет, и требования к ним
// подключать не к чему.
func TestProductionProjectGetsNoChecklist(t *testing.T) {
	pool := integration.Pool(t)
	hideTemplates(t, pool)
	template(t, pool, "Действующий", "Логотип в первые 3 секунды")

	h := integration.NewAPIHarness(t, pool)
	client, cleanup := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanup)

	// Продакшну нужна воронка — заводим свою: тест про чек-лист, а не
	// про обход проверок, и брать чужую из общей базы нельзя.
	ctx := context.Background()
	var pipeline, stage uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO pipelines (name) VALUES ($1) RETURNING id`,
		"Чек-лист: воронка "+uuid.NewString()[:8]).Scan(&pipeline); err != nil {
		t.Fatalf("завести воронку: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO pipeline_stages (pipeline_id, name, sort_order) VALUES ($1, 'Съёмка', 1) RETURNING id`,
		pipeline).Scan(&stage); err != nil {
		t.Fatalf("стадия воронки: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO pipeline_steps (stage_id, name, owner, sort_order) VALUES ($1, 'Смена', 'team', 1)`,
		stage); err != nil {
		t.Fatalf("шаг воронки: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pipelines WHERE id = $1`, pipeline)
	})

	pid := startViaService(t, pool, projects.StartProjectInput{
		ClientUserID: &client, Title: "Продакшн без выкладок",
		Kind: projects.KindProductionTurnkey, PipelineID: pipeline,
	})

	if got := checklistTexts(t, pool, pid); len(got) != 0 {
		t.Fatalf("проекту без выкладок подключили чек-лист: %v", got)
	}
}

// Менеджер дополняет чек-лист ПОД ПРОЕКТ.
//
// «Шрифт титров — Onest Bold» касается одного бренда, и класть это в
// общую библиотеку неверно. Снимок для того и снимок, что принадлежит
// проекту: правило снимка — про то, что правка библиотеки не доезжает до
// идущих проектов, а не про запрет уточнять.
func TestManagerAddsItemToProjectOnly(t *testing.T) {
	pool := integration.Pool(t)
	hideTemplates(t, pool)
	tplID := template(t, pool, "Действующий", "Логотип в первые 3 секунды")

	h := integration.NewAPIHarness(t, pool)
	client, cleanupC := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanupC)
	mgr, cleanupM := h.NewUser(t, integration.UserOpts{Kind: "client", IsManager: true})
	t.Cleanup(cleanupM)
	token := h.Token(t, mgr)

	pid := startViaService(t, pool, projects.StartProjectInput{
		ClientUserID: &client, Title: "Проект с уточнением", AssignedToUserID: &mgr,
	})

	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+pid.String()+"/checklist/items", token,
		map[string]any{"text": "Шрифт титров — Onest Bold", "is_required": false})
	if code != http.StatusCreated {
		t.Fatalf("добавить пункт: код %d, тело %v", code, body)
	}

	got := checklistTexts(t, pool, pid)
	if len(got) != 2 || got[1] != "Шрифт титров — Onest Bold" {
		t.Fatalf("чек-лист проекта: %v, ожидали шаблонный пункт и добавленный", got)
	}

	// И в библиотеку он не уехал: следующий проект его не получит.
	var inLibrary int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM checklist_template_items WHERE template_id = $1`, tplID).
		Scan(&inLibrary); err != nil {
		t.Fatalf("посчитать пункты шаблона: %v", err)
	}
	if inLibrary != 1 {
		t.Fatalf("пунктов в библиотеке %d — уточнение проекта уехало в общий шаблон", inLibrary)
	}
}

// Пункт, по которому уже отчитывались, не удаляется.
//
// Отметки висят на нём внешним ключом с каскадом: вместе с пунктом
// исчез бы след того, что креатор это проверял. Спорить потом нечем —
// ни ему, ни менеджеру.
func TestCheckedChecklistItemSurvivesDeletion(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	hideTemplates(t, pool)
	template(t, pool, "Действующий", "Логотип в первые 3 секунды")

	h := integration.NewAPIHarness(t, pool)
	client, cleanupC := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanupC)
	creator, cleanupCr := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanupCr)
	mgr, cleanupM := h.NewUser(t, integration.UserOpts{Kind: "client", IsManager: true})
	t.Cleanup(cleanupM)
	token := h.Token(t, mgr)

	pid := startViaService(t, pool, projects.StartProjectInput{
		ClientUserID: &client, Title: "Проект с отметками", AssignedToUserID: &mgr,
	})

	var itemID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project_checklist_items WHERE project_id = $1`, pid).Scan(&itemID); err != nil {
		t.Fatalf("пункт проекта: %v", err)
	}

	var pubID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO project_publications (project_id, creator_user_id, due_date, status)
VALUES ($1, $2, CURRENT_DATE, 'planned') RETURNING id`, pid, creator).Scan(&pubID); err != nil {
		t.Fatalf("выкладка: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO publication_checklist_marks (publication_id, item_id) VALUES ($1, $2)`,
		pubID, itemID); err != nil {
		t.Fatalf("отметка: %v", err)
	}

	code, body := h.Do(t, http.MethodDelete,
		"/api/v1/manager/projects/"+pid.String()+"/checklist/items/"+itemID.String(), token, nil)
	if code != http.StatusConflict {
		t.Fatalf("удаление отмеченного пункта: код %d, тело %v — ожидали 409", code, body)
	}

	if got := checklistTexts(t, pool, pid); len(got) != 1 {
		t.Fatalf("пункт всё-таки удалили: %v", got)
	}
}

// Обновление шаблона не стирает пункты, заведённые под проект.
//
// Повторное подключение — это и есть «обновить до новой версии»:
// отдельной ручки нет. Снимок сносится целиком и кладётся заново, и
// вместе с ним раньше исчезли бы уточнения менеджера — молча, в момент,
// когда он всего лишь поднимает версию. Заметить это он смог бы только
// через неделю, когда креатор сдал ролик не по тем требованиям.
func TestOwnChecklistItemSurvivesTemplateUpgrade(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	hideTemplates(t, pool)
	template(t, pool, "Действующий", "Логотип в первые 3 секунды")

	h := integration.NewAPIHarness(t, pool)
	client, cleanupC := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanupC)
	mgr, cleanupM := h.NewUser(t, integration.UserOpts{Kind: "client", IsManager: true})
	t.Cleanup(cleanupM)
	token := h.Token(t, mgr)

	pid := startViaService(t, pool, projects.StartProjectInput{
		ClientUserID: &client, Title: "Проект с обновлением шаблона", AssignedToUserID: &mgr,
	})

	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+pid.String()+"/checklist/items", token,
		map[string]any{"text": "Шрифт титров — Onest Bold", "is_required": false})
	if code != http.StatusCreated {
		t.Fatalf("добавить пункт: код %d, тело %v", code, body)
	}

	// Вышла новая версия шаблона, менеджер её подключает.
	var v2 uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO checklist_templates (name, version, is_active) VALUES ($1, 2, FALSE) RETURNING id`,
		"Новая версия "+uuid.NewString()[:8]).Scan(&v2); err != nil {
		t.Fatalf("вторая версия: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM checklist_templates WHERE id = $1`, v2)
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO checklist_template_items (template_id, text, is_required, sort_order)
VALUES ($1, 'Логотип в первые 2 секунды', TRUE, 1)`, v2); err != nil {
		t.Fatalf("пункт второй версии: %v", err)
	}

	code, body = h.Do(t, http.MethodPost,
		"/api/v1/manager/projects/"+pid.String()+"/checklist", token,
		map[string]any{"template_id": v2.String()})
	if code != http.StatusOK {
		t.Fatalf("обновить шаблон: код %d, тело %v", code, body)
	}

	got := checklistTexts(t, pool, pid)
	var kept, upgraded bool
	for _, text := range got {
		if text == "Шрифт титров — Onest Bold" {
			kept = true
		}
		if text == "Логотип в первые 2 секунды" {
			upgraded = true
		}
	}
	if !kept {
		t.Errorf("обновление шаблона стёрло пункт менеджера: %v", got)
	}
	if !upgraded {
		t.Errorf("новая версия шаблона не подключилась: %v", got)
	}
	// И старый библиотечный пункт ушёл: снимок заменён, а не дополнен.
	for _, text := range got {
		if text == "Логотип в первые 3 секунды" {
			t.Errorf("пункт старой версии остался: %v", got)
		}
	}
}
