package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/orders"
	"marketpclce/internal/profiles"
	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Воронка «под ключ»: рассылка, отклики, финализация.
//
// Проверяем то, что ломается тихо и целиком:
//   • пинг менеджерам уходит вместе с заявкой — без него человек снова
//     уходит в тишину, а заявка лежит в базе никем не замеченной;
//   • рассылка идёт ВСЕМ известным креаторам и ровно один раз: второе
//     сообщение по той же заявке читается как беспорядок;
//   • откликнуться может тот, кого заказчик не отмечал, — и его отклик
//     обязан долететь до менеджера;
//   • финализация добавляет людей в состав тем же путём, каким их
//     добавляет менеджер руками: вместе с составом уходит задание.

// markProfile — завести профиль специалиста, без которого человек не
// «известный креатор»: рассылка идёт по профилям, а не по строкам
// users.
func markProfile(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO specialist_profiles (user_id, display_name, moderation_status)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET moderation_status = EXCLUDED.moderation_status`,
		userID, "Креатор "+userID.String()[:8], status); err != nil {
		t.Fatalf("profile: %v", err)
	}
}

func lastPayload(t *testing.T, pool *pgxpool.Pool, aggregateID, eventType string) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `
SELECT payload FROM outbox
WHERE aggregate_id = $1 AND event_type = $2
ORDER BY id DESC LIMIT 1`, aggregateID, eventType).Scan(&raw); err != nil {
		t.Fatalf("событие %s не найдено: %v", eventType, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload %s: %v", eventType, err)
	}
	return out
}

// Заявка отправлена — в общий чат менеджеров уходит сообщение.
//
// Между «нажал отправить» и «менеджер позвонил» нет ничего, что
// заметило бы заявку само: в CRM она видна проектом, то есть тому, кто
// и так туда зашёл. Проверяем и содержимое: разговор начинается с той
// суммы, которую человек видел на баре, а не с пересчитанной.
func TestOrderSubmittedPingsManagers(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	projectsSvc := projects.NewService(projects.NewRepo(pool))
	svc := orders.NewService(orders.NewRepo(pool)).WithProjects(projectsSvc)

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 2,
		VideosCount: 40, CreatorIDs: creators[:2],
		Brief:   orders.OrderBrief{Product: "кофе на подписку", Goal: "продажи"},
		Ceiling: 12_345_600,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, res.Order.ID.String())
		if res.Order.ProjectID != nil {
			_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, *res.Order.ProjectID)
		}
	}()

	p := lastPayload(t, pool, res.Order.ID.String(), "order.submitted")
	if p["videos_count"] != float64(40) {
		t.Errorf("в сообщении %v роликов, а в заявке 40", p["videos_count"])
	}
	if p["preferred"] != float64(2) {
		t.Errorf("отмеченных в сообщении %v, а отмечали двоих", p["preferred"])
	}
	if p["ceiling"] != float64(12_345_600) {
		t.Errorf("потолок в сообщении %v — менеджер начнёт разговор с чужой суммы", p["ceiling"])
	}
	if p["title"] != "кофе на подписку" {
		t.Errorf("заголовок %v, ожидался из брифа", p["title"])
	}
	if s, _ := p["brief"].(string); !strings.Contains(s, "кофе на подписку") {
		t.Errorf("бриф не доехал до сообщения: %q", s)
	}
	if res.Order.ProjectID == nil || p["project_id"] != res.Order.ProjectID.String() {
		t.Errorf("в сообщении нет ссылки на проект: %v", p["project_id"])
	}
	// Клиента и контакт тоже: без них менеджеру некому звонить.
	if p["client_contact"] == nil || p["client_name"] == nil {
		t.Errorf("в сообщении нет заказчика: %v", p)
	}
}

// Рассылка уходит всем известным креаторам — и ровно один раз.
func TestBroadcastReachesKnownCreatorsOnce(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	// Двое из четверых — «известные»: профиль есть, модерация пройдена
	// или идёт. Третий без профиля, четвёртый отклонён — рассылка их
	// обходит.
	markProfile(t, pool, creators[0], "approved")
	markProfile(t, pool, creators[1], "pending_review")
	markProfile(t, pool, creators[3], "rejected")

	repo := orders.NewRepo(pool)
	svc := orders.NewService(repo)
	now := time.Now().UTC()

	// Заказчик отметил только первого: второй получит ту же рассылку
	// без приписки «хотят особенно».
	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:1],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orderID := res.Order.ID
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, orderID.String())
		_, _ = pool.Exec(ctx, `DELETE FROM notification_log WHERE subject_id = $1`, orderID)
	}()

	if res.Broadcast.Recipients != 2 {
		t.Fatalf("рассылка ушла %d получателям, ожидали двоих известных",
			res.Broadcast.Recipients)
	}
	if res.Broadcast.Preferred != 1 {
		t.Errorf("отмеченных среди получателей %d, ожидали одного", res.Broadcast.Preferred)
	}

	// У второго появилась строка кандидата с отметкой рассылки: без неё
	// «не писали» и «написали, молчит» неразличимы.
	var broadcastAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT broadcast_at FROM order_candidates WHERE order_id = $1 AND creator_user_id = $2`,
		orderID, creators[1]).Scan(&broadcastAt); err != nil {
		t.Fatalf("кандидат рассылки: %v", err)
	}
	if broadcastAt == nil {
		t.Error("рассылка не отметилась на кандидате")
	}

	// Кого рассылка обошла — того в заявке и нет.
	for _, skipped := range []uuid.UUID{creators[2], creators[3]} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM order_candidates WHERE order_id = $1 AND creator_user_id = $2`,
			orderID, skipped).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("рассылка задела того, кто не проходит отбор: %s", skipped)
		}
	}

	// Повторный прогон никому не пишет второй раз.
	again, err := repo.Broadcast(ctx, orderID, now)
	if err != nil {
		t.Fatalf("повторная рассылка: %v", err)
	}
	if again.Recipients != 0 {
		t.Errorf("повторная рассылка написала %d людям — они получат дубль",
			again.Recipients)
	}

	// И событие про неё есть, даже когда получателей ноль: «рассылка
	// была и никого не нашла» отличается от «рассылки не было».
	p := lastPayload(t, pool, orderID.String(), "order.broadcast_sent")
	if p["recipients"] != float64(0) {
		t.Errorf("последнее событие рассылки говорит о %v получателях", p["recipients"])
	}

	// Приглашение видно креатору в кабинете — он же не знает, что оно
	// ушло рассылкой.
	list, err := svc.InvitationsFor(ctx, creators[1])
	if err != nil {
		t.Fatalf("InvitationsFor: %v", err)
	}
	var seen bool
	for _, iv := range list {
		if iv.OrderID == orderID {
			seen = true
			if iv.IsPreferred {
				t.Error("неотмеченному показали «хотят особенно»")
			}
		}
	}
	if !seen {
		t.Error("рассылка не видна креатору в «моих приглашениях»")
	}
}

// Отклик креатора: тремя способами и от того, кого не отмечали.
func TestCreatorRespondsToBroadcast(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	markProfile(t, pool, creators[0], "approved")
	markProfile(t, pool, creators[1], "approved")

	repo := orders.NewRepo(pool)
	svc := orders.NewService(repo)
	now := time.Now().UTC()

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:1],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orderID := res.Order.ID
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, orderID.String())
		_, _ = pool.Exec(ctx, `DELETE FROM notification_log WHERE subject_id = $1`, orderID)
	}()

	// Свой ролик в портфолио — для ответа «отправить из моих».
	var itemID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO portfolio_items (user_id, kind, title, video_url)
VALUES ($1, 'video', 'мой ролик', 'https://cdn.example/portfolio/a.mp4') RETURNING id`,
		creators[1]).Scan(&itemID); err != nil {
		t.Fatalf("portfolio item: %v", err)
	}
	var foreignItem uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO portfolio_items (user_id, kind, title, video_url)
