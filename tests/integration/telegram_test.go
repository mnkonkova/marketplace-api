package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"golang.org/x/crypto/bcrypt"

	"marketpclce/internal/auth"
	"marketpclce/internal/eventroute"
	"marketpclce/internal/notifications"
	"marketpclce/internal/telegram"
	"marketpclce/tests/integration"
)

// Привязка к телеграм-ботам: кабинет, бот и вход из мини-аппа.
//
// Ломается это тише всего в двух местах. Первое: код погашен, а
// привязки нет — человек нажал /start, увидел «готово» и не получает
// ничего; по логам такое не видно вовсе. Второе: чужой телеграм
// привязался к аккаунту — уведомления о чужих проектах уходят не тому,
// и узнают об этом от того, кому они пришли.

const tgCreatorToken = "111111:AA-creator-token"

func tgInitData(t *testing.T, tgUserID int64, username string, now time.Time) string {
	t.Helper()
	user := map[string]any{
		"id": tgUserID, "first_name": "Марина", "last_name": "Ким", "username": username,
	}
	raw, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("marshal user: %v", err)
	}
	return telegram.SignForTest(tgCreatorToken, map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"user":      string(raw),
	})
}

// Полный путь привязки из кабинета: ссылка → /start → привязка.
func TestTelegramLinkByCode(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := integration.NewAPIHarness(t, pool)
	userID, cleanup := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanup)

	svc := telegram.NewService(telegram.NewRepo(pool)).
		WithBots("prcreators_bot", "prmclients_bot", "shared-secret")

	start, err := svc.NewLinkStart(ctx, userID, telegram.BotCreator)
	if err != nil {
		t.Fatalf("NewLinkStart: %v", err)
	}
	if !strings.HasPrefix(start.URL, "https://t.me/prcreators_bot?start=") {
		t.Fatalf("ссылка подключения: %q", start.URL)
	}
	code := strings.TrimPrefix(start.URL, "https://t.me/prcreators_bot?start=")

	const tgID = 482512345
	link, err := svc.LinkByCode(ctx, telegram.BotCreator, code, tgID, tgID, "marina")
	if err != nil {
		t.Fatalf("LinkByCode: %v", err)
	}
	if link.UserID != userID || link.TGChatID != tgID {
		t.Errorf("привязка не та: %+v", link)
	}

	// Код одноразовый: вторая попытка тем же кодом — отказ. Иначе
	// утёкшая ссылка остаётся ключом от аккаунта.
	if _, err := svc.LinkByCode(ctx, telegram.BotCreator, code, 999, 999, "other"); !errors.Is(err, telegram.ErrCodeExpired) {
		t.Errorf("повторное использование кода: %v", err)
	}

	// Бот спрашивает «кто это» по одному tg_user_id — больше он ничего
	// не знает.
	who, err := svc.ByTelegram(ctx, telegram.BotCreator, tgID)
	if err != nil || who.UserID != userID {
		t.Fatalf("ByTelegram: %+v, %v", who, err)
	}

	// Чужой телеграм к этому же аккаунту — законно (человек сменил
	// телеграм), а вот наш телеграм к ДРУГОМУ аккаунту — нет.
	other, cleanupOther := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanupOther)
	otherStart, err := svc.NewLinkStart(ctx, other, telegram.BotCreator)
	if err != nil {
		t.Fatalf("NewLinkStart(other): %v", err)
	}
	otherCode := strings.TrimPrefix(otherStart.URL, "https://t.me/prcreators_bot?start=")
	if _, err := svc.LinkByCode(ctx, telegram.BotCreator, otherCode, tgID, tgID, "marina"); !errors.Is(err, telegram.ErrTaken) {
		t.Errorf("чужой телеграм привязался к другому аккаунту: %v", err)
	}

	// Блокировка не удаляет строку: удаление выглядит как «никогда не
	// подключал», и мы бы позвали человека заново.
	if _, err := svc.Blocked(ctx, telegram.BotCreator, tgID); err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	status, err := svc.Status(ctx, userID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.Links) != 1 || status.Links[0].BlockedAt == nil {
		t.Errorf("после блокировки: %+v", status.Links)
	}

	// И писать заблокированному мы больше не будем.
	got, err := svc.Recipients(ctx, telegram.BotCreator, []uuid.UUID{userID})
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("заблокировавший бота остался в получателях: %+v", got)
	}
}

