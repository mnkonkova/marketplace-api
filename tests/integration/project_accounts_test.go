package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"marketpclce/internal/publications"
	"marketpclce/tests/integration"
)

// Доступы к аккаунтам бренда: заполняет менеджер, читает заказчик.
//
// Главное здесь — пароль. Он не должен лежать в базе текстом (дамп и
// бэкап уезжают туда, где чужим паролям от соцсетей не место), не должен
// ездить в списке проекта и не должен теряться, когда менеджер правит
// соседнее поле.

func testSecrets(t *testing.T) *publications.Secrets {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random key: %v", err)
	}
	sec, err := publications.NewSecrets(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("NewSecrets: %v", err)
	}
	return sec
}

func strptr(s string) *string { return &s }

func TestProjectAccounts(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool)).WithSecrets(testSecrets(t))
	manager := creators[0]

	acc, err := svc.ManagerAddAccount(ctx, projectID, manager, publications.AccountInput{
		Platform: "tiktok",
		Title:    "основной",
		URL:      "https://www.tiktok.com/@brand",
		Login:    "brand.smm",
		Password: strptr("Autumn-2026!"),
		Note:     "двухфакторка на телефоне Оли",
	})
	if err != nil {
		t.Fatalf("ManagerAddAccount: %v", err)
	}
	if !acc.HasPassword {
		t.Fatal("has_password=false у доступа с паролем")
	}

	t.Run("пароль лежит в базе зашифрованным", func(t *testing.T) {
		var enc []byte
		if err := pool.QueryRow(ctx,
			`SELECT secret_enc FROM project_accounts WHERE id = $1`, acc.ID).Scan(&enc); err != nil {
			t.Fatalf("read secret_enc: %v", err)
		}
		if len(enc) == 0 {
			t.Fatal("пароль не записан")
		}
		if strings.Contains(string(enc), "Autumn-2026!") {
			t.Fatal("пароль лежит в базе открытым текстом")
		}
	})

	t.Run("в списке пароля нет, есть только признак", func(t *testing.T) {
		items, err := svc.Accounts(ctx, projectID)
		if err != nil {
			t.Fatalf("Accounts: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("ожидали один доступ, получили %d", len(items))
		}
		if !items[0].HasPassword {
			t.Fatal("has_password потерялся")
		}
		if items[0].Login != "brand.smm" || items[0].URL != "https://www.tiktok.com/@brand" {
			t.Fatalf("ссылка и логин не сохранились: %+v", items[0])
		}
	})

	t.Run("пароль отдаётся отдельной ручкой и совпадает", func(t *testing.T) {
		got, err := svc.RevealAccountPassword(ctx, projectID, acc.ID)
		if err != nil {
			t.Fatalf("RevealAccountPassword: %v", err)
		}
		if got != "Autumn-2026!" {
			t.Fatalf("пароль %q, ожидали Autumn-2026!", got)
		}
	})

	t.Run("правка логина пароль не стирает", func(t *testing.T) {
		if _, err := svc.ManagerUpdateAccount(ctx, projectID, acc.ID, publications.AccountInput{
			Platform: "tiktok",
			Title:    "основной",
			URL:      "https://www.tiktok.com/@brand",
			Login:    "brand.smm.new",
			Note:     "двухфакторка на телефоне Оли",
		}); err != nil {
			t.Fatalf("ManagerUpdateAccount: %v", err)
		}
		got, err := svc.RevealAccountPassword(ctx, projectID, acc.ID)
		if err != nil || got != "Autumn-2026!" {
			t.Fatalf("пароль после правки логина: %q, %v", got, err)
		}
	})

	t.Run("пустой пароль стирает его", func(t *testing.T) {
		updated, err := svc.ManagerUpdateAccount(ctx, projectID, acc.ID, publications.AccountInput{
			Platform: "tiktok",
			Login:    "brand.smm.new",
			Password: strptr(""),
		})
		if err != nil {
			t.Fatalf("ManagerUpdateAccount(стереть): %v", err)
		}
		if updated.HasPassword {
			t.Fatal("has_password=true после стирания")
		}
		if _, err := svc.RevealAccountPassword(ctx, projectID, acc.ID); !errors.Is(err, publications.ErrNotFound) {
			t.Fatalf("ожидали ErrNotFound, получили %v", err)
		}
	})

	t.Run("чужой проект доступов не видит", func(t *testing.T) {
		other, _, cleanupOther := setupCreatorsProject(t, pool)
		defer cleanupOther()
		items, err := svc.Accounts(ctx, other)
		if err != nil {
			t.Fatalf("Accounts(чужой): %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("в чужом проекте видно %d доступов", len(items))
		}
	})

	t.Run("доступ без ссылки, логина и названия не заводится", func(t *testing.T) {
		_, err := svc.ManagerAddAccount(ctx, projectID, manager, publications.AccountInput{
			Platform: "vk",
			Password: strptr("x"),
		})
		if !errors.Is(err, publications.ErrInvalidInput) {
			t.Fatalf("ожидали ErrInvalidInput, получили %v", err)
		}
	})

	t.Run("удаление", func(t *testing.T) {
		if err := svc.RemoveAccount(ctx, projectID, acc.ID); err != nil {
			t.Fatalf("RemoveAccount: %v", err)
		}
		if err := svc.RemoveAccount(ctx, projectID, acc.ID); !errors.Is(err, publications.ErrNotFound) {
			t.Fatalf("повторное удаление: ожидали ErrNotFound, получили %v", err)
		}
	})
}

// Без ключа в окружении пароли не заводятся — и об этом говорят прямо, а
// не сохраняют их в открытую.
func TestProjectAccountsWithoutKey(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	projectID, creators, cleanup := setupCreatorsProject(t, pool)
	defer cleanup()

	svc := publications.NewService(publications.NewRepo(pool))
	if svc.SecretsEnabled() {
		t.Fatal("без ключа SecretsEnabled=true")
	}

	_, err := svc.ManagerAddAccount(ctx, projectID, creators[0], publications.AccountInput{
		Platform: "vk",
		Login:    "brand",
		Password: strptr("secret"),
	})
	if !errors.Is(err, publications.ErrSecretsDisabled) {
		t.Fatalf("ожидали ErrSecretsDisabled, получили %v", err)
	}

	// Логин и ссылка без пароля — законный режим: половина пользы
	// доступов в том, чтобы знать, где смотреть.
	acc, err := svc.ManagerAddAccount(ctx, projectID, creators[0], publications.AccountInput{
		Platform: "vk",
		Login:    "brand",
		URL:      "https://vk.com/brand",
	})
	if err != nil {
		t.Fatalf("доступ без пароля: %v", err)
	}
	if acc.HasPassword {
		t.Fatal("has_password=true у доступа без пароля")
	}
	if _, err := svc.RevealAccountPassword(ctx, projectID, acc.ID); !errors.Is(err, publications.ErrSecretsDisabled) {
		t.Fatalf("показ пароля без ключа: %v", err)
	}
}

// Шифрование: один и тот же пароль двух аккаунтов не даёт одинаковых
// байтов, а испорченный шифротекст не расшифровывается в мусор.
func TestAccountSecretsCrypto(t *testing.T) {
	sec := testSecrets(t)

	a, err := sec.Seal("one-and-the-same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := sec.Seal("one-and-the-same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if string(a) == string(b) {
		t.Fatal("одинаковые байты у двух записей одного пароля")
	}

	got, err := sec.Open(a)
	if err != nil || got != "one-and-the-same" {
		t.Fatalf("Open: %q, %v", got, err)
	}

	broken := append([]byte(nil), a...)
	broken[len(broken)-1] ^= 0xFF
	if _, err := sec.Open(broken); err == nil {
		t.Fatal("испорченный шифротекст расшифровался")
	}

	other := testSecrets(t)
	if _, err := other.Open(a); err == nil {
		t.Fatal("чужой ключ расшифровал пароль")
	}

	if _, err := publications.NewSecrets("короткий"); err == nil {
		t.Fatal("кривой ключ принят")
	}
	if _, err := publications.NewSecrets(base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("ключ на 16 байт принят")
	}

	off, err := publications.NewSecrets("")
	if err != nil {
		t.Fatalf("пустой ключ — законный режим: %v", err)
	}
	if off.Enabled() {
		t.Fatal("пустой ключ включил шифрование")
	}
	if _, err := off.Seal("x"); !errors.Is(err, publications.ErrSecretsDisabled) {
		t.Fatalf("Seal без ключа: %v", err)
	}
}