VALUES ($1, 'video', 'чужой ролик', 'https://cdn.example/portfolio/b.mp4') RETURNING id`,
		creators[0]).Scan(&foreignItem); err != nil {
		t.Fatalf("foreign item: %v", err)
	}

	// Отвечает тот, кого заказчик НЕ отмечал: рассылка ушла всем.
	if _, err := svc.SaveResponse(ctx, orders.ResponseInput{
		OrderID: orderID, CreatorUserID: creators[1],
		Mode: orders.ModeFromPortfolio, PortfolioItems: []uuid.UUID{itemID},
		Note: "снимал похожее в июле",
	}, now); err != nil {
		t.Fatalf("SaveResponse(from_portfolio): %v", err)
	}

	// Чужой ролик в отклик не попадает.
	if _, err := svc.SaveResponse(ctx, orders.ResponseInput{
		OrderID: orderID, CreatorUserID: creators[1],
		Mode: orders.ModeFromPortfolio, PortfolioItems: []uuid.UUID{foreignItem},
	}, now); !errors.Is(err, orders.ErrNotYourPortfolio) {
		t.Errorf("чужой ролик приняли в отклик: %v", err)
	}

	// Отклик без работы — не отклик.
	if _, err := svc.SaveResponse(ctx, orders.ResponseInput{
		OrderID: orderID, CreatorUserID: creators[1], Mode: orders.ModeAttach,
	}, now); !errors.Is(err, orders.ErrNothingAttached) {
		t.Errorf("пустой отклик приняли: %v", err)
	}

	// Отмеченный прикладывает файл.
	const sample = "https://cdn.example/orders/sample.mp4"
	if _, err := svc.SaveResponse(ctx, orders.ResponseInput{
		OrderID: orderID, CreatorUserID: creators[0],
		Mode: orders.ModeAttach, FileURL: sample,
	}, now); err != nil {
		t.Fatalf("SaveResponse(attach): %v", err)
	}

	list, err := svc.Responses(ctx, orderID)
	if err != nil {
		t.Fatalf("Responses: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("откликов %d, ожидали два", len(list))
	}
	// Отмеченные сверху: из двадцати согласных менеджер начинает с тех,
	// кого просили.
	if !list[0].IsPreferred || list[0].CreatorUserID != creators[0] {
		t.Errorf("первым в списке не отмеченный заказчиком: %+v", list[0])
	}
	if list[0].FileURL != sample {
		t.Errorf("файл отклика %q, ожидали %q", list[0].FileURL, sample)
	}
	if list[0].CreatorName == "" {
		t.Error("отклик без имени — менеджер видит uuid")
	}
	if len(list[1].Items) != 1 || list[1].Items[0].ID != itemID {
		t.Errorf("ролики отклика не доехали: %+v", list[1].Items)
	}
	if list[1].Note != "снимал похожее в июле" {
		t.Errorf("комментарий отклика %q", list[1].Note)
	}

	// Статус кандидата — «откликнулся», а НЕ «согласился»: согласие
	// подтверждает менеджер, добавляя человека в состав.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM order_candidates WHERE order_id = $1 AND creator_user_id = $2`,
		orderID, creators[1]).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != string(orders.CandidateResponded) {
		t.Errorf("статус кандидата %q, ожидали responded", status)
	}

	// Отказ — тоже ответ, и он виден: «сказал нет» и «молчит» для
	// менеджера разные вещи.
	if _, err := svc.SaveResponse(ctx, orders.ResponseInput{
		OrderID: orderID, CreatorUserID: creators[1], Mode: orders.ModeDecline,
	}, now); err != nil {
		t.Fatalf("SaveResponse(decline): %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status FROM order_candidates WHERE order_id = $1 AND creator_user_id = $2`,
		orderID, creators[1]).Scan(&status); err != nil {
		t.Fatalf("status after decline: %v", err)
	}
	if status != string(orders.CandidateDeclined) {
		t.Errorf("после отказа статус %q", status)
	}
	// Отклик переписался целиком: прежние ролики не остались висеть.
	one, err := repo.ResponseOf(ctx, orderID, creators[1])
	if err != nil {
		t.Fatalf("ResponseOf: %v", err)
	}
	if one.Mode != orders.ModeDecline || len(one.Items) != 0 {
		t.Errorf("отказ не переписал прежний отклик: %+v", one)
	}
}