// Отбор получателей: только подключённые, только живые и только в
// пределах дневного потолка.
func TestTelegramRecipientsRespectDailyCap(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := integration.NewAPIHarness(t, pool)
	quiet, cleanupQuiet := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanupQuiet)
	loud, cleanupLoud := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanupLoud)
	none, cleanupNone := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanupNone)

	repo := telegram.NewRepo(pool)
	svc := telegram.NewService(repo).WithBots("c_bot", "cl_bot", "s")
	now := time.Now().UTC()
	for i, id := range []uuid.UUID{quiet, loud} {
		if _, err := repo.LinkDirect(ctx, id, telegram.BotCreator,
			int64(900000+i), int64(900000+i), "u", now); err != nil {
			t.Fatalf("LinkDirect: %v", err)
		}
	}

	// Шумному сегодня уже написали тридцать раз. Тридцать первое
	// сообщение он не прочитает, а заодно перестанет читать и все
	// остальные наши.
	day := now.Truncate(24 * time.Hour)
	for i := 0; i < telegram.DailyCap; i++ {
		if _, err := pool.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4)`, loud, "cap_test_"+strconv.Itoa(i), uuid.New(), day); err != nil {
			t.Fatalf("log: %v", err)
		}
	}

	got, err := svc.Recipients(ctx, telegram.BotCreator, []uuid.UUID{quiet, loud, none})
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if len(got) != 1 || got[0].UserID != quiet {
		t.Fatalf("получатели: %+v — ожидали одного, не упёршегося в потолок", got)
	}
}

// Вход из мини-аппа: новый человек, повторный вход и «у меня уже есть
// аккаунт».
func TestTelegramMiniAppLogin(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	links := telegram.NewRepo(pool)
	issuer := auth.NewTokenIssuer("telegram-test-secret", 15*time.Minute, 7*24*time.Hour)
	svc := auth.NewService(auth.NewRepo(pool), issuer).
		WithTelegram(auth.TelegramConfig{
			CreatorToken: tgCreatorToken,
			TTL:          time.Hour,
		}, links)

	now := time.Now().UTC()
	const tgID = 770077007
	init := tgInitData(t, tgID, "marina", now)

	// Незнакомый телеграм без явного «я новый» — 404, а не молча
	// заведённый второй аккаунт: у человека уже может быть наш.
	if _, err := svc.LoginWithTelegram(ctx, auth.TelegramLogin{
		Bot: telegram.BotCreator, InitData: init,
	}); !errors.Is(err, auth.ErrTelegramUnknown) {
		t.Fatalf("незнакомый телеграм: %v", err)
	}

	res, err := svc.LoginWithTelegram(ctx, auth.TelegramLogin{
		Bot: telegram.BotCreator, InitData: init, Create: true,
	})
	if err != nil {
		t.Fatalf("регистрация из мини-аппа: %v", err)
	}
	if !res.IsNew || res.Kind != auth.KindSpecialist {
		t.Errorf("новый человек: is_new=%v kind=%q", res.IsNew, res.Kind)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, res.UserID)
	})

	// Человек без почты существует: правило «email или phone»
	// перестало быть правилом про почту.
	var email *string
	var tgStored *int64
	if err := pool.QueryRow(ctx,
		`SELECT email, telegram_user_id FROM users WHERE id = $1`, res.UserID).
		Scan(&email, &tgStored); err != nil {
		t.Fatalf("user: %v", err)
	}
	if email != nil {
		t.Errorf("у телеграм-человека завелась почта: %v", *email)
	}
	if tgStored == nil || *tgStored != tgID {
		t.Errorf("telegram_user_id не записан: %v", tgStored)
	}

	// Привязка появилась сама: иначе бот, ради которого он пришёл,
	// ничего не пришлёт.
	if _, err := links.ByTelegram(ctx, telegram.BotCreator, tgID); err != nil {
		t.Errorf("привязка после входа: %v", err)
	}

	// Повторный вход — тот же аккаунт, не новый.
	again, err := svc.LoginWithTelegram(ctx, auth.TelegramLogin{
		Bot: telegram.BotCreator, InitData: tgInitData(t, tgID, "marina", time.Now().UTC()),
	})
	if err != nil {
		t.Fatalf("повторный вход: %v", err)
	}
	if again.IsNew || again.UserID != res.UserID {
		t.Errorf("повторный вход завёл второй аккаунт: %+v", again)
	}

	// Подделанная строка не пускает никого.
	if _, err := svc.LoginWithTelegram(ctx, auth.TelegramLogin{
		Bot:      telegram.BotCreator,
		InitData: strings.Replace(init, strconv.Itoa(tgID), "770077008", 1),
		Create:   true,
	}); !errors.Is(err, telegram.ErrBadSignature) {
		t.Errorf("подделанная подпись принята: %v", err)
	}

	// «У меня уже есть аккаунт»: пароль один раз — и привязка.
	//
	// Своего человека заводим руками: харнессный лежит с непригодным
	// хешем пароля, а здесь проверяется как раз вход паролем.
	const password = "PrMarket!2026-telegram"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	login := "tg-existing-" + uuid.NewString() + "@example.com"
	var existing uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, is_active, email_verified_at)
VALUES ($1, $2, 'client', TRUE, TRUE, now()) RETURNING id`, login, string(hash)).
		Scan(&existing); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, existing)
	})
	const otherTG = 770077009
	linked, err := svc.LoginWithTelegram(ctx, auth.TelegramLogin{
		Bot:      telegram.BotCreator,
		InitData: tgInitData(t, otherTG, "lev", time.Now().UTC()),
		Login:    login,
		Password: password,
	})
	if err != nil {
		t.Fatalf("привязка к существующему: %v", err)
	}
	if linked.UserID != existing {
		t.Fatalf("привязали не к тому аккаунту: %s vs %s", linked.UserID, existing)
	}
	// Заказчик написал креаторскому боту — поднялся до «и снимает, и
	// заказывает». Вниз не опускаем никогда: у человека уже есть
	// кабинет, в котором лежит работа.
	if linked.Kind != auth.KindBoth {
		t.Errorf("роль после креаторского бота: %q, ожидали both", linked.Kind)
	}
}

