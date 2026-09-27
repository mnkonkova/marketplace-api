// Package telegram — проверка данных, пришедших из мини-аппа Telegram.
//
// Мини-апп отдаёт клиенту строку `Telegram.WebApp.initData` — обычный
// query-string с полями пользователя и подписью. Подпись считается
// токеном бота, то есть проверить её может только тот, у кого этот
// токен есть. Поэтому проверка живёт у нас, а не на стороне бота:
// гонять её через чужой сервис значит уронить вход в мини-апп вместе с
// ним.
//
// Строку принимаем СЫРОЙ и не пересобираем. Любая нормализация на
// клиенте — перекодировать, отсортировать, убрать пустое — ломает
// подпись, и отличить это от подделки нечем.
package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrBadSignature — подпись не сошлась. Либо чужой бот, либо
	// подделка, либо строку по дороге пересобрали.
	ErrBadSignature = errors.New("telegram init data signature mismatch")
	// ErrExpired — данные старше допустимого. Мини-апп живёт в
	// webview, и вкладку могут открыть через сутки.
	ErrExpired = errors.New("telegram init data expired")
	// ErrMalformed — это не initData: нет hash, нет user, битый JSON.
	ErrMalformed = errors.New("telegram init data malformed")
)

// User — кто пришёл. Ровно то, что кладёт Telegram: ни почты, ни
// телефона в initData нет вовсе.
type User struct {
	ID           int64  `json:"id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	PhotoURL     string `json:"photo_url"`
	LanguageCode string `json:"language_code"`
	IsBot        bool   `json:"is_bot"`
}

// DisplayName — «Имя Фамилия», а если их нет — @username.
//
// Пустое имя лучше выдуманного: «Пользователь 482…» в списке проектов
// читается как ошибка данных, а пустое поле менеджер заполнит сам.
func (u User) DisplayName() string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return ""
}

// Data — разобранная initData.
type Data struct {
	User     User
	AuthDate time.Time
	// QueryID — идентификатор запроса мини-аппа. Нужен только для
	// answerWebAppQuery, который делает бот; храним, чтобы не разбирать
	// строку второй раз.
	QueryID string
}

// Verify — проверить подпись и срок, вернуть разобранные данные.
//
// ttl <= 0 — срок не проверяем: так удобно в тестах и на стенде, где
// часы уезжают. В проде он обязан быть задан.
func Verify(initData, botToken string, ttl time.Duration, now time.Time) (Data, error) {
	if strings.TrimSpace(initData) == "" {
		return Data{}, fmt.Errorf("%w: пустая строка", ErrMalformed)
	}
	if botToken == "" {
		return Data{}, errors.New("telegram bot token is not configured")
	}

	// url.ParseQuery, а не разбор руками: Telegram кодирует значения
	// процентами, и склеенный из них JSON иначе развалится на первом же
	// эмодзи в имени.
	values, err := url.ParseQuery(initData)
	if err != nil {
		return Data{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	got := values.Get("hash")
	if got == "" {
		return Data{}, fmt.Errorf("%w: нет hash (поля: %s)", ErrMalformed, keysOf(values))
	}

	// Проверочная строка: все поля кроме hash, «ключ=значение», по
	// одному на строку, отсортированные по ключу.
	//
	// Проверяем ДВА варианта — с полем signature и без него. Telegram
	// добавил его для сторонней проверки (Ed25519) уже после того, как
	// описал этот алгоритм, и клиенты разных версий кладут в
	// проверочную строку разное. Один вариант из двух — это отказ
	// живому человеку на ровном месте, а перебрать оба стоит одного
	// HMAC: подделать подпись это не помогает, ключ всё тот же.
	if !signatureOK(values, botToken, got) {
		return Data{}, ErrBadSignature
	}

	out := Data{QueryID: values.Get("query_id")}

	if raw := values.Get("auth_date"); raw != "" {
		sec, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Data{}, fmt.Errorf("%w: auth_date", ErrMalformed)
		}
		out.AuthDate = time.Unix(sec, 0).UTC()
		if ttl > 0 && now.Sub(out.AuthDate) > ttl {
			return Data{}, ErrExpired
		}
	}

	rawUser := values.Get("user")
	if rawUser == "" {
		// Бывает у инлайн-режима: подпись верна, а человека нет. Нам
		// такое не подходит — привязывать нечего. Перечисляем, что
		// пришло: без этого «данные не разобрались» не отличить от
		// десятка других причин, а увидеть строку живого человека мы
		// не можем — в ней его подпись.
		return Data{}, fmt.Errorf("%w: нет user (поля: %s)", ErrMalformed, keysOf(values))
	}
	if err := json.Unmarshal([]byte(rawUser), &out.User); err != nil {
		return Data{}, fmt.Errorf("%w: user: %v", ErrMalformed, err)
	}
	if out.User.ID == 0 {
		return Data{}, fmt.Errorf("%w: user.id пуст", ErrMalformed)
	}
	if out.User.IsBot {
		return Data{}, fmt.Errorf("%w: это бот, а не человек", ErrMalformed)
	}
	return out, nil
}

// SignForTest — собрать подписанную initData. Только для тестов и
// стенда: в бою строку подписывает Telegram.
//
// Живёт рядом с проверкой намеренно. Тест, который подписывает строку
// своей копией алгоритма, проверяет сам себя: перепутайте порядок
// HMAC — и проверка, и подпись ошибутся одинаково, а тест останется
// зелёным.
func SignForTest(botToken string, fields map[string]string) string {
	parts := make([]string, 0, len(fields))
	for k, v := range fields {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	keyMac := hmac.New(sha256.New, []byte("WebAppData"))
	keyMac.Write([]byte(botToken))
	mac := hmac.New(sha256.New, keyMac.Sum(nil))
	mac.Write([]byte(strings.Join(parts, "\n")))

	q := url.Values{}
	for k, v := range fields {
		q.Set(k, v)
	}
	q.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	return q.Encode()
}

// signatureOK — сходится ли подпись хотя бы в одном из двух вариантов
// проверочной строки: с полем signature и без него.
func signatureOK(values url.Values, botToken, got string) bool {
	for _, withSignature := range []bool{false, true} {
		parts := make([]string, 0, len(values))
		for k, v := range values {
			if k == "hash" || len(v) == 0 {
				continue
			}
			if k == "signature" && !withSignature {
				continue
			}
			parts = append(parts, k+"="+v[0])
		}
		sort.Strings(parts)

		// Ключ — HMAC от токена бота под меткой WebAppData, и только
		// потом им подписывается сама строка. Порядок не
		// переставляется: с ним сходится подпись Telegram, без него —
		// нет.
		keyMac := hmac.New(sha256.New, []byte("WebAppData"))
		keyMac.Write([]byte(botToken))
		mac := hmac.New(sha256.New, keyMac.Sum(nil))
		mac.Write([]byte(strings.Join(parts, "\n")))
		want := hex.EncodeToString(mac.Sum(nil))

		// hmac.Equal, а не ==: сравнение строк выходит за первым же
		// несовпавшим байтом и рассказывает подбирающему, насколько он
		// близок.
		if hmac.Equal([]byte(want), []byte(got)) {
			return true
		}
		if _, ok := values["signature"]; !ok {
			// Второго варианта не существует — поля нет.
			break
		}
	}
	return false
}

// keysOf — какие поля пришли, без значений.
//
// Значения показывать нельзя: там подпись и данные человека. А имена
// полей — единственное, чем «данные не разобрались» отличается от
// «данные не разобрались» по другой причине; без них разбор такой
// жалобы упирается в догадки.
func keysOf(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