// Финализация: менеджер утверждает состав и объём.
func TestFinalizeAddsCrewAndPlan(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	markProfile(t, pool, creators[0], "approved")

	projectsSvc := projects.NewService(projects.NewRepo(pool))
	publicationsSvc := publications.NewService(publications.NewRepo(pool))
	svc := orders.NewService(orders.NewRepo(pool)).
		WithProjects(projectsSvc).
		WithCrew(publicationsSvc)

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:1],
		Brief: orders.OrderBrief{Product: "чай"},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orderID := res.Order.ID
	projectID := *res.Order.ProjectID
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, orderID.String())
		_, _ = pool.Exec(ctx, `DELETE FROM notification_log WHERE subject_id = $1`, orderID)
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID)
	}()

	// Менеджер берёт откликнувшегося: id менеджера тут — сам заказчик
	// не подойдёт, но для added_by достаточно любого пользователя.
	out, err := svc.Finalize(ctx, orderID, creators[:1], 25, clientID, time.Now().UTC())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if len(out.Added) != 1 {
		t.Fatalf("добавили %d людей, ожидали одного", len(out.Added))
	}
	if out.Order.Status != orders.StatusFinalized {
		t.Errorf("статус заявки %q, ожидали finalized", out.Order.Status)
	}

	// Человек в составе проекта — тем же путём, каким его добавляет
	// менеджер руками: вместе с составом уходит задание.
	var inCrew int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM project_creators