// Ручки бота закрыты общим секретом, и это единственное, что стоит
// между чужим curl'ом и привязкой чужого телеграма к чужому аккаунту.
func TestBotEndpointsRequireSecret(t *testing.T) {
	pool := integration.Pool(t)
	h := integration.NewAPIHarness(t, pool)

	code, _ := h.Do(t, http.MethodPost, "/api/v1/bot/link", "", map[string]any{
		"bot": "creator", "code": "whatever", "tg_user_id": 1, "tg_chat_id": 1,
	})
	// Харнесс поднимает роутер без телеграм-ручек (секрет не задан) —
	// тогда их просто нет. С секретом они есть, но требуют его.
	if code != http.StatusNotFound && code != http.StatusUnauthorized {
		t.Errorf("ручка бота без секрета ответила %d — ожидали 404 или 401", code)
	}
}

// Сообщения менеджерам едут тем же ботом, а не в n8n.
//
// Решение владельца от 27 сентября: отдельного менеджерского бота нет
// — в общий чат пишет креаторский. Проверяем развилку целиком, потому
// что ошибиться в ней можно тихо и дважды: отправить личное событие в
// группу (тогда «сегодня срок» одного креатора прочитают все) или
// отправить чатовое ещё и в n8n (тогда в группе два одинаковых
// сообщения, и каждый решит, что это сбой).
func TestBotFanoutSplitsPersonalAndManagers(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := integration.NewAPIHarness(t, pool)
	creator, cleanup := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanup)

	repo := telegram.NewRepo(pool)
	if _, err := repo.LinkDirect(ctx, creator, telegram.BotCreator,
		770123456, 770123456, "creator", time.Now().UTC()); err != nil {
		t.Fatalf("LinkDirect: %v", err)
	}

	bot := &recordingBot{}
	crm := &recordingBot{}
	deps := eventroute.Deps{
		Bot:        bot,
		BotUsers:   telegram.NewService(repo),
		CRM:        crm,
		AppBaseURL: "https://example.test",
	}
	handler := deps.CRMHandler("project")

	// 1. Личное: напоминание креатору. Уходит ему, в группу — нет.
	personal, _ := json.Marshal(map[string]any{
		"project_id": uuid.New(), "creator_user_id": creator, "due_date": "2026-10-03",
	})
	if err := handler(ctx, 1, uuid.NewString(),
		"project.publication_due_today", personal); err != nil {
		t.Fatalf("личное событие: %v", err)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("в бот ушло %d сообщений, ожидали одно", len(bot.sent))
	}
	if got := bot.sent[0]["audience"]; got != "person" {
		t.Errorf("личное событие поехало как %v", got)
	}
	if rec, _ := bot.sent[0]["recipients"].([]any); len(rec) != 1 {
		t.Errorf("получателей %d, ожидали одного", len(rec))
	}

	// 2. Чатовое: просрочка. Уходит в группу — и НЕ дублируется в n8n.
	chat, _ := json.Marshal(map[string]any{
		"project_id": uuid.New(), "project_title": "Проект", "days_overdue": 2,
	})
	if err := handler(ctx, 2, uuid.NewString(),
		"project.publication_overdue", chat); err != nil {
		t.Fatalf("чатовое событие: %v", err)
	}
	if len(bot.sent) != 2 {
		t.Fatalf("после чатового в боте %d сообщений", len(bot.sent))
	}
	if got := bot.sent[1]["audience"]; got != "managers" {
		t.Errorf("чатовое событие поехало как %v", got)
	}
	if rec, _ := bot.sent[1]["recipients"].([]any); len(rec) != 0 {
		t.Errorf("в сообщение менеджерам попали получатели: %v", rec)
	}
	// В n8n личное событие уезжает как раньше (там оно попадает в
	// ветку «неизвестное» и никуда не пишется), а вот ЧАТОВОЕ — не
	// должно: иначе в группе два одинаковых сообщения.
	for _, m := range crm.sent {
		if m["event_type"] == "project.publication_overdue" {
			t.Errorf("чатовое событие ушло ещё и в n8n — в группе будет два одинаковых сообщения")
		}
	}
}

