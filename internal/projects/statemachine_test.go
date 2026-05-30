package projects

import "testing"

func TestIsValidStepTransition(t *testing.T) {
	allow := func(from, to StepStatus) {
		t.Helper()
		if !IsValidStepTransition(from, to) {
			t.Errorf("%s → %s should be ALLOWED", from, to)
		}
	}
	deny := func(from, to StepStatus) {
		t.Helper()
		if IsValidStepTransition(from, to) {
			t.Errorf("%s → %s should be DENIED", from, to)
		}
	}

	t.Run("разрешённые", func(t *testing.T) {
		allow(StepPending, StepInProgress)
		allow(StepInProgress, StepDone)
		allow(StepInProgress, StepWaitingClient)
		allow(StepInProgress, StepSkipped)
		allow(StepWaitingClient, StepDone)
		allow(StepWaitingClient, StepRejected)
		allow(StepWaitingClient, StepSkipped)
		allow(StepRejected, StepInProgress)
	})

	t.Run("запрещённые", func(t *testing.T) {
		deny(StepPending, StepDone)
		deny(StepPending, StepWaitingClient)
		deny(StepPending, StepRejected)
		deny(StepPending, StepSkipped)
		deny(StepDone, StepInProgress)
		deny(StepDone, StepRejected)
		deny(StepSkipped, StepInProgress)
		deny(StepWaitingClient, StepInProgress)
		deny(StepInProgress, StepRejected)
		deny(StepRejected, StepDone)
		deny(StepRejected, StepSkipped)
	})
}

func TestIsTerminalStep(t *testing.T) {
	cases := []struct {
		s    StepStatus
		want bool
	}{
		{StepDone, true},
		{StepSkipped, true},
		{StepPending, false},
		{StepInProgress, false},
		{StepWaitingClient, false},
		{StepRejected, false},
	}
	for _, c := range cases {
		if got := IsTerminalStep(c.s); got != c.want {
			t.Errorf("IsTerminalStep(%s) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestIsValidProjectStatusUpdate(t *testing.T) {
	allow := func(from, to ProjectStatus) {
		t.Helper()
		if !IsValidProjectStatusUpdate(from, to) {
			t.Errorf("%s → %s should be ALLOWED", from, to)
		}
	}
	deny := func(from, to ProjectStatus) {
		t.Helper()
		if IsValidProjectStatusUpdate(from, to) {
			t.Errorf("%s → %s should be DENIED", from, to)
		}
	}

	// Идемпотентность — каждый сам в себя.
	for _, s := range []ProjectStatus{StatusDraft, StatusActive, StatusOnHold, StatusDone, StatusDispute, StatusCancelled} {
		allow(s, s)
	}

	allow(StatusDraft, StatusActive)
	allow(StatusActive, StatusOnHold)
	allow(StatusOnHold, StatusActive)
	allow(StatusDispute, StatusActive) // resolution менеджером
	allow(StatusActive, StatusCancelled)

	deny(StatusDone, StatusActive)
	deny(StatusCancelled, StatusActive)
	deny(StatusDraft, StatusDispute) // dispute = автоматический результат revisions exhausted
	deny(StatusActive, StatusDone)   // done — автомат после последнего шага
}
