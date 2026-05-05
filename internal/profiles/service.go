package profiles

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

var (
	ErrInvalidInput     = errors.New("invalid input")
	ErrProfileRejected  = errors.New("profile rejected")
	ErrPublishIncomplete = errors.New("publish incomplete")
)

type ProfileChecker interface {
	Available() bool
	Check(ctx context.Context, in CheckInput) (CheckResult, error)
}

type CheckInput struct {
	Bio                  string
	DisplayName          string
	PrimaryCategory      string
	PrimaryCategoryTitle string
}

type PartResult struct {
	OK         bool     `json:"ok"`
	Score      int      `json:"score"`
	Reasons    []string `json:"reasons"`
	Suggestion string   `json:"suggestion"`
}

type CheckResult struct {
	OK   bool       `json:"ok"`
	Bio  PartResult `json:"bio"`
	Name PartResult `json:"name"`
}

type Service struct {
	repo    *Repo
	checker ProfileChecker
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) WithProfileChecker(c ProfileChecker) *Service {
	s.checker = c
	return s
}

func (s *Service) Get(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return s.repo.Get(ctx, userID)
}

func (s *Service) GetPublic(ctx context.Context, userID uuid.UUID) (PublicProfile, error) {
	return s.repo.GetPublic(ctx, userID)
}

func (s *Service) Patch(ctx context.Context, userID uuid.UUID, in PatchInput) (Profile, error) {
	if in.DisplayName != nil {
		v := strings.TrimSpace(*in.DisplayName)
		if v == "" {
			return Profile{}, fmt.Errorf("%w: display_name cannot be empty", ErrInvalidInput)
		}
		in.DisplayName = &v
	}
	if in.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Currency))
		if len(c) != 3 {
			return Profile{}, fmt.Errorf("%w: currency must be 3-letter code", ErrInvalidInput)
		}
		in.Currency = &c
	}
	if in.RateMin != nil && *in.RateMin < 0 {
		return Profile{}, fmt.Errorf("%w: rate_min must be >= 0", ErrInvalidInput)
	}
	if in.RateMax != nil && *in.RateMax < 0 {
		return Profile{}, fmt.Errorf("%w: rate_max must be >= 0", ErrInvalidInput)
	}
	if in.RateMin != nil && in.RateMax != nil && *in.RateMin > *in.RateMax {
		return Profile{}, fmt.Errorf("%w: rate_min must be <= rate_max", ErrInvalidInput)
	}

	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.PatchInTx(ctx, tx, userID, in); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]string{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

func (s *Service) SetCategories(ctx context.Context, userID uuid.UUID, in SetCategoriesInput) (Profile, error) {
	codes := dedupStrings(in.Codes)
	if len(codes) == 0 {
		return Profile{}, fmt.Errorf("%w: at least one category is required", ErrInvalidInput)
	}
	if in.Primary == "" {
		return Profile{}, fmt.Errorf("%w: primary category is required", ErrInvalidInput)
	}
	if !contains(codes, in.Primary) {
		return Profile{}, fmt.Errorf("%w: primary must be in codes", ErrInvalidInput)
	}

	valid, err := s.repo.ValidCategoryCodes(ctx, codes)
	if err != nil {
		return Profile{}, err
	}
	if len(valid) != len(codes) {
		return Profile{}, fmt.Errorf("%w: unknown category code", ErrInvalidInput)
	}

	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.ReplaceCategoriesInTx(ctx, tx, userID, codes, in.Primary); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]any{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

func (s *Service) SetSkills(ctx context.Context, userID uuid.UUID, in SetSkillsInput) (Profile, error) {
	ids := make([]uuid.UUID, 0, len(in.SkillIDs))
	seen := map[uuid.UUID]struct{}{}
	for _, raw := range in.SkillIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return Profile{}, fmt.Errorf("%w: bad skill id %q", ErrInvalidInput, raw)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	valid, err := s.repo.ValidSkillIDs(ctx, ids)
	if err != nil {
		return Profile{}, err
	}
	if len(valid) != len(ids) {
		return Profile{}, fmt.Errorf("%w: unknown skill id", ErrInvalidInput)
	}

	err = s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.ReplaceSkillsInTx(ctx, tx, userID, ids); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			outbox.EventSpecialistUpserted, map[string]any{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

func (s *Service) SetPublished(ctx context.Context, userID uuid.UUID, published bool) (Profile, error) {
	event := outbox.EventSpecialistPublished
	if !published {
		event = outbox.EventSpecialistRetracted
	}
	if published && s.checker != nil && s.checker.Available() {
		p, err := s.repo.Get(ctx, userID)
		if err != nil {
			return Profile{}, err
		}
		bio := strings.TrimSpace(p.Bio)
		name := strings.TrimSpace(p.DisplayName)
		if bio == "" {
			return Profile{}, fmt.Errorf("%w: bio is empty", ErrPublishIncomplete)
		}
		if name == "" {
			return Profile{}, fmt.Errorf("%w: display_name is empty", ErrPublishIncomplete)
		}
		title, _ := s.repo.CategoryTitle(ctx, p.PrimaryCategory)
		res, err := s.checker.Check(ctx, CheckInput{
			Bio:                  bio,
			DisplayName:          name,
			PrimaryCategory:      p.PrimaryCategory,
			PrimaryCategoryTitle: title,
		})
		if err != nil {
			return Profile{}, fmt.Errorf("profile check: %w", err)
		}
		if !res.OK {
			return Profile{}, &ProfileRejectedError{Result: res}
		}
	}
	err := s.repo.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.repo.SetPublishedInTx(ctx, tx, userID, published); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, outbox.AggregateSpecialist, userID.String(),
			event, map[string]any{"user_id": userID.String()})
	})
	if err != nil {
		return Profile{}, err
	}
	return s.repo.Get(ctx, userID)
}

type ProfileRejectedError struct {
	Result CheckResult
}

func (e *ProfileRejectedError) Error() string { return "profile rejected by llm check" }
func (e *ProfileRejectedError) Is(target error) bool {
	return target == ErrProfileRejected
}

func dedupStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
