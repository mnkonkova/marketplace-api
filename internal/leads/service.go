package leads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput   = errors.New("invalid input")
	ErrNoSpecialists  = errors.New("no valid specialists in recipients")
)

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	in.ClientName = strings.TrimSpace(in.ClientName)
	in.ClientContact = strings.TrimSpace(in.ClientContact)
	in.Brief = strings.TrimSpace(in.Brief)
	in.TargetCategoryCode = strings.TrimSpace(in.TargetCategoryCode)

	if in.ClientName == "" {
		return uuid.Nil, fmt.Errorf("%w: client_name is required", ErrInvalidInput)
	}
	if in.ClientContact == "" {
		return uuid.Nil, fmt.Errorf("%w: client_contact is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Brief) < 10 {
		return uuid.Nil, fmt.Errorf("%w: brief must be at least 10 characters", ErrInvalidInput)
	}
	if in.BudgetMin != nil && *in.BudgetMin < 0 {
		return uuid.Nil, fmt.Errorf("%w: budget_min must be >= 0", ErrInvalidInput)
	}
	if in.BudgetMax != nil && *in.BudgetMax < 0 {
		return uuid.Nil, fmt.Errorf("%w: budget_max must be >= 0", ErrInvalidInput)
	}
	if in.BudgetMin != nil && in.BudgetMax != nil && *in.BudgetMin > *in.BudgetMax {
		return uuid.Nil, fmt.Errorf("%w: budget_min must be <= budget_max", ErrInvalidInput)
	}
	if in.Deadline != nil && in.Deadline.Before(time.Now().AddDate(0, 0, -1)) {
		return uuid.Nil, fmt.Errorf("%w: deadline cannot be in the past", ErrInvalidInput)
	}
	if len(in.SpecialistIDs) == 0 {
		return uuid.Nil, fmt.Errorf("%w: at least one specialist is required", ErrInvalidInput)
	}

	in.SpecialistIDs = dedupUUIDs(in.SpecialistIDs)

	valid, err := s.repo.ValidPublishedSpecialists(ctx, in.SpecialistIDs)
	if err != nil {
		return uuid.Nil, err
	}
	if len(valid) == 0 {
		return uuid.Nil, ErrNoSpecialists
	}
	in.SpecialistIDs = valid

	return s.repo.Create(ctx, in)
}

func (s *Service) ListIncoming(ctx context.Context, specialistID uuid.UUID, status string, limit, offset int) ([]IncomingLead, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	switch status {
	case "", RecipientStatusSent, RecipientStatusViewed, RecipientStatusAccepted, RecipientStatusDeclined:
	default:
		return nil, fmt.Errorf("%w: bad status filter", ErrInvalidInput)
	}
	return s.repo.ListIncoming(ctx, specialistID, status, limit, offset)
}

func (s *Service) UpdateRecipientStatus(ctx context.Context, leadID, specialistID uuid.UUID, status string) error {
	switch status {
	case RecipientStatusViewed, RecipientStatusAccepted, RecipientStatusDeclined:
	default:
		return fmt.Errorf("%w: status must be viewed, accepted or declined", ErrInvalidInput)
	}
	return s.repo.UpdateRecipientStatus(ctx, leadID, specialistID, status)
}

func dedupUUIDs(in []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(in))
	out := make([]uuid.UUID, 0, len(in))
	for _, id := range in {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
