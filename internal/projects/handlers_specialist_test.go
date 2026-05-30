package projects

import (
	"context"
	"testing"
	"time"
)

// TestSpecialistIsolation: спец A видит только свой назначенный проект;
// чужой → ErrNotFound. В funnel специалиста скрыты payment, escrow_hold,
// social_setup, revision_round, nps, review (visible_to_specialist=false).
func TestSpecialistIsolation(t *testing.T) {
	pool := integrationDB(t)
	defer pool.Close()
	repo := NewRepo(pool)
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mkClient(t, ctx, repo, "iso-client")
	specA := mkSpecialist(t, ctx, repo, "iso-specA")
	specB := mkSpecialist(t, ctx, repo, "iso-specB")
	actor := mkClient(t, ctx, repo, "iso-actor")

	// Проект для specA
	in := CreateManualInput{
		ClientUserID:     client,
		SpecialistUserID: &specA,
		TemplateCode:     "video_production",
		TemplateVersion:  1,
		Title:            "Spec-isolation test",
		Source:           SourceManual,
	}
	created, err := svc.StartProjectManual(ctx, in, actor)
	if err != nil {
		t.Fatalf("StartProjectManual: %v", err)
	}

	// specB не видит проект specA → ErrNotFound
	if _, err := svc.GetSpecialistProject(ctx, created.ID, specB); err == nil || err.Error() != ErrNotFound.Error() {
		t.Errorf("expected ErrNotFound for foreign specialist, got %v", err)
	}
	if _, err := svc.GetSpecialistFunnel(ctx, created.ID, specB); err == nil || err.Error() != ErrNotFound.Error() {
		t.Errorf("expected ErrNotFound on funnel for foreign specialist, got %v", err)
	}

	// specA видит → проверяем что funnel содержит ровно 11 видимых шагов
	// (17 - 6 скрытых от спеца: payment, escrow_hold, social_setup,
	// revision_round, nps, review).
	f, err := svc.GetSpecialistFunnel(ctx, created.ID, specA)
	if err != nil {
		t.Fatalf("GetSpecialistFunnel: %v", err)
	}
	hidden := map[string]bool{
		"payment": true, "escrow_hold": true, "social_setup": true,
		"revision_round": true, "nps": true, "review": true,
	}
	total := 0
	for _, st := range f.Stages {
		for _, s := range st.Steps {
			if hidden[s.Code] {
				t.Errorf("step %s must be hidden from specialist", s.Code)
			}
			total++
		}
	}
	if total != 11 {
		t.Errorf("specialist-visible steps = %d, want 11", total)
	}

	// ListSpecialist: specA видит свой проект, specB пусто.
	specAList, err := svc.ListSpecialistProjects(ctx, specA)
	if err != nil {
		t.Fatalf("ListSpecialistProjects(A): %v", err)
	}
	foundA := false
	for _, p := range specAList {
		if p.ID == created.ID {
			foundA = true
			break
		}
	}
	if !foundA {
		t.Error("specA должен видеть свой проект в списке")
	}

	specBList, _ := svc.ListSpecialistProjects(ctx, specB)
	for _, p := range specBList {
		if p.ID == created.ID {
			t.Error("specB не должен видеть проект specA")
		}
	}
}
