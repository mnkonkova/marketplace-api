package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/admin"
	"marketpclce/internal/audit"
	"marketpclce/internal/auth"
	"marketpclce/tests/integration"
)

// Ссылка для входа сотруднику.
//
// Главное, что здесь проверяется, — не сама ссылка, а то, что её
// появление не открыло дыру рядом. Менеджерская ручка
// /manager/users/{id}/generate_invite обязана по-прежнему отказывать на
// админах: иначе менеджер выписывает токен на UUID админа, обменивает
// его и получает админский доступ (data-sec D1).

// Дыра D1 закрыта и должна оставаться закрытой: менеджер не может
// выписать magic-link на админа. Тест сторожит фикс от будущих правок —
// в том числе от соблазна «просто убрать фильтр» ради ссылки сотруднику.
func TestManagerCannotGenerateInviteForAdmin(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	victim, cleanupVictim := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	defer cleanupVictim()

	code, body := h.Do(t, http.MethodPost,
		"/api/v1/manager/users/"+victim.String()+"/generate_invite", h.Token(t, manager), nil)
	if code == http.StatusOK {
		t.Fatalf("менеджер выписал ссылку на админа — эскалация привилегий: %v", body)
	}
	if code != http.StatusNotFound {
		t.Errorf("код %d, ожидали 404 (тело %v)", code, body)
	}
	if n := invitesOf(t, pool, victim); n != 0 {
		t.Errorf("отказ оставил %d приглашений на админа", n)
	}

	// И на менеджере тоже: ручка менеджера — только для обычных людей.
	other, cleanupOther := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupOther()
	code, _ = h.Do(t, http.MethodPost,
		"/api/v1/manager/users/"+other.String()+"/generate_invite", h.Token(t, manager), nil)
	if code == http.StatusOK {
		t.Error("менеджер выписал ссылку другому менеджеру через свою ручку")
	}
	if n := invitesOf(t, pool, other); n != 0 {
		t.Errorf("отказ оставил %d приглашений", n)
	}
}

// Новая ручка живёт только в админской секции: под менеджером её нет.
func TestStaffLoginLinkRequiresAdmin(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)

	manager, cleanupManager := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupManager()
	target, cleanupTarget := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupTarget()

	code, body := h.Do(t, http.MethodPost,
		"/api/v1/admin/users/"+target.String()+"/login_link", h.Token(t, manager), nil)
	if code != http.StatusForbidden {
		t.Fatalf("менеджер на ссылке для входа: код %d, ожидали 403 (тело %v)", code, body)
	}
	if n := invitesOf(t, pool, target); n != 0 {
		t.Errorf("отказ оставил %d приглашений", n)
	}
}

