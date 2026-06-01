package projects

import (
	"testing"

	"github.com/google/uuid"
)

func mkStepView(status StepStatus, owner StepOwner, sortOrder int) StepView {
	return StepView{
		ID:        uuid.New(),
		Status:    status,
		Owner:     owner,
		SortOrder: sortOrder,
		Weight:    1,
	}
}

func TestDeriveProjectDisplayStatus(t *testing.T) {
	cases := []struct {
		name   string
		status ProjectStatus
		steps  []StepView
		want   ProjectDisplayStatus
	}{
		{
			name:   "all pending → not_started",
			status: StatusActive,
			steps: []StepView{
				mkStepView(StepPending, OwnerClient, 1),
				mkStepView(StepPending, OwnerTeam, 2),
				mkStepView(StepPending, OwnerTeam, 3),
			},
			want: DisplayStatusNotStarted,
		},
		{
			name:   "one in_progress → in_progress",
			status: StatusActive,
			steps: []StepView{
				mkStepView(StepInProgress, OwnerTeam, 1),
				mkStepView(StepPending, OwnerTeam, 2),
			},
			want: DisplayStatusInProgress,
		},
		{
			name:   "one done → in_progress (work has started)",
			status: StatusActive,
			steps: []StepView{
				mkStepView(StepDone, OwnerClient, 1),
				mkStepView(StepPending, OwnerTeam, 2),
			},
			want: DisplayStatusInProgress,
		},
		{
			name:   "waiting_client + owner=client → waiting_action",
			status: StatusActive,
			steps: []StepView{
				mkStepView(StepDone, OwnerClient, 1),
				mkStepView(StepWaitingClient, OwnerClient, 2),
			},
			want: DisplayStatusWaitingAction,
		},
		{
			name:   "waiting_action ПРИОРИТЕТНЕЕ in_progress",
			status: StatusActive,
			steps: []StepView{
				mkStepView(StepInProgress, OwnerTeam, 1),
				mkStepView(StepWaitingClient, OwnerClient, 2),
			},
			want: DisplayStatusWaitingAction,
		},
		{
			name:   "waiting_client с owner=team — НЕ waiting_action",
			status: StatusActive,
			steps: []StepView{
				mkStepView(StepWaitingClient, OwnerTeam, 1),
				mkStepView(StepPending, OwnerTeam, 2),
			},
			want: DisplayStatusInProgress, // waiting_client есть, но без client-owner
		},
		{
			name:   "project.done → completed",
			status: StatusDone,
			steps: []StepView{
				mkStepView(StepDone, OwnerTeam, 1),
			},
			want: DisplayStatusCompleted,
		},
		{
			name:   "project.cancelled → cancelled (даже если шаги in_progress)",
			status: StatusCancelled,
			steps: []StepView{
				mkStepView(StepInProgress, OwnerTeam, 1),
			},
			want: DisplayStatusCancelled,
		},
		{
			name:   "project.on_hold → on_hold",
			status: StatusOnHold,
			steps:  []StepView{mkStepView(StepInProgress, OwnerTeam, 1)},
			want:   DisplayStatusOnHold,
		},
		{
			name:   "project.dispute → dispute (отдельный статус)",
			status: StatusDispute,
			steps:  []StepView{mkStepView(StepInProgress, OwnerTeam, 1)},
			want:   DisplayStatusDispute,
		},
		{
			name:   "empty steps + active project → not_started",
			status: StatusActive,
			steps:  []StepView{},
			want:   DisplayStatusNotStarted,
		},
		{
			name:   "draft проект анализирует шаги (нет терминала)",
			status: StatusDraft,
			steps: []StepView{
				mkStepView(StepPending, OwnerClient, 1),
			},
			want: DisplayStatusNotStarted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveProjectDisplayStatus(tc.status, tc.steps)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeriveStageDisplayStatus(t *testing.T) {
	cases := []struct {
		name      string
		steps     []StepView
		wantStat  StageDisplayStatus
		wantDone  int
		wantTotal int
	}{
		{
			name:      "пустая стадия → not_started (фолбэк)",
			steps:     nil,
			wantStat:  StageNotStarted,
			wantDone:  0,
			wantTotal: 0,
		},
		{
			name: "3 done из 3 → completed",
			steps: []StepView{
				mkStepView(StepDone, OwnerTeam, 1),
				mkStepView(StepDone, OwnerClient, 2),
				mkStepView(StepDone, OwnerTeam, 3),
			},
			wantStat:  StageCompleted,
			wantDone:  3,
			wantTotal: 3,
		},
		{
			name: "1 done + 2 pending → active",
			steps: []StepView{
				mkStepView(StepDone, OwnerTeam, 1),
				mkStepView(StepPending, OwnerTeam, 2),
				mkStepView(StepPending, OwnerTeam, 3),
			},
			wantStat:  StageActive,
			wantDone:  1,
			wantTotal: 3,
		},
		{
			name: "0 done + 1 in_progress → active",
			steps: []StepView{
				mkStepView(StepInProgress, OwnerTeam, 1),
				mkStepView(StepPending, OwnerTeam, 2),
			},
			wantStat:  StageActive,
			wantDone:  0,
			wantTotal: 2,
		},
		{
			name: "0 done + всё pending → not_started",
			steps: []StepView{
				mkStepView(StepPending, OwnerTeam, 1),
				mkStepView(StepPending, OwnerClient, 2),
			},
			wantStat:  StageNotStarted,
			wantDone:  0,
			wantTotal: 2,
		},
		{
			name: "skipped считается completed",
			steps: []StepView{
				mkStepView(StepSkipped, OwnerTeam, 1),
				mkStepView(StepDone, OwnerTeam, 2),
			},
			wantStat:  StageCompleted,
			wantDone:  2,
			wantTotal: 2,
		},
		{
			name: "waiting_client → active",
			steps: []StepView{
				mkStepView(StepDone, OwnerTeam, 1),
				mkStepView(StepWaitingClient, OwnerClient, 2),
			},
			wantStat:  StageActive,
			wantDone:  1,
			wantTotal: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStat, gotDone, gotTotal := DeriveStageDisplayStatus(tc.steps)
			if gotStat != tc.wantStat {
				t.Errorf("status: got %q, want %q", gotStat, tc.wantStat)
			}
			if gotDone != tc.wantDone {
				t.Errorf("done: got %d, want %d", gotDone, tc.wantDone)
			}
			if gotTotal != tc.wantTotal {
				t.Errorf("total: got %d, want %d", gotTotal, tc.wantTotal)
			}
		})
	}
}

func TestDeriveCurrentStep(t *testing.T) {
	t.Run("waiting_client+client побеждает in_progress", func(t *testing.T) {
		steps := []StepView{
			mkStepView(StepInProgress, OwnerTeam, 1),
			mkStepView(StepWaitingClient, OwnerClient, 2),
		}
		got := DeriveCurrentStep(steps)
		if got == nil || got.SortOrder != 2 {
			t.Errorf("ожидал шаг #2 (waiting_client+client), получил %+v", got)
		}
	})

	t.Run("in_progress побеждает waiting_client+system", func(t *testing.T) {
		steps := []StepView{
			mkStepView(StepWaitingClient, OwnerSystem, 1),
			mkStepView(StepInProgress, OwnerTeam, 2),
		}
		got := DeriveCurrentStep(steps)
		if got == nil || got.SortOrder != 2 {
			t.Errorf("ожидал шаг #2 (in_progress), получил %+v", got)
		}
	})

	t.Run("waiting_client+team — фолбэк когда нет ничего лучше", func(t *testing.T) {
		steps := []StepView{
			mkStepView(StepDone, OwnerTeam, 1),
			mkStepView(StepWaitingClient, OwnerTeam, 2),
			mkStepView(StepPending, OwnerTeam, 3),
		}
		got := DeriveCurrentStep(steps)
		if got == nil || got.SortOrder != 2 {
			t.Errorf("ожидал шаг #2 (waiting_client owner=team), получил %+v", got)
		}
	})

	t.Run("первый pending когда ничего активного нет", func(t *testing.T) {
		steps := []StepView{
			mkStepView(StepDone, OwnerTeam, 1),
			mkStepView(StepPending, OwnerTeam, 2),
			mkStepView(StepPending, OwnerClient, 3),
		}
		got := DeriveCurrentStep(steps)
		if got == nil || got.SortOrder != 2 {
			t.Errorf("ожидал первый pending (#2), получил %+v", got)
		}
	})

	t.Run("nil при всех terminal", func(t *testing.T) {
		steps := []StepView{
			mkStepView(StepDone, OwnerTeam, 1),
			mkStepView(StepSkipped, OwnerTeam, 2),
		}
		if got := DeriveCurrentStep(steps); got != nil {
			t.Errorf("ожидал nil, получил %+v", got)
		}
	})
}