// recordingBot — и BotSender, и Dispatcher: обе роли нужны одному
// тесту, а заводить два типа ради двух методов незачем.
type recordingBot struct{ sent []map[string]any }

func (r *recordingBot) SendEnvelope(_ context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	r.sent = append(r.sent, m)
	return nil
}

func (r *recordingBot) Send(_ context.Context, p notifications.Payload) error {
	r.sent = append(r.sent, map[string]any{"event_type": p.EventType})
	return nil
}

// Билет привязки: регистрация идёт на сайте, а телеграм привязывается
// сам.
//
// Это замена регистрации в мини-аппе (решение владельца от 27
// сентября): «я новый» нажимали и те, у кого аккаунт давно есть, и
// телеграм оказывался привязан к пустому дублю. Теперь мини-апп
// выдаёт билет, человек заводит аккаунт в браузере, и билет гасится
// из-под свежей сессии.
//
// Проверяем то, из-за чего механизм и существует: билет одноразовый,
// протухает, не привязывает чужой телеграм и ничего не значит сам по
// себе — без сессии предъявить его некому.
func TestTelegramLinkTicket(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	h := integration.NewAPIHarness(t, pool)

	links := telegram.NewRepo(pool)
	issuer := auth.NewTokenIssuer("telegram-ticket-secret", 15*time.Minute, 7*24*time.Hour)
	authSvc := auth.NewService(auth.NewRepo(pool), issuer).
		WithTelegram(auth.TelegramConfig{CreatorToken: tgCreatorToken, TTL: time.Hour}, links)
	tgSvc := telegram.NewService(links).WithBots("c_bot", "cl_bot", "s")

	const tgID = 660066006
	init := tgInitData(t, tgID, "fresh", time.Now().UTC())

	code, err := authSvc.TelegramLinkTicket(ctx, telegram.BotCreator, init)
	if err != nil {
		t.Fatalf("TelegramLinkTicket: %v", err)
	}
	if code == "" {
		t.Fatal("билет пустой")
	}

	// Пока человек не зарегистрировался, привязки нет: билет сам по
	// себе не привязывает ничего.
	if _, err := links.ByTelegram(ctx, telegram.BotCreator, tgID); !errors.Is(err, telegram.ErrNotFound) {
		t.Errorf("билет привязал телеграм до регистрации: %v", err)
	}

	// Человек завёл аккаунт в браузере — гасим билет из-под его
	// сессии.
	userID, cleanup := h.NewUser(t, integration.UserOpts{Kind: "specialist"})
	t.Cleanup(cleanup)
	link, err := tgSvc.Claim(ctx, userID, code)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if link.UserID != userID || link.TGUserID != tgID {
		t.Errorf("привязалось не то: %+v", link)
	}

	// Второй раз тем же билетом — нет: иначе утёкший адрес анкеты
	// остаётся ключом от привязки.
	other, cleanupOther := h.NewUser(t, integration.UserOpts{Kind: "client"})
	t.Cleanup(cleanupOther)
	if _, err := tgSvc.Claim(ctx, other, code); !errors.Is(err, telegram.ErrCodeExpired) {
		t.Errorf("билет погасили дважды: %v", err)
	}

	// Чужой билет к чужому телеграму: этот телеграм уже занят первым.
	second, err := authSvc.TelegramLinkTicket(ctx, telegram.BotCreator,
		tgInitData(t, tgID, "fresh", time.Now().UTC()))
	if err != nil {
		t.Fatalf("второй билет: %v", err)
	}
	if _, err := tgSvc.Claim(ctx, other, second); !errors.Is(err, telegram.ErrTaken) {
		t.Errorf("телеграм увели у первого аккаунта: %v", err)
	}

	// Несуществующий билет — не ошибка сервера, а «нет такого».
	if _, err := tgSvc.Claim(ctx, other, "no-such-ticket"); !errors.Is(err, telegram.ErrNotFound) {
		t.Errorf("несуществующий билет: %v", err)
	}
}

