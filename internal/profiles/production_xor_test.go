package profiles

import (
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeProductionPatch(t *testing.T) {
	someID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	set := func(u uuid.UUID) OptionalUUID { return OptionalUUID{Present: true, Value: &u} }
	clear := func() OptionalUUID { return OptionalUUID{Present: true, Value: nil} }
	untouched := func() OptionalUUID { return OptionalUUID{} }
	boolPtr := func(b bool) *bool { return &b }

	type want struct {
		productionTouched bool
		productionCleared bool
		productionValue   string
		isFreelance       *bool
	}

	cases := []struct {
		name string
		in   PatchInput
		want want
	}{
		{
			name: "is_freelance=true wins over production_id set",
			in: PatchInput{
				ProductionID: set(someID),
				IsFreelance:  boolPtr(true),
			},
			want: want{
				productionTouched: true, productionCleared: true,
				isFreelance: boolPtr(true),
			},
		},
		{
			name: "production_id set forces is_freelance=false",
			in: PatchInput{
				ProductionID: set(someID),
			},
			want: want{
				productionTouched: true, productionCleared: false,
				productionValue: someID.String(),
				isFreelance:     boolPtr(false),
			},
		},
		{
			name: "production_id set + explicit is_freelance=false",
			in: PatchInput{
				ProductionID: set(someID),
				IsFreelance:  boolPtr(false),
			},
			want: want{
				productionTouched: true, productionCleared: false,
				productionValue: someID.String(),
				isFreelance:     boolPtr(false),
			},
		},
		{
			name: "production_id=clear, is_freelance=false — состояние «не выбрал»",
			in: PatchInput{
				ProductionID: clear(),
				IsFreelance:  boolPtr(false),
			},
			want: want{
				productionTouched: true, productionCleared: true,
				isFreelance: boolPtr(false),
			},
		},
		{
			name: "оба не заданы — оба не трогаются",
			in:   PatchInput{ProductionID: untouched(), IsFreelance: nil},
			want: want{productionTouched: false, isFreelance: nil},
		},
		{
			name: "только is_freelance=true — production_id чистится принудительно",
			in: PatchInput{
				ProductionID: untouched(),
				IsFreelance:  boolPtr(true),
			},
			want: want{
				productionTouched: true, productionCleared: true,
				isFreelance: boolPtr(true),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := normalizeProductionPatch(tc.in)

			if out.ProductionID.Present != tc.want.productionTouched {
				t.Fatalf("Present=%v, want %v", out.ProductionID.Present, tc.want.productionTouched)
			}
			if out.ProductionID.Present {
				gotCleared := out.ProductionID.Value == nil
				if gotCleared != tc.want.productionCleared {
					t.Fatalf("cleared=%v, want %v", gotCleared, tc.want.productionCleared)
				}
				if !gotCleared && out.ProductionID.Value.String() != tc.want.productionValue {
					t.Fatalf("value=%v, want %v", out.ProductionID.Value.String(), tc.want.productionValue)
				}
			}

			if (out.IsFreelance == nil) != (tc.want.isFreelance == nil) {
				t.Fatalf("isFreelance nil-state mismatch: got %v want %v",
					out.IsFreelance, tc.want.isFreelance)
			}
			if out.IsFreelance != nil && *out.IsFreelance != *tc.want.isFreelance {
				t.Fatalf("isFreelance=%v, want %v", *out.IsFreelance, *tc.want.isFreelance)
			}
		})
	}
}
