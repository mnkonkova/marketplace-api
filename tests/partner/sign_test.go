// Подпись вебхука — единственное, чем наш запрос к «Боту Работ» отличается от
// постороннего. Здесь проверяется, что она считается от тела целиком и от
// общего секрета: подпись «по коду» или по части тела позволила бы подменить
// аккаунт, оставив её валидной.
package partner_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"marketpclce/internal/partner"
)

// same повторяет то, что делает соседняя сторона при проверке: ключ — общий
// секрет, сообщение — тело запроса байт в байт.
func same(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSignatureMatchesTheOtherSide(t *testing.T) {
	body := []byte(`{"code":"abc","user_id":"u1","email":null,"issued_at":"2026-08-12T10:00:00Z"}`)
	got := partner.SignForTest("secret", body)

	if want := same("secret", body); got != want {
		t.Fatalf("подпись разошлась с проверяющей стороной:\n got: %s\nwant: %s", got, want)
	}
}

func TestSignatureCoversWholeBody(t *testing.T) {
	base := []byte(`{"code":"abc","user_id":"u1"}`)
	swapped := []byte(`{"code":"abc","user_id":"u2"}`)

	if partner.SignForTest("secret", base) == partner.SignForTest("secret", swapped) {
		t.Fatal("подмена аккаунта не изменила подпись — тело подписано не целиком")
	}
}

func TestSignatureDependsOnSecret(t *testing.T) {
	body := []byte(`{"code":"abc"}`)

	if partner.SignForTest("one", body) == partner.SignForTest("two", body) {
		t.Fatal("подпись не зависит от секрета — подделать сможет кто угодно")
	}
}

func TestDisabledWithoutSecretOrWebhook(t *testing.T) {
	cases := []struct {
		name    string
		webhook string
		secret  string
		want    bool
	}{
		{"всё задано", "https://example.org/hook", "s3cret", true},
		{"нет секрета", "https://example.org/hook", "", false},
		{"нет адреса", "", "s3cret", false},
		{"ничего нет", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := partner.NewService(nil, c.webhook, c.secret)
			if got := svc.Enabled(); got != c.want {
				t.Fatalf("Enabled() = %v, ожидали %v", got, c.want)
			}
		})
	}
}