// ---- очередь сообщений ----
//
// Доставка перевёрнута: мы кладём сообщение в таблицу, бот забирает
// сам и квитирует. Повод был не теоретический — первого октября 2026
// уведомление потерялось потому, что маршрут с российской ВДС до
// Railway обрывался у хостера, а воркер честно отстучал десять попыток
// в пустоту. После переворота дозваниваться до облака не нужно вовсе:
// работает направление, на котором и так живут привязка и мини-апп.

// Конверт доходит до очереди, выдаётся один раз и закрывается ответом.
func TestBotQueueDeliversOnce(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	queue := telegram.NewQueue(pool)
	repo := telegram.NewRepo(pool)

	// Чистим хвост от соседних прогонов: выдача идёт по всей таблице.
	if _, err := pool.Exec(ctx, `DELETE FROM bot_messages`); err != nil {
		t.Fatalf("очистка очереди: %v", err)
	}

	env := map[string]any{
		"bot": telegram.BotCreator, "audience": "person",
		"event_type": "project.publication_due_today", "event_id": "90001",
		"recipients": []map[string]any{{"tg_chat_id": 42}},
	}
	if err := queue.SendEnvelope(ctx, env); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}

	got, err := repo.Lease(ctx, 10)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("выдано %d сообщений, ожидали одно", len(got))
	}
	if got[0].Bot != telegram.BotCreator || got[0].Audience != "person" {
		t.Errorf("конверт адресован неверно: %+v", got[0])
	}
	// Конверт отдаётся ЦЕЛИКОМ: бот ничего не досчитывает, получателей
	// считаем мы.
	if !strings.Contains(string(got[0].Envelope), "tg_chat_id") {
		t.Errorf("в конверте нет получателей: %s", got[0].Envelope)
	}

	// Второй опрос подряд ничего не даёт: сообщение в аренде.
	again, err := repo.Lease(ctx, 10)
	if err != nil {
		t.Fatalf("повторный Lease: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("арендованное выдали второй раз: %d", len(again))
	}

	if err := repo.Ack(ctx, []int64{got[0].ID}, nil); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	after, err := repo.Lease(ctx, 10)
	if err != nil {
		t.Fatalf("Lease после ack: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("доставленное вернулось в очередь: %d", len(after))
	}
}

// Повтор outbox не плодит второе сообщение: ключ — id события.
func TestBotQueueIsIdempotent(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM bot_messages`); err != nil {
		t.Fatalf("очистка очереди: %v", err)
	}
	queue := telegram.NewQueue(pool)

	env := map[string]any{
		"bot": telegram.BotClient, "audience": "person",
		"event_type": "project.client_new_video", "event_id": "90002",
	}
	for i := 0; i < 3; i++ {
		if err := queue.SendEnvelope(ctx, env); err != nil {
			t.Fatalf("SendEnvelope %d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM bot_messages WHERE event_id = 90002`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("строк по одному событию %d, ожидали одну", n)
	}
}

