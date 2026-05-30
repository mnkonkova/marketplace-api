package profiles_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"marketpclce/internal/profiles"
)

// PatchFullInput.ProductionID — tri-state через OptionalUUID. Тест фиксирует
// JSON-контракт для фронта (см. onProductionChange в кабинете):
//   - поле отсутствует     → Present=false (не трогать);
//   - поле = null          → Present=true, Value=nil (clear);
//   - поле = "<uuid>"      → Present=true, Value=&uuid (set).
func TestPatchFullInputProductionIDTriState(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	t.Run("missing → untouched", func(t *testing.T) {
		var in profiles.PatchFullInput
		if err := json.Unmarshal([]byte(`{}`), &in); err != nil {
			t.Fatal(err)
		}
		if in.ProductionID.Present {
			t.Fatalf("expected Present=false, got %+v", in.ProductionID)
		}
	})

	t.Run("null → clear", func(t *testing.T) {
		var in profiles.PatchFullInput
		if err := json.Unmarshal([]byte(`{"production_id":null}`), &in); err != nil {
			t.Fatal(err)
		}
		if !in.ProductionID.Present {
			t.Fatalf("expected Present=true for null, got %+v", in.ProductionID)
		}
		if in.ProductionID.Value != nil {
			t.Fatalf("expected Value=nil for null, got %v", in.ProductionID.Value)
		}
	})

	t.Run("uuid → set", func(t *testing.T) {
		var in profiles.PatchFullInput
		body := []byte(`{"production_id":"` + id.String() + `"}`)
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatal(err)
		}
		if !in.ProductionID.Present || in.ProductionID.Value == nil {
			t.Fatalf("expected both set, got %+v", in.ProductionID)
		}
		if *in.ProductionID.Value != id {
			t.Fatalf("expected %v, got %v", id, *in.ProductionID.Value)
		}
	})

	t.Run("garbage uuid → error", func(t *testing.T) {
		var in profiles.PatchFullInput
		err := json.Unmarshal([]byte(`{"production_id":"not-a-uuid"}`), &in)
		if err == nil {
			t.Fatal("expected error for invalid uuid")
		}
	})
}

func TestPatchFullInputIsFreelanceContract(t *testing.T) {
	t.Run("missing → nil", func(t *testing.T) {
		var in profiles.PatchFullInput
		_ = json.Unmarshal([]byte(`{}`), &in)
		if in.IsFreelance != nil {
			t.Fatalf("expected nil, got %v", *in.IsFreelance)
		}
	})
	t.Run("true → set true", func(t *testing.T) {
		var in profiles.PatchFullInput
		_ = json.Unmarshal([]byte(`{"is_freelance":true}`), &in)
		if in.IsFreelance == nil || *in.IsFreelance != true {
			t.Fatalf("expected true ptr, got %v", in.IsFreelance)
		}
	})
	t.Run("false → set false", func(t *testing.T) {
		var in profiles.PatchFullInput
		_ = json.Unmarshal([]byte(`{"is_freelance":false}`), &in)
		if in.IsFreelance == nil || *in.IsFreelance != false {
			t.Fatalf("expected false ptr, got %v", in.IsFreelance)
		}
	})
}