// Админ выписывает ссылку менеджеру: обмен даёт доступ именно менеджера.
// Права берутся из базы в момент использования, сама ссылка никакой роли
// в себе не несёт — поэтому админской она человека не делает.
func TestStaffLoginLinkGivesTargetRights(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	manager := s.user(t, userOpts{Kind: "client", IsManager: true})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, manager.String())
		_, _ = pool.Exec(ctx, `DELETE FROM client_invites WHERE user_id = $1`, manager)
	})

	code, body := s.post(t, "/api/v1/admin/users/"+manager.String()+"/login_link", nil)
	if code != http.StatusOK {
		t.Fatalf("ссылка для входа: код %d, тело %v", code, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("в ответе нет токена: %v", body)
	}
	if url, _ := body["url"].(string); url == "" {
		t.Error("в ответе нет ссылки — админу нечего скопировать")
	}

	// Ссылка живёт 72 часа, а не общие семь дней инвайта: она пускает в
	// аккаунт сотрудника, и окно должно быть короче.
	expires, err := time.Parse(time.RFC3339, mustString(t, body, "expires_at"))
	if err != nil {
		t.Fatalf("разобрать expires_at: %v", err)
	}
	if delta := time.Until(expires) - 72*time.Hour; delta > time.Minute || delta < -time.Minute {
		t.Errorf("ссылка действует %s, ожидали 72 часа", time.Until(expires).Round(time.Minute))
	}

	// Обмен идёт мимо HTTP: публичная ручка redeem_invite стоит под
	// rate-limit'ом, а он без Redis отвечает 503. Токены, которые выдаёт
	// сервис, проверяем уже через роутер — там и живут права.
	issuer := auth.NewTokenIssuer("harness-secret", 15*time.Minute, 7*24*time.Hour)
	svc := admin.NewService(admin.NewRepo(pool), issuer, "http://harness.local", time.Hour)
	pair, userID, err := svc.RedeemInvite(ctx, token)
	if err != nil {
		t.Fatalf("обмен ссылки: %v", err)
	}
	if userID != manager {
		t.Fatalf("вошли под %s, а ссылка была для %s", userID, manager)
	}

	if code, body := s.h.Do(t, http.MethodGet,
		"/api/v1/manager/projects/inbox", pair.Access, nil); code != http.StatusOK {
		t.Errorf("вошедший по ссылке менеджер не попал в менеджерский кабинет: код %d, тело %v", code, body)
	}
	// Ссылку выписал админ — но админом она не делает.
	if code, _ := s.h.Do(t, http.MethodGet,
		"/api/v1/admin/summary", pair.Access, nil); code != http.StatusForbidden {
		t.Errorf("по ссылке для менеджера пустили в админку: код %d", code)
	}

	// Одноразовая: второй обмен того же токена не проходит.
	if _, _, err := svc.RedeemInvite(ctx, token); !errors.Is(err, admin.ErrInviteInvalid) {
		t.Errorf("повторный обмен: %v, ожидали ErrInviteInvalid (410)", err)
	}
}

// Новая ссылка гасит прошлую, а просроченная не работает. Иначе
// «одноразовая» означало бы «сколько выписали, столько и действует».
func TestStaffLoginLinkInvalidatesPreviousAndExpires(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	target := s.user(t, userOpts{Kind: "client", IsManager: true})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, target.String())
		_, _ = pool.Exec(ctx, `DELETE FROM client_invites WHERE user_id = $1`, target)
	})
	issuer := auth.NewTokenIssuer("harness-secret", 15*time.Minute, 7*24*time.Hour)
	svc := admin.NewService(admin.NewRepo(pool), issuer, "http://harness.local", time.Hour)

	_, first := s.post(t, "/api/v1/admin/users/"+target.String()+"/login_link", nil)
	firstToken := mustString(t, first, "token")
	_, second := s.post(t, "/api/v1/admin/users/"+target.String()+"/login_link", nil)
	secondToken := mustString(t, second, "token")

	if _, _, err := svc.RedeemInvite(ctx, firstToken); !errors.Is(err, admin.ErrInviteInvalid) {
		t.Errorf("прошлая ссылка всё ещё работает: %v", err)
	}

	// Просроченную не принимаем: состарить срок иначе нечем — ждать трое
	// суток тест не может.
	if _, err := pool.Exec(ctx,
		`UPDATE client_invites SET expires_at = now() - interval '1 hour'
		 WHERE user_id = $1 AND used_at IS NULL`, target); err != nil {
		t.Fatalf("состарить ссылку: %v", err)
	}
	if _, _, err := svc.RedeemInvite(ctx, secondToken); !errors.Is(err, admin.ErrInviteInvalid) {
		t.Errorf("просроченная ссылка сработала: %v", err)
	}
}