WHERE project_id = $1 AND creator_user_id = $2 AND removed_at IS NULL`,
		projectID, creators[0]).Scan(&inCrew); err != nil {
		t.Fatalf("crew: %v", err)
	}
	if inCrew != 1 {
		t.Error("финализация не добавила человека в состав проекта")
	}

	// План месяца — в проекте: заказ хранит, что просили, проект — о
	// чём договорились.
	var plan *int
	if err := pool.QueryRow(ctx,
		`SELECT monthly_plan FROM projects WHERE id = $1`, projectID).Scan(&plan); err != nil {
		t.Fatalf("monthly_plan: %v", err)
	}
	if plan == nil || *plan != 25 {
		t.Errorf("план месяца %v, ожидали 25", plan)
	}

	p := lastPayload(t, pool, orderID.String(), "order.finalized")
	if p["creators"] != float64(1) || p["monthly_plan"] != float64(25) {
		t.Errorf("событие финализации: %v", p)
	}

	// Кандидат отмечен принятым — иначе список у менеджера покажет его
	// как «ещё думает» рядом с составом, где он уже стоит.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM order_candidates WHERE order_id = $1 AND creator_user_id = $2`,
		orderID, creators[0]).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != string(orders.CandidateAccepted) {
		t.Errorf("статус кандидата после финализации %q", status)
	}
}

