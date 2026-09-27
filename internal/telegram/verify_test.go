package telegram_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"marketpclce/internal/telegram"
)

const token = "123456:AA-test-token"

func signed(t *testing.T, now time.Time, user string) string {
	t.Helper()
	return telegram.SignForTest(token, map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"query_id":  "AAH-query",
		"user":      user,
	})
}

const userJSON = `{"id":482512345,"first_name":"Марина","last_name":"Ким","username":"marina","photo_url":"https://t.me/i/userpic/320/x.jpg"}`

// Подпись сходится — и разбирается ровно то, что положили.
//
// Проверяем и имя с кириллицей: initData кодирует значения процентами,
// и склейка проверочной строки из СЫРЫХ кусков разваливается на первом
// же не-ASCII символе — подпись при этом не сходится, а выглядит это
// как «Telegram прислал мусор».
func TestVerifyHappy(t *testing.T) {
	now := time.Now().UTC()
	data, err := telegram.Verify(signed(t, now, userJSON), token, time.Hour, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if data.User.ID != 482512345 {
		t.Errorf("id %d", data.User.ID)
	}
	if data.User.DisplayName() != "Марина Ким" {
		t.Errorf("имя %q", data.User.DisplayName())
	}
	if data.QueryID != "AAH-query" {
		t.Errorf("query_id %q", data.QueryID)
	}
	if data.AuthDate.Unix() != now.Unix() {
		t.Errorf("auth_date %v", data.AuthDate)
	}
}

// Чужой токен — чужая подпись. Это главная проверка файла: без неё
// мини-апп пускает любого, кто прислал строку нужной формы.
func TestVerifyRejectsForeignToken(t *testing.T) {
	now := time.Now().UTC()
	init := telegram.SignForTest("999:other-bot", map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"user":      userJSON,
	})
	if _, err := telegram.Verify(init, token, time.Hour, now); !errors.Is(err, telegram.ErrBadSignature) {
		t.Fatalf("подпись чужого бота принята: %v", err)
	}
}

// Правка поля после подписи ломает её — иначе id пользователя можно
// было бы просто переписать на чужой.
func TestVerifyRejectsTamperedUser(t *testing.T) {
	now := time.Now().UTC()
	init := signed(t, now, userJSON)
	tampered := strings.Replace(init, "482512345", "482599999", 1)
	if tampered == init {
		t.Fatal("подмена не сработала — тест ничего не проверяет")
	}
	if _, err := telegram.Verify(tampered, token, time.Hour, now); !errors.Is(err, telegram.ErrBadSignature) {
		t.Fatalf("подделанные данные приняты: %v", err)
	}
}

// Протухшая строка: webview живёт долго, и вкладку открывают через
// сутки. Подпись у неё верная — отказывать надо по сроку.
func TestVerifyExpires(t *testing.T) {
	old := time.Now().UTC().Add(-25 * time.Hour)
	init := signed(t, old, userJSON)
	if _, err := telegram.Verify(init, token, 24*time.Hour, time.Now().UTC()); !errors.Is(err, telegram.ErrExpired) {
		t.Fatalf("протухшая строка принята: %v", err)
	}
	// ttl=0 — срок не проверяем: так удобно на стенде, где часы уезжают.
	if _, err := telegram.Verify(init, token, 0, time.Now().UTC()); err != nil {
		t.Fatalf("без ttl строка должна пройти: %v", err)
	}
}

// Не-initData отсекается до криптографии: пустая строка, строка без
// hash, строка без user.
func TestVerifyMalformed(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]string{
		"пустая":   "",
		"без hash": "auth_date=1&user=%7B%22id%22%3A1%7D",
		"без user": telegram.SignForTest(token, map[string]string{
			"auth_date": strconv.FormatInt(now.Unix(), 10),
		}),
		"user без id": telegram.SignForTest(token, map[string]string{
			"auth_date": strconv.FormatInt(now.Unix(), 10),
			"user":      `{"first_name":"Без id"}`,
		}),
		"это бот": telegram.SignForTest(token, map[string]string{
			"auth_date": strconv.FormatInt(now.Unix(), 10),
			"user":      `{"id":7,"is_bot":true,"first_name":"Бот"}`,
		}),
	}
	for name, init := range cases {
		if _, err := telegram.Verify(init, token, time.Hour, now); !errors.Is(err, telegram.ErrMalformed) {
			t.Errorf("%s: ожидали ErrMalformed, получили %v", name, err)
		}
	}
}

