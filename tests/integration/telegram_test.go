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
		WithBots("sotka_creator_bot", "sotka_client_bot", "shared-secret")

	start, err := svc.NewLinkStart(ctx, userID, telegram.BotCreator)
	if err != nil {
		t.Fatalf("NewLinkStart: %v", err)
	}
	if !strings.HasPrefix(start.URL, "https://t.me/sotka_creator_bot?start=") {
		t.Fatalf("ссылка подключения: %q", start.URL)
	}
	code := strings.TrimPrefix(start.URL, "https://t.me/sotka_creator_bot?start=")

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
	otherCode := strings.TrimPrefix(otherStart.URL, "https://t.me/sotka_creator_bot?start=")
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
	const password = "Sotka!2026-telegram"
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
