package leads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput    = errors.New("invalid input")
	ErrNoSpecialists   = errors.New("no valid specialists in recipients")
	// ErrSpecialistUnpublished — хотя бы один из выбранных специалистов
	// не опубликован (или удалён). Брифу не даём отправиться — иначе
	// клиент думает, что отправил пятерым, а реально дошло двоим.
	ErrSpecialistUnpublished = errors.New("some recipients are not published")
	// ErrEmailUnverified — soft-gate: для авторизованного клиента почта
	// должна быть подтверждена. Хендлер мапит в 403 email_unverified.
	ErrEmailUnverified = errors.New("email is not verified")
	// ErrUserInactive — auth-юзер деактивирован (is_active=false). Middleware
	// не реchecker'ит каждый запрос, поэтому JWT инактивированного юзера всё
	// ещё валиден до своего TTL — ловим здесь, до рассылки.
	ErrUserInactive = errors.New("user is inactive")
)

// EmailVerifier — узкий интерфейс soft-gate'а. Реализуется auth.Service.
// Возвращает (active, verified, err). nil-safe: без подключения gate не
// действует (анонимные лиды всегда проходят без проверки).
type EmailVerifier interface {
	CheckActiveVerified(ctx context.Context, userID uuid.UUID) (active, verified bool, err error)
}

// ProjectStarter — узкий интерфейс для создания проекта в CRM сразу после
// брифа авторизованного клиента. Реализуется projects.Service. nil-safe:
// без подключения проект не создаётся, только бриф (back-compat).
// proposedSpecialistID — выбранный клиентом спец (ровно один в брифе);
// сохраняется как «предложение», менеджер аппрувит через approve_specialist.
type ProjectStarter interface {
	StartFromLead(ctx context.Context, clientID uuid.UUID, leadID uuid.UUID, title, brief string, proposedSpecialistID *uuid.UUID) (uuid.UUID, error)
}

type Service struct {
	repo           *Repo
	verifier       EmailVerifier
	projectStarter ProjectStarter
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// WithProjectStarter — при POST /leads от залогиненного клиента дополнительно
// создаём проект в CRM с default-воронкой и assigned_to=NULL (в inbox
// менеджеров). Если starter не настроен или default-воронки нет — лид
// просто остаётся брифом без проекта.
func (s *Service) WithProjectStarter(p ProjectStarter) *Service {
	s.projectStarter = p
	return s
}

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

	// Soft-gate: авторизованный клиент должен быть активен + email подтверждён.
	// Анонимный путь (ClientUserID == nil) — без проверки. Активность проверяем
	// здесь же: JWT деактивированного юзера всё ещё валиден до TTL.
	if in.ClientUserID != nil && s.verifier != nil {
		active, verified, err := s.verifier.CheckActiveVerified(ctx, *in.ClientUserID)
		if err != nil {
			return CreateResult{}, fmt.Errorf("verify user: %w", err)
		}
		if !active {
			return CreateResult{}, ErrUserInactive
		}
		if !verified {
			return CreateResult{}, ErrEmailUnverified
		}
	}

	in.SpecialistIDs = DedupUUIDs(in.SpecialistIDs)

	valid, err := s.repo.ValidPublishedSpecialists(ctx, in.SpecialistIDs)
	if err != nil {
		return CreateResult{}, err
	}
	if len(valid) == 0 {
		return CreateResult{}, ErrNoSpecialists
	}
	// Строгая проверка: бриф отправится ровно тем, кого клиент выбрал.
	// Если каталог отдал устаревшие/несуществующие id (например, спеца сняли
	// с публикации после того как клиент положил его в корзину) — лучше
	// сказать «обнови корзину», чем тихо удалить часть получателей.
	if len(valid) < len(in.SpecialistIDs) {
		return CreateResult{}, ErrSpecialistUnpublished
	}
	in.SpecialistIDs = valid

	id, err := s.repo.Create(ctx, in)
	if errors.Is(err, ErrRecipientUnpublishedRace) {
		// R6: между ValidPublishedSpecialists и INSERT кого-то сняли с
		// публикации — отдаём то же сообщение, что и при offline-фильтре.
		return CreateResult{}, ErrSpecialistUnpublished
	}
	if err != nil {
		return CreateResult{}, err
	}
	// Если клиент залогинен и есть default-воронка — создаём по проекту в CRM
	// на каждого выбранного спеца (в inbox менеджеров). Best-effort:
	// ошибка одного спеца не валит ни другие проекты, ни ответ /leads.
	//
	// Раньше делали один проект с proposed=NULL когда выбрано >1 спеца —
	// менеджер должен был сам резолвить «кому из N отдать». На практике
	// получалось хуже: один лид с 5 спецами выглядел в инбоксе как один
	// проект «5 кандидатов», менеджер тратил время на merging вместо того
	// чтобы быстро отдать каждому своего. N проектов = N независимых
	// pipelines, каждый со своим proposed.
	if in.ClientUserID != nil && s.projectStarter != nil {
		title := briefTitle(in.Brief)
		for _, sid := range valid {
			specID := sid
			if _, perr := s.projectStarter.StartFromLead(ctx, *in.ClientUserID, id, title, in.Brief, &specID); perr != nil {
				slog.Error("leads: StartFromLead failed",
					"lead_id", id, "client_id", in.ClientUserID,
					"specialist_id", specID, "err", perr)
			}
		}
	}
	// Контакты подгружаем уже после создания — они уезжают в ответ ровно
	// в этот момент (видимы только менеджеру/клиенту, после отправки брифа).
	contacts, err := s.repo.LoadSpecialistContacts(ctx, valid)
	if err != nil {
		// Лид уже создан; контакты — best-effort. Не валим запрос, но без
		// лога такая ошибка невидима — фронт покажет пустые контакты и юзер
		// не поймёт, почему. Симметрично логу StartFromLead выше.
		slog.Error("leads: LoadSpecialistContacts failed",
			"lead_id", id, "err", err)
		return CreateResult{ID: id, Specialists: nil}, nil
	}
	return CreateResult{ID: id, Specialists: contacts}, nil
}

// briefTitle — короткое название проекта из первых ~80 символов брифа.
// Берём первую строку (до \n), обрезаем до разумной длины.
func briefTitle(brief string) string {
	brief = strings.TrimSpace(brief)
	if i := strings.IndexByte(brief, '\n'); i > 0 {
		brief = brief[:i]
	}
	if utf8.RuneCountInString(brief) > 80 {
		// Берём первые 80 рун, ища пробел чтобы не резать слово
		runes := []rune(brief)
		brief = string(runes[:80]) + "…"
	}
	if brief == "" {
		return "Новый проект"
	}
	return brief
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

func DedupUUIDs(in []uuid.UUID) []uuid.UUID {
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