// Не доставленное возвращается в очередь с причиной: упавший бот
// ничего не теряет.
func TestBotQueueReturnsFailed(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM bot_messages`); err != nil {
		t.Fatalf("очистка очереди: %v", err)
	}
	queue := telegram.NewQueue(pool)
	repo := telegram.NewRepo(pool)

	if err := queue.SendEnvelope(ctx, map[string]any{
		"bot": telegram.BotCreator, "audience": "managers",
		"event_type": "project.manager_digest", "event_id": "90003",
	}); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	got, err := repo.Lease(ctx, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("Lease: %v, %d", err, len(got))
	}
	if err := repo.Ack(ctx, nil, map[int64]string{got[0].ID: "telegram 502"}); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	// Вернулось сразу, не дожидаясь срока аренды: мы знаем, что
	// попытка была и не удалась.
	back, err := repo.Lease(ctx, 10)
	if err != nil {
		t.Fatalf("Lease после отказа: %v", err)
	}
	if len(back) != 1 || back[0].ID != got[0].ID {
		t.Fatalf("не доставленное не вернулось в очередь: %+v", back)
	}
	var attempts int
	var lastErr *string
	if err := pool.QueryRow(ctx,
		`SELECT attempts, last_error FROM bot_messages WHERE id = $1`, got[0].ID).
		Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if attempts != 2 {
		t.Errorf("попыток %d, ожидали две: выдача считается попыткой", attempts)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "502") {
		t.Errorf("причина отказа не записана: %v", lastErr)
	}
}

// Доставленное старше срока убирается: очередь — транспорт, а не журнал.
func TestBotQueueCleanupKeepsPending(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM bot_messages`); err != nil {
		t.Fatalf("очистка очереди: %v", err)
	}
	queue := telegram.NewQueue(pool)
	repo := telegram.NewRepo(pool)

	for _, id := range []string{"90004", "90005"} {
		if err := queue.SendEnvelope(ctx, map[string]any{
			"bot": telegram.BotCreator, "audience": "person",
			"event_type": "project.publication_accepted", "event_id": id,
		}); err != nil {
			t.Fatalf("SendEnvelope: %v", err)
		}
	}
	// Первое «доставлено давно», второе ждёт отправки.
	if _, err := pool.Exec(ctx, `
UPDATE bot_messages SET delivered_at = now() - interval '30 days' WHERE event_id = 90004`); err != nil {
		t.Fatalf("состарить: %v", err)
	}

	removed, err := repo.CleanupDelivered(ctx, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupDelivered: %v", err)
	}
	if removed != 1 {
		t.Errorf("убрано %d строк, ожидали одну", removed)
	}
	left, err := repo.Lease(ctx, 10)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("неотправленное не должно убираться уборкой: осталось %d", len(left))
	}
}