// Проба работы к заявке не пропадает из бакета.
//
// Файл лежит под префиксом orders/, и подметальщик по этому префиксу
// ходит (SweepOrphanMedia). Единственное, что не даёт ему снести живую
// пробу, — ссылка в LoadReferencedMediaURLs. Забыли одно из двух: либо
// проба исчезает через сутки, либо мусор копится вечно.
func TestWorkSampleIsReferencedMedia(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	markProfile(t, pool, creators[0], "approved")
	svc := orders.NewService(orders.NewRepo(pool))
	now := time.Now().UTC()

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID, StartMonth: nextMonth(), Needed: 1,
		VideosCount: 30, CreatorIDs: creators[:1],
	}, now)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, res.Order.ID.String())
		_, _ = pool.Exec(ctx, `DELETE FROM notification_log WHERE subject_id = $1`, res.Order.ID)
	}()

	// Адрес НАШ — из бакета и под префиксом orders/: внешнюю ссылку
	// KeyFromURL отсекает раньше сравнения, и тест был бы зелёным при
	// пустом запросе.
	const url = "https://storage.yandexcloud.net/bucket/orders/" +
		"7f0d8d0e-0a7a-4f26-9a4a-f2f2f2f2f2f2/sample.mp4"
	if _, err := svc.SaveResponse(ctx, orders.ResponseInput{
		OrderID: res.Order.ID, CreatorUserID: creators[0],
		Mode: orders.ModeAttach, FileURL: url,
	}, now); err != nil {
		t.Fatalf("SaveResponse: %v", err)
	}

	urls, err := profiles.NewRepo(pool).LoadReferencedMediaURLs(ctx)
	if err != nil {
		t.Fatalf("load referenced: %v", err)
	}
	for _, u := range urls {
		if u == url {
			return
		}
	}
	t.Fatalf("проба работы не попала в список ссылок — подметальщик снесёт её "+
		"через S3_ORPHAN_MIN_AGE (всего ссылок: %d)", len(urls))
}

// Договор — первым в материалах, каким бы ни был его порядковый номер.
//
// Это первое, что человек ищет, когда его добавили в проект. Раньше
// «первым» он был только по случайности: по sort_order, то есть по
// порядку добавления, и любой новый референс мог встать выше.
func TestContractMaterialComesFirst(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if _, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialLink, Title: "Референс",
		URL: "https://example.com/ref", CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("add reference: %v", err)
	}
	// Договор добавлен ВТОРЫМ — то есть по sort_order он ниже.
	if _, err := svc.AddMaterial(ctx, publications.AddMaterialInput{
		ProjectID: pid, Kind: publications.MaterialContract, Title: "Договор",
		URL: "https://example.com/contract.pdf", CreatedBy: creators[0],
	}); err != nil {
		t.Fatalf("add contract: %v", err)
	}

	list, err := svc.ListMaterials(ctx, pid, "")
	if err != nil {
		t.Fatalf("Materials: %v", err)
	}
	if len(list) < 2 {
		t.Fatalf("материалов %d, ожидали два", len(list))
	}
	if list[0].Kind != publications.MaterialContract {
		t.Errorf("первым идёт %q — договор придётся искать глазами", list[0].Kind)
	}
}

// Обязательность пункта чек-листа переключается, и отметки проверки
// при этом остаются.
//
// Без этой ручки менеджер удалял пункт и заводил заново — вместе с
// пунктом исчезал след того, что креатор его проверял.
func TestChecklistItemRequiredToggle(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	pid, _, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	it, err := svc.AddChecklistItem(ctx, pid, "Обложка вертикальная", "", false)
	if err != nil {
		t.Fatalf("AddChecklistItem: %v", err)
	}
	if it.IsRequired {
		t.Fatal("пункт завёлся обязательным, хотя просили необязательный")
	}

	got, err := svc.SetChecklistItemRequired(ctx, pid, it.ID, true)
	if err != nil {
		t.Fatalf("SetChecklistItemRequired: %v", err)
	}
	if !got.IsRequired {
		t.Error("пункт не стал обязательным")
	}
	var required bool
	if err := pool.QueryRow(ctx,
		`SELECT is_required FROM project_checklist_items WHERE id = $1`, it.ID).
		Scan(&required); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if !required {
		t.Error("обязательность не доехала до базы")
	}

	// Чужой пункт не трогаем: id пункта приходит из адреса, и без
	// сверки с проектом менеджер правил бы соседний проект.
	if _, err := svc.SetChecklistItemRequired(ctx, uuid.New(), it.ID, false); err == nil {
		t.Error("пункт правится из чужого проекта")
	}
}

