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
	ErrInvalidInput    = errors.New("invalid input")
	ErrNoSpecialists   = errors.New("no valid specialists in recipients")
	// ErrEmailUnverified — soft-gate: для авторизованного клиента почта
	// должна быть подтверждена. Хендлер мапит в 403 email_unverified.
	ErrEmailUnverified = errors.New("email is not verified")
)

// EmailVerifier — узкий интерфейс soft-gate'а. Реализуется auth.Service.
// (true, nil) = подтверждена; (false, nil) = НЕ подтверждена; (_, err) = ошибка.
// nil-safe: без подключения gate не действует (анонимные лиды всегда
// проходят без проверки).
type EmailVerifier interface {
	IsEmailVerified(ctx context.Context, userID uuid.UUID) (bool, error)
}

type Service struct {
	repo     *Repo
	verifier EmailVerifier
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// WithEmailVerifier — подключает soft-gate: авторизованный клиент с
// неподтверждённым email не может создать лид. Анонимные (без auth)
// продолжают работать без проверки.
func (s *Service) WithEmailVerifier(v EmailVerifier) *Service {
	s.verifier = v
	return s
}

func (s *Service) Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	in.ClientName = strings.TrimSpace(in.ClientName)
	in.ClientContact = strings.TrimSpace(in.ClientContact)
	in.Brief = strings.TrimSpace(in.Brief)
	in.TargetCategoryCode = strings.TrimSpace(in.TargetCategoryCode)

	if in.ClientName == "" {
		return CreateResult{}, fmt.Errorf("%w: client_name is required", ErrInvalidInput)
	}
	if in.ClientContact == "" {
		return CreateResult{}, fmt.Errorf("%w: client_contact is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Brief) < 10 {
		return CreateResult{}, fmt.Errorf("%w: brief must be at least 10 characters", ErrInvalidInput)
	}
	if in.BudgetMin != nil && *in.BudgetMin < 0 {
		return CreateResult{}, fmt.Errorf("%w: budget_min must be >= 0", ErrInvalidInput)
	}
	if in.BudgetMax != nil && *in.BudgetMax < 0 {
		return CreateResult{}, fmt.Errorf("%w: budget_max must be >= 0", ErrInvalidInput)
	}
	if in.BudgetMin != nil && in.BudgetMax != nil && *in.BudgetMin > *in.BudgetMax {
		return CreateResult{}, fmt.Errorf("%w: budget_min must be <= budget_max", ErrInvalidInput)
	}
	if in.Deadline != nil && in.Deadline.Before(time.Now().AddDate(0, 0, -1)) {
		return CreateResult{}, fmt.Errorf("%w: deadline cannot be in the past", ErrInvalidInput)
	}
	if len(in.SpecialistIDs) == 0 {
		return CreateResult{}, fmt.Errorf("%w: at least one specialist is required", ErrInvalidInput)
	}

	// Soft-gate: авторизованный клиент должен подтвердить email перед
	// созданием лида. Анонимный путь (ClientUserID == nil) — без проверки.
	if in.ClientUserID != nil && s.verifier != nil {
		ok, err := s.verifier.IsEmailVerified(ctx, *in.ClientUserID)
		if err != nil {
			return CreateResult{}, fmt.Errorf("verify email: %w", err)
		}
		if !ok {
			return CreateResult{}, ErrEmailUnverified
		}
	}

	in.SpecialistIDs = dedupUUIDs(in.SpecialistIDs)

	valid, err := s.repo.ValidPublishedSpecialists(ctx, in.SpecialistIDs)
	if err != nil {
		return CreateResult{}, err
	}
	if len(valid) == 0 {
		return CreateResult{}, ErrNoSpecialists
	}
	in.SpecialistIDs = valid

	id, err := s.repo.Create(ctx, in)
	if err != nil {
		return CreateResult{}, err
	}
	// Контакты подгружаем уже после создания — они уезжают в ответ ровно
	// в этот момент (видимы только менеджеру/клиенту, после отправки брифа).
	contacts, err := s.repo.LoadSpecialistContacts(ctx, valid)
	if err != nil {
		// Лид уже создан; контакты — best-effort. Не валим запрос.
		return CreateResult{ID: id, Specialists: nil}, nil
	}
	return CreateResult{ID: id, Specialists: contacts}, nil
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

func (s *Service) UpdateRecipientStatus(ctx context.Context, leadID, specialistID uuid.UUID, status string, expectedUpdatedAt *time.Time) error {
	switch status {
	case RecipientStatusViewed, RecipientStatusAccepted, RecipientStatusDeclined:
	default:
		return fmt.Errorf("%w: status must be viewed, accepted or declined", ErrInvalidInput)
	}
	return s.repo.UpdateRecipientStatus(ctx, leadID, specialistID, status, expectedUpdatedAt)
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
