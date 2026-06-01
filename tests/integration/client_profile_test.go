package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: GetClientProfile для несуществующего юзера → пустой профиль ----

func TestGetClientProfileEmpty(t *testing.T) {
	pool := integration.Pool(t)
	repo := profiles.NewRepo(pool)
	cp, err := repo.GetClientProfile(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cp.DisplayName != "" || cp.Phone != "" || cp.Telegram != "" {
		t.Errorf("ожидался пустой профиль, получен %+v", cp)
	}
}

// ---- ТЕСТ: PatchClientProfile upsert + частичный апдейт ----

func TestPatchClientProfileUpsert(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	var uid uuid.UUID
	_ = pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, role, is_approved)
VALUES ($1, 'x', 'client', 'client', TRUE) RETURNING id`,
		"cp-"+uuid.NewString()+"@x").Scan(&uid)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)

	repo := profiles.NewRepo(pool)

	name := "Анна Петрова"
	phone := "+79991234567"
	tg := "@anna"

	cp, err := repo.PatchClientProfile(ctx, uid, profiles.ClientProfilePatch{
		DisplayName: &name,
		Phone:       &phone,
		Telegram:    &tg,
	})
	if err != nil {
		t.Fatalf("patch upsert: %v", err)
	}
	if cp.DisplayName != name || cp.Phone != phone || cp.Telegram != tg {
		t.Errorf("после upsert: %+v", cp)
	}

	// Частичный апдейт — только phone.
	newPhone := "+79990000000"
	cp2, err := repo.PatchClientProfile(ctx, uid, profiles.ClientProfilePatch{
		Phone: &newPhone,
	})
	if err != nil {
		t.Fatalf("patch partial: %v", err)
	}
	if cp2.Phone != newPhone {
		t.Errorf("phone не обновился: %s", cp2.Phone)
	}
	// display_name и telegram остались
	if cp2.DisplayName != name || cp2.Telegram != tg {
		t.Errorf("частичный апдейт затронул другие поля: %+v", cp2)
	}
}

// ---- ТЕСТ: ValidateClientPatch отвергает overflow ----

func TestValidateClientPatchTooLong(t *testing.T) {
	long := string(make([]byte, 200))
	for i := range long {
		long = long[:i] + "a" + long[i+1:]
	}
	if err := profiles.ValidateClientPatch(profiles.ClientProfilePatch{DisplayName: &long}); err == nil {
		t.Errorf("name 200 chars must fail")
	}
}
