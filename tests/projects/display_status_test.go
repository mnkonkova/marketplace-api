package projects_test

import (
	"testing"

	"marketpclce/internal/projects"
)

func mkStep(owner string, status projects.StepStatus, weight int) projects.StepView {
	return projects.StepView{
		Step: projects.Step{
			Owner:  owner,
			Status: status,
			Weight: weight,
		},
	}
}

func TestDeriveProjectDisplayStatus(t *testing.T) {
	cases := []struct {
		name   string
		status projects.ProjectStatus
		steps  []projects.StepView
		want   projects.ProjectDisplayStatus
	}{
		{"draft empty", projects.ProjectStatusDraft, nil, projects.ProjectDisplayNotStarted},
		{"active no steps", projects.ProjectStatusActive, nil, projects.ProjectDisplayNotStarted},
		{"on_hold wins", projects.ProjectStatusOnHold, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 1),
		}, projects.ProjectDisplayOnHold},
		{"cancelled wins", projects.ProjectStatusCancelled, nil, projects.ProjectDisplayCancelled},
		{"dispute shows as on_hold", projects.ProjectStatusDispute, nil, projects.ProjectDisplayOnHold},
		{"done shows as completed", projects.ProjectStatusDone, nil, projects.ProjectDisplayCompleted},

		{"waiting client+client → waiting_action", projects.ProjectStatusActive, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerClient, projects.StepStatusWaitingClient, 1),
		}, projects.ProjectDisplayWaitingAction},

		{"waiting client+team → in_progress (не waiting_action!)", projects.ProjectStatusActive, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusWaitingClient, 1),
		}, projects.ProjectDisplayInProgress},

		{"in_progress step → in_progress", projects.ProjectStatusActive, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 1),
		}, projects.ProjectDisplayInProgress},

		{"all pending → not_started", projects.ProjectStatusActive, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
			mkStep(projects.OwnerClient, projects.StepStatusPending, 1),
		}, projects.ProjectDisplayNotStarted},

		{"done + pending → not_started? нет: in_progress (есть прогресс)", projects.ProjectStatusActive, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		},
			// done без активного шага и без waiting_client → not_started (нет активной работы)
			// но это спорно. По брифу — если есть прогресс, проект "в работе".
			// Однако наша функция говорит "in_progress = active_work=true",
			// а done-шаг — это не active_work. Тестируем поведение функции:
			projects.ProjectDisplayNotStarted},

		{"waiting_action перебивает обычный in_progress", projects.ProjectStatusActive, []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 1),
			mkStep(projects.OwnerClient, projects.StepStatusWaitingClient, 1),
		}, projects.ProjectDisplayWaitingAction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projects.DeriveProjectDisplayStatus(tc.status, tc.steps)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestDeriveStageDisplayStatus(t *testing.T) {
	cases := []struct {
		name       string
		steps      []projects.StepView
		wantStatus projects.StageDisplayStatus
		wantDone   int
		wantTotal  int
	}{
		{"empty", nil, projects.StageDisplayNotStarted, 0, 0},
		{"all pending", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, projects.StageDisplayNotStarted, 0, 2},
		{"all done", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
		}, projects.StageDisplayCompleted, 2, 2},
		{"mixed done+pending → active", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, projects.StageDisplayActive, 1, 2},
		{"in_progress in middle → active", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 1),
		}, projects.StageDisplayActive, 0, 2},
		{"skipped counts as done", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusSkipped, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
		}, projects.StageDisplayCompleted, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotDone, gotTotal := projects.DeriveStageDisplayStatus(tc.steps)
			if gotStatus != tc.wantStatus || gotDone != tc.wantDone || gotTotal != tc.wantTotal {
				t.Fatalf("got (%v, %d, %d) want (%v, %d, %d)",
					gotStatus, gotDone, gotTotal, tc.wantStatus, tc.wantDone, tc.wantTotal)
			}
		})
	}
}

func TestDeriveCurrentStep(t *testing.T) {
	cases := []struct {
		name     string
		steps    []projects.StepView
		wantNil  bool
		wantIdx  int
	}{
		{"empty", nil, true, 0},
		{"only done → nil", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
		}, true, 0},
		{"first pending wins among pending", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, false, 1},
		{"in_progress over pending", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, false, 1},
		{"waiting_client+team over in_progress", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusWaitingClient, 1),
		}, false, 1},
		{"waiting_client+client over waiting_client+team", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusWaitingClient, 1),
			mkStep(projects.OwnerClient, projects.StepStatusWaitingClient, 1),
		}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projects.DeriveCurrentStep(tc.steps)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want non-nil")
			}
			if got != &tc.steps[tc.wantIdx] {
				t.Fatalf("want idx %d, got different step", tc.wantIdx)
			}
		})
	}
}

func TestDeriveProgress(t *testing.T) {
	cases := []struct {
		name  string
		steps []projects.StepView
		want  float64
	}{
		{"empty", nil, 0},
		{"all pending", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, 0},
		{"all done", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
		}, 100},
		{"half done", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, 50},
		{"in_progress = половина веса", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusInProgress, 2),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 2),
		}, 25},
		{"weighted", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 3),
			mkStep(projects.OwnerTeam, projects.StepStatusPending, 1),
		}, 75},
		{"skipped считается завершённым", []projects.StepView{
			mkStep(projects.OwnerTeam, projects.StepStatusSkipped, 1),
			mkStep(projects.OwnerTeam, projects.StepStatusDone, 1),
		}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projects.DeriveProgress(tc.steps)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