// Без токена не проверяем вовсе: пустой токен дал бы стабильный HMAC,
// и подпись «сошлась бы» у любого, кто знает, что бот выключен.
func TestVerifyRequiresToken(t *testing.T) {
	now := time.Now().UTC()
	if _, err := telegram.Verify(signed(t, now, userJSON), "", time.Hour, now); err == nil {
		t.Fatal("проверка без токена бота прошла")
	}
}

// Имя собирается из того, что есть: пустое лучше выдуманного.
func TestDisplayNameFallbacks(t *testing.T) {
	cases := []struct {
		u    telegram.User
		want string
	}{
		{telegram.User{FirstName: "Лев", LastName: "Северов"}, "Лев Северов"},
		{telegram.User{FirstName: "Лев"}, "Лев"},
		{telegram.User{Username: "lev"}, "@lev"},
		{telegram.User{}, ""},
	}
	for _, c := range cases {
		if got := c.u.DisplayName(); got != c.want {
			t.Errorf("DisplayName(%+v) = %q, ожидали %q", c.u, got, c.want)
		}
	}
}

// Подпись сходится и тогда, когда клиент положил в проверочную строку
// поле signature.
//
// Telegram добавил его для сторонней проверки уже после того, как
// описал алгоритм, и клиенты разных версий считают hash по-разному.
// Принять только один вариант — значит отказать живому человеку на
// ровном месте, а перебрать оба стоит одного HMAC: подделать подпись
// это не помогает, ключ всё тот же.
func TestVerifyAcceptsSignatureInCheckString(t *testing.T) {
	now := time.Now().UTC()
	fields := map[string]string{
		"auth_date":     strconv.FormatInt(now.Unix(), 10),
		"chat_instance": "-1234567890",
		"chat_type":     "sender",
		"signature":     "3zYm4b_fake_ed25519_signature",
		"user":          userJSON,
	}
	// SignForTest считает hash по ВСЕМ переданным полям, то есть
	// включая signature, — ровно как новые клиенты.
	init := telegram.SignForTest(token, fields)
	if _, err := telegram.Verify(init, token, time.Hour, now); err != nil {
		t.Fatalf("строка с signature в подписи отвергнута: %v", err)
	}

	// И старый вариант — без signature в проверочной строке — тоже
	// принимается: клиенты постарше живы.
	old := map[string]string{"auth_date": fields["auth_date"], "user": userJSON}
	initOld := telegram.SignForTest(token, old)
	// Дописываем signature УЖЕ ПОСЛЕ подписи, как сделал бы клиент,
	// который его не учитывает.
	if _, err := telegram.Verify(initOld+"&signature=3zYm4b_fake", token, time.Hour, now); err != nil {
		t.Fatalf("строка без signature в подписи отвергнута: %v", err)
	}
}

// В отказе «не разобрались» перечислены пришедшие поля — без значений.
//
// Без них жалоба «не пускает» упирается в догадки: строку живого
// человека мы посмотреть не можем, в ней его подпись.
func TestMalformedNamesTheFields(t *testing.T) {
	now := time.Now().UTC()
	init := telegram.SignForTest(token, map[string]string{
		"auth_date":     strconv.FormatInt(now.Unix(), 10),
		"chat_instance": "-1",
	})
	_, err := telegram.Verify(init, token, time.Hour, now)
	if err == nil {
		t.Fatal("строка без user принята")
	}
	msg := err.Error()
	for _, want := range []string{"auth_date", "chat_instance", "hash"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в отказе нет поля %q: %s", want, msg)
		}
	}
	// А значений в нём быть не должно: там подпись.
	if strings.Contains(msg, "=") {
		t.Errorf("в отказе просочились значения: %s", msg)
	}
}
