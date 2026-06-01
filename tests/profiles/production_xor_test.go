package profiles_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/profiles"
)

// stubLookup — реализует ProductionLookup. activeIDs — множество активных.
type stubLookup struct {
	activeIDs map[uuid.UUID]struct{}
	err       error
}

func (s stubLookup) IsActiveProduction(_ context.Context, id uuid.UUID) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	_, ok := s.activeIDs[id]
	return ok, nil
}

// TestNormalizeProductionFreelance проверяет XOR-нормализацию: продакшен и
// фриланс — взаимоисключающие. Тест дёргает чистый помощник через
// сервис-обёртку, без БД (PatchFull частично работает на чистой логике до
// первого вызова репо).
//
// Используем экспортированный NormalizeProduction() (см. service.go).
func TestNormalizeProduction(t *testing.T) {
	active := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	inactive := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	lookup := stubLookup{activeIDs: map[uuid.UUID]struct{}{active: {}}}

	tr := func(s string) *string { return &s }
	bo := func(b bool) *bool { return &b }

	cases := []struct {
		name         string
		productionID *string
		isFreelance  *bool
		wantErr      bool
		wantSetProd  bool
		wantProdID   *uuid.UUID
		wantSetFree  bool
		wantFree     bool
	}{
		{
			name:         "pick production clears freelance",
			productionID: tr(active.String()),
			wantSetProd:  true, wantProdID: &active,
			wantSetFree: true, wantFree: false,
		},
		{
			name:        "go freelance clears production",
			isFreelance: bo(true),
			wantSetProd: true, wantProdID: nil,
			wantSetFree: true, wantFree: true,
		},
		{
			name:         "clear via empty production_id",
			productionID: tr(""),
			wantSetProd:  true, wantProdID: nil,
			wantSetFree: false, wantFree: false,
		},
		{
			name:         "conflict: production + freelance=true",
			productionID: tr(active.String()),
			isFreelance:  bo(true),
			wantErr:      true,
		},
		{
			name:         "inactive production rejected",
			productionID: tr(inactive.String()),
			wantErr:      true,
		},
		{
			name:         "bad uuid rejected",
			productionID: tr("not-a-uuid"),
			wantErr:      true,
		},
		{
			name:        "freelance=false alone, no production",
			isFreelance: bo(false),
			wantSetProd: false, wantProdID: nil,
			wantSetFree: true, wantFree: false,
		},
		{
			name:         "production + explicit freelance=false (ok, both consistent)",
			productionID: tr(active.String()),
			isFreelance:  bo(false),
			wantSetProd:  true, wantProdID: &active,
			wantSetFree: true, wantFree: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := profiles.NormalizeProduction(context.Background(), tc.productionID, tc.isFreelance, lookup)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error")
				}
				if !errors.Is(err, profiles.ErrInvalidInput) {
					t.Fatalf("want ErrInvalidInput, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.SetProduction != tc.wantSetProd {
				t.Errorf("SetProduction: got %v, want %v", got.SetProduction, tc.wantSetProd)
			}
			if (got.ProductionID == nil) != (tc.wantProdID == nil) {
				t.Errorf("ProductionID nil mismatch: got %v want %v", got.ProductionID, tc.wantProdID)
			}
			if got.ProductionID != nil && tc.wantProdID != nil && *got.ProductionID != *tc.wantProdID {
				t.Errorf("ProductionID: got %v want %v", *got.ProductionID, *tc.wantProdID)
			}
			if got.SetIsFreelance != tc.wantSetFree {
				t.Errorf("SetIsFreelance: got %v want %v", got.SetIsFreelance, tc.wantSetFree)
			}
			if got.IsFreelance != tc.wantFree {
				t.Errorf("IsFreelance: got %v want %v", got.IsFreelance, tc.wantFree)
			}
		})
	}
}
