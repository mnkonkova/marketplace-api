package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/productions"
	"marketpclce/internal/profiles"
	"marketpclce/tests/integration"
)

// ---- ТЕСТ: NormalizeProduction с реальным ProductionLookup (через repo) ----

func TestNormalizeProductionWithRealLookup(t *testing.T) {
	pool := integration.Pool(t)
	ctx := context.Background()

	// Создаём один активный и один деактивированный продакшен.
	prodRepo := productions.NewRepo(pool)
	prodSvc := productions.NewService(prodRepo)

	active, err := prodSvc.Create(ctx, productions.CreateInput{Name: "IT-Active-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM productions WHERE id = $1`, active.ID)

	inactive, err := prodSvc.Create(ctx, productions.CreateInput{Name: "IT-Inactive-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("create inactive: %v", err)
	}
	if _, err := prodSvc.Delete(ctx, inactive.ID), error(nil); err != nil {
		// Delete возвращает nil или error
	}
	_ = prodSvc.Delete(ctx, inactive.ID)
	defer pool.Exec(ctx, `DELETE FROM productions WHERE id = $1`, inactive.ID)

	// Pick активный — норм.
	idStr := active.ID.String()
	got, err := profiles.NormalizeProduction(ctx, &idStr, nil, prodSvc)
	if err != nil {
		t.Fatalf("pick active: %v", err)
	}
	if !got.SetProduction || got.ProductionID == nil || *got.ProductionID != active.ID {
		t.Errorf("active production не выставился: %+v", got)
	}
	if !got.SetIsFreelance || got.IsFreelance {
		t.Errorf("freelance не сброшен в false при выборе production")
	}

	// Pick неактивный — ошибка.
	inactiveStr := inactive.ID.String()
	_, err = profiles.NormalizeProduction(ctx, &inactiveStr, nil, prodSvc)
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Errorf("inactive production: want ErrInvalidInput, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "inactive") && !strings.Contains(err.Error(), "not found") {
		t.Logf("err text: %v", err)
	}
}

// ---- ТЕСТ: NormalizeProduction — фриланс автоматом сбрасывает production ----

func TestNormalizeProductionFreelanceClears(t *testing.T) {
	freelance := true
	got, err := profiles.NormalizeProduction(context.Background(), nil, &freelance, nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !got.SetIsFreelance || !got.IsFreelance {
		t.Errorf("freelance не выставился")
	}
	if !got.SetProduction || got.ProductionID != nil {
		t.Errorf("production не сброшен в NULL при is_freelance=true")
	}
}

// ---- ТЕСТ: NormalizeProduction — конфликт (production + freelance=true) ----

func TestNormalizeProductionConflict(t *testing.T) {
	prodID := uuid.NewString()
	freelance := true
	_, err := profiles.NormalizeProduction(context.Background(), &prodID, &freelance, nil)
	if !errors.Is(err, profiles.ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput on production+freelance=true, got %v", err)
	}
}
