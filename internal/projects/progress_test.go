package projects

import (
	"testing"

	"github.com/google/uuid"
)

func mkStep(weight int, status StepStatus, sortOrder int) StepView {
	return StepView{
		ID:        uuid.New(),
		Status:    status,
		Weight:    weight,
		SortOrder: sortOrder,
	}
}

func TestCalcProgress(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		p := CalcProgress(nil)
		if p.Percent != 0 || p.TotalWeight != 0 || p.CurrentStepID != nil {
			t.Errorf("expected zero, got %+v", p)
		}
	})

	t.Run("all pending → 0%", func(t *testing.T) {
		steps := []StepView{
			mkStep(1, StepPending, 1),
			mkStep(2, StepPending, 2),
		}
		p := CalcProgress(steps)
		if p.Percent != 0 {
			t.Errorf("percent=%d, want 0", p.Percent)
		}
		if p.TotalWeight != 3 {
			t.Errorf("total=%d, want 3", p.TotalWeight)
		}
		if p.CurrentStepID == nil || *p.CurrentStepID != steps[0].ID.String() {
			t.Errorf("current step mismatch: %v", p.CurrentStepID)
		}
	})

	t.Run("all done → 100%", func(t *testing.T) {
		p := CalcProgress([]StepView{
			mkStep(1, StepDone, 1),
			mkStep(2, StepDone, 2),
		})
		if p.Percent != 100 {
			t.Errorf("percent=%d, want 100", p.Percent)
		}
		if p.CurrentStepID != nil {
			t.Errorf("expected no current step, got %v", p.CurrentStepID)
		}
	})

	t.Run("skipped считается completed", func(t *testing.T) {
		// 5 weight skipped + 5 done + 0 pending → 100%
		p := CalcProgress([]StepView{
			mkStep(5, StepDone, 1),
			mkStep(5, StepSkipped, 2),
		})
		if p.Percent != 100 {
			t.Errorf("percent=%d, want 100 (skipped считается completed)", p.Percent)
		}
	})

	t.Run("half done", func(t *testing.T) {
		p := CalcProgress([]StepView{
			mkStep(5, StepDone, 1),
			mkStep(5, StepInProgress, 2),
		})
		if p.Percent != 50 {
			t.Errorf("percent=%d, want 50", p.Percent)
		}
	})

	t.Run("waiting_client не considered done", func(t *testing.T) {
		steps := []StepView{
			mkStep(1, StepDone, 1),
			mkStep(1, StepWaitingClient, 2),
			mkStep(1, StepPending, 3),
		}
		p := CalcProgress(steps)
		if p.Percent != 33 {
			t.Errorf("percent=%d, want 33 (1 из 3)", p.Percent)
		}
		// current step — waiting_client (первый не-done/skipped)
		if p.CurrentStepID == nil || *p.CurrentStepID != steps[1].ID.String() {
			t.Errorf("current step должен быть waiting_client")
		}
	})

	t.Run("rejected не считается completed", func(t *testing.T) {
		steps := []StepView{
			mkStep(2, StepDone, 1),
			mkStep(1, StepRejected, 2),
		}
		p := CalcProgress(steps)
		// 2 из 3 = 67% (округление до ближайшего)
		if p.Percent != 67 {
			t.Errorf("percent=%d, want 67", p.Percent)
		}
		if p.CurrentStepID == nil || *p.CurrentStepID != steps[1].ID.String() {
			t.Errorf("current должен быть rejected")
		}
	})
}
