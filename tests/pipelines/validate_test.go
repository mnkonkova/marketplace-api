package pipelines_test

import (
	"errors"
	"testing"

	"marketpclce/internal/pipelines"
)

func TestValidateOwner(t *testing.T) {
	for _, owner := range []string{
		pipelines.OwnerClient,
		pipelines.OwnerTeam,
		pipelines.OwnerSystem,
	} {
		if err := pipelines.ValidateOwner(owner); err != nil {
			t.Errorf("owner %q must be valid: %v", owner, err)
		}
	}
	for _, owner := range []string{"", "manager", "Client", "TEAM"} {
		err := pipelines.ValidateOwner(owner)
		if err == nil {
			t.Errorf("owner %q must be invalid", owner)
		} else if !errors.Is(err, pipelines.ErrInvalidInput) {
			t.Errorf("owner %q: must wrap ErrInvalidInput, got %v", owner, err)
		}
	}
}

func TestValidateStep(t *testing.T) {
	cases := []struct {
		name      string
		stepName  string
		owner     string
		duration  int
		weight    int
		sortOrder int
		wantErr   bool
	}{
		{"ok minimal", "Brief", pipelines.OwnerClient, 1, 1, 0, false},
		{"empty name", "", pipelines.OwnerTeam, 1, 1, 0, true},
		{"bad owner", "Brief", "boss", 1, 1, 0, true},
		{"zero duration", "Brief", pipelines.OwnerTeam, 0, 1, 0, true},
		{"negative duration", "Brief", pipelines.OwnerTeam, -1, 1, 0, true},
		{"zero weight", "Brief", pipelines.OwnerTeam, 1, 0, 0, true},
		{"negative sort_order", "Brief", pipelines.OwnerTeam, 1, 1, -1, true},
		{"big numbers ok", "Edit", pipelines.OwnerTeam, 90, 100, 1000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pipelines.ValidateStep(tc.stepName, tc.owner, tc.duration, tc.weight, tc.sortOrder)
			if tc.wantErr && err == nil {
				t.Fatalf("want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr && !errors.Is(err, pipelines.ErrInvalidInput) {
				t.Fatalf("error must wrap ErrInvalidInput, got %v", err)
			}
		})
	}
}
