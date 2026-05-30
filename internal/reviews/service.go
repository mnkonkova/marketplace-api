package reviews

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidInput = errors.New("invalid input")

const (
	textMaxLen    = 2000
	textMinNoLead = 10 // если отзыв без привязки к lead'у, текст обязателен
	authorNameCap = 120
)

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, in CreateInput) (Review, error) {
	in.AuthorName = strings.TrimSpace(in.AuthorName)
	in.Text = strings.TrimSpace(in.Text)

	if in.Rating < 1 || in.Rating > 5 {
		return Review{}, fmt.Errorf("%w: rating must be 1..5", ErrInvalidInput)
	}
	if in.TargetUserID == uuid.Nil {
		return Review{}, fmt.Errorf("%w: target_user_id is required", ErrInvalidInput)
	}
	if in.AuthorUserID == in.TargetUserID {
		return Review{}, fmt.Errorf("%w: cannot review yourself", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Text) > textMaxLen {
		return Review{}, fmt.Errorf("%w: text too long (max %d)", ErrInvalidInput, textMaxLen)
	}
	if utf8.RuneCountInString(in.AuthorName) > authorNameCap {
		return Review{}, fmt.Errorf("%w: author_name too long (max %d)", ErrInvalidInput, authorNameCap)
	}
	if in.LeadID == nil && utf8.RuneCountInString(in.Text) < textMinNoLead {
		return Review{}, fmt.Errorf("%w: text must be at least %d chars when no lead is referenced", ErrInvalidInput, textMinNoLead)
	}

	isSpec, err := s.repo.TargetIsSpecialist(ctx, in.TargetUserID)
	if err != nil {
		return Review{}, err
	}
	if !isSpec {
		return Review{}, fmt.Errorf("%w: target is not a specialist", ErrInvalidInput)
	}

	if in.LeadID != nil {
		ok, err := s.repo.LeadAuthorizesReview(ctx, *in.LeadID, in.AuthorUserID, in.TargetUserID)
		if err != nil {
			return Review{}, err
		}
		if !ok {
			return Review{}, ErrLeadCheck
		}
	}

	id, err := s.repo.Create(ctx, in)
	if err != nil {
		return Review{}, err
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Update(ctx context.Context, id, authorID uuid.UUID, in UpdateInput) (Review, error) {
	if in.Rating == nil && in.Text == nil {
		return Review{}, fmt.Errorf("%w: nothing to update", ErrInvalidInput)
	}
	if in.Rating != nil && (*in.Rating < 1 || *in.Rating > 5) {
		return Review{}, fmt.Errorf("%w: rating must be 1..5", ErrInvalidInput)
	}
	if in.Text != nil {
		t := strings.TrimSpace(*in.Text)
		if utf8.RuneCountInString(t) > textMaxLen {
			return Review{}, fmt.Errorf("%w: text too long (max %d)", ErrInvalidInput, textMaxLen)
		}
		in.Text = &t
	}
	if _, err := s.repo.Update(ctx, id, authorID, in); err != nil {
		return Review{}, err
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id, authorID uuid.UUID) error {
	_, err := s.repo.Delete(ctx, id, authorID)
	return err
}

func (s *Service) ListByTarget(ctx context.Context, targetID uuid.UUID, limit, offset int) ([]Review, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.ListByTarget(ctx, targetID, limit, offset)
}