// Вторая ветка воронки: «видео под ключ», проект без креаторов.
//
// Заводится она ТАК ЖЕ, как первая, и это главное требование: заявка
// создаёт проект в ту же секунду, менеджеру уходит пинг, заказчику есть
// куда прийти. Отличий ровно три, и все три — отсутствие: нет отбора
// креаторов, нет рассылки, нет откликов. Ролики снимаем мы и выкладываем
// с аккаунтов бренда, звать некого.
func TestBrandBranchStartsProjectWithoutCrew(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	clientID, creators, cleanup := setupOrderWorld(t, pool)
	defer cleanup()

	// Известный креатор в базе есть — и он НЕ должен получить ничего:
	// во второй ветке рассылки не бывает.
	markProfile(t, pool, creators[0], "approved")

	projectsSvc := projects.NewService(projects.NewRepo(pool))
	svc := orders.NewService(orders.NewRepo(pool)).WithProjects(projectsSvc)

	res, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID,
		ProjectKind:  orders.KindBrand,
		StartMonth:   nextMonth(),
		VideosCount:  20,
		Brief:        orders.OrderBrief{Product: "ролики для бренда"},
		Ceiling:      2_000_000,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orderID := res.Order.ID
	if res.Order.ProjectID == nil {
		t.Fatal("вторая ветка не завела проект — заказчику снова некуда прийти")
	}
	projectID := *res.Order.ProjectID
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM outbox WHERE aggregate_id = $1`, orderID.String())
		_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID)
	}()

	if res.Order.ProjectKind != orders.KindBrand {
		t.Errorf("ветка заявки %q, ожидали brand_turnkey", res.Order.ProjectKind)
	}

	// Проект — того самого вида: у него нет состава, чеклиста и
	// проверки роликов, и определяется это видом, а не пустотой.
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM projects WHERE id = $1`, projectID).
		Scan(&kind); err != nil {
		t.Fatalf("kind: %v", err)
	}
	if kind != string(projects.KindBrandTurnkey) {
		t.Errorf("вид проекта %q, ожидали brand_turnkey", kind)
	}

	// Чеклист к такому проекту не цепляется сам: проверять нечего и
	// некого.
	var checklist int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_checklist_items WHERE project_id = $1`, projectID).
		Scan(&checklist); err != nil {
		t.Fatalf("checklist: %v", err)
	}
	if checklist != 0 {
		t.Errorf("к проекту без креаторов прицепился чеклист (%d пунктов)", checklist)
	}

	// Рассылки не было: ни кандидатов, ни записей в журнале.
	if res.Broadcast.Recipients != 0 {
		t.Errorf("во второй ветке ушла рассылка на %d человек", res.Broadcast.Recipients)
	}
	var candidates int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM order_candidates WHERE order_id = $1`, orderID).
		Scan(&candidates); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if candidates != 0 {
		t.Errorf("во второй ветке завелись кандидаты (%d) — звать здесь некого", candidates)
	}

	// А пинг менеджерам — такой же: заявку всё так же надо взять. И в
	// нём видно ветку: у проекта без креаторов другой разговор.
	p := lastPayload(t, pool, orderID.String(), "order.submitted")
	if p["project_kind"] != string(orders.KindBrand) {
		t.Errorf("в сообщении менеджерам ветка %v", p["project_kind"])
	}
	if p["videos_count"] != float64(20) {
		t.Errorf("в сообщении %v роликов", p["videos_count"])
	}

	// Отметки креаторов во второй ветке — это перепутанная ветка, а не
	// лишнее поле: принять такую заявку значит завести проект не того
	// вида и обнаружить это по отсутствующему кабинету креатора.
	if _, err := svc.Create(ctx, orders.CreateOrderInput{
		ClientUserID: clientID,
		ProjectKind:  orders.KindBrand,
		StartMonth:   nextMonth(),
		VideosCount:  10,
		CreatorIDs:   creators[:1],
	}, time.Now().UTC()); !errors.Is(err, orders.ErrInvalidInput) {
		t.Errorf("заявка без креаторов с отмеченными креаторами принята: %v", err)
	}
}