// Запись в журнале появляется вместе со ссылкой и не появляется без неё.
func TestStaffLoginLinkJournalFollowsTheAction(t *testing.T) {
	pool := integration.Pool(t)
	s := newAdminShell(t, pool)
	ctx := context.Background()

	target := s.user(t, userOpts{Kind: "client", IsManager: true})
	s.defer_(func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, target.String())
		_, _ = pool.Exec(ctx, `DELETE FROM client_invites WHERE user_id = $1`, target)
	})

	if code, body := s.post(t, "/api/v1/admin/users/"+target.String()+"/login_link", nil); code != http.StatusOK {
		t.Fatalf("ссылка: код %d, тело %v", code, body)
	}
	if n := auditCountAction(t, pool, audit.ActionUserLoginLink, target.String()); n != 1 {
		t.Fatalf("записей о выдаче ссылки %d, ожидали 1", n)
	}
	// Токена в журнале быть не должно: по журналу нельзя входить.
	var payload string
	if err := pool.QueryRow(ctx, `
SELECT payload::text FROM admin_audit_log
WHERE action = $1 AND object_id = $2`, audit.ActionUserLoginLink, target.String()).Scan(&payload); err != nil {
		t.Fatalf("payload записи: %v", err)
	}
	if len(payload) > 200 {
		t.Errorf("в журнале слишком много данных о ссылке: %s", payload)
	}

	// Отключённому аккаунту ссылку не выписываем — и следа не остаётся.
	if code, _ := s.post(t, "/api/v1/admin/users/"+target.String()+"/deactivate", nil); code != http.StatusNoContent {
		t.Fatal("не удалось отключить аккаунт")
	}
	code, body := s.post(t, "/api/v1/admin/users/"+target.String()+"/login_link", nil)
	if code != http.StatusConflict {
		t.Fatalf("ссылка отключённому: код %d, ожидали 409 (тело %v)", code, body)
	}
	if n := auditCountAction(t, pool, audit.ActionUserLoginLink, target.String()); n != 1 {
		t.Errorf("отказ добавил запись в журнал (всего %d)", n)
	}
	if n := invitesOf(t, pool, target); n != 1 {
		t.Errorf("отказ выписал ещё одну ссылку (всего %d)", n)
	}
}

// Ссылку выписывает только админ. Маршрут это уже проверил, но роль
// могли снять после выдачи токена, а токен живёт ещё минуты — поэтому
// репозиторий перечитывает права в своей транзакции.
func TestStaffLoginLinkRechecksActorRole(t *testing.T) {
	pool := integration.Pool(t)
	h := newAPIHarness(t, pool)
	ctx := context.Background()

	actor, cleanupActor := h.NewUser(t, userOpts{Kind: "client", IsAdmin: true})
	defer cleanupActor()
	target, cleanupTarget := h.NewUser(t, userOpts{Kind: "client", IsManager: true})
	defer cleanupTarget()
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM client_invites WHERE user_id = $1`, target)
		_, _ = pool.Exec(ctx,
			`DELETE FROM admin_audit_log WHERE object_type = 'user' AND object_id = $1`, target.String())
	}()

	if _, _, err := admin.NewRepo(pool).GenerateStaffLoginLink(ctx, target, actor); err != nil {
		t.Fatalf("админ не смог выписать ссылку: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_admin = FALSE WHERE id = $1`, actor); err != nil {
		t.Fatalf("снять роль: %v", err)
	}
	_, _, err := admin.NewRepo(pool).GenerateStaffLoginLink(ctx, target, actor)
	if !errors.Is(err, admin.ErrForbiddenActor) {
		t.Fatalf("бывший админ выписал ссылку: %v", err)
	}
	// Первая ссылка осталась единственной: отказ ничего не добавил и
	// ничего не погасил.
	if n := invitesOf(t, pool, target); n != 1 {
		t.Errorf("приглашений у цели %d, ожидали 1", n)
	}
	if n := auditCountAction(t, pool, audit.ActionUserLoginLink, target.String()); n != 1 {
		t.Errorf("записей в журнале %d, ожидали 1", n)
	}
}

func invitesOf(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM client_invites WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("считать приглашения: %v", err)
	}
	return n
}

func mustString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok || v == "" {
		t.Fatalf("в ответе нет строки %q: %v", key, m)
	}
	return v
}
