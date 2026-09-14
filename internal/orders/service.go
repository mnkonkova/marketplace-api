package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	// ErrDuplicateCandidate — один и тот же человек дважды в подборке.
	ErrDuplicateCandidate = errors.New("duplicate candidate in the shortlist")
	// ErrNoReserve — в подборке ровно столько, сколько нужно взять.
	// Не ошибка, а предупреждение: при первом отказе выбирать придётся
	// заново. Сервис отдаёт его отдельным флагом, а не отказом.
	ErrNoReserve = errors.New("shortlist has no reserve")
)

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// CreateResult — что получилось из заказа.
type CreateResult struct {
	Order Order `json:"order"`
	// WithoutReserve — в подборке ровно нужное число. При отказе одного
	// придётся выбирать заново, и клиента об этом предупреждают.
	WithoutReserve bool `json:"without_reserve"`
}

// Create — завести заказ.
//
// Порядок проверок не случаен: сначала согласие с правилами (без него
// вообще нельзя), потом ограничение объёма (оно про клиента), потом
// состав подборки (он про конкретных людей). Так сообщение об ошибке
// всегда про самое существенное, а не про мелочь на фоне запрета.
func (s *Service) Create(ctx context.Context, in CreateOrderInput, now time.Time) (CreateResult, error) {
	var out CreateResult

	terms, err := s.repo.CurrentTerms(ctx)
	if err != nil {
		return out, err
	}
	ok, err := s.repo.HasConsent(ctx, in.ClientUserID, terms.ID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrNoConsent
	}

	// Лимит считаем на месяц заказа, а не на сегодня: клиент мог выбрать
	// декабрь, и к декабрю он отработает больше месяцев, чем сейчас.
	allowed, err := s.repo.AllowedCreators(ctx, in.ClientUserID, in.StartMonth)
	if err != nil {
		return out, err
	}
	if in.Needed < 1 {
		return out, fmt.Errorf("%w: нужен хотя бы один креатор", ErrInvalidInput)
	}
	if in.Needed > allowed {
		return out, fmt.Errorf("%w: сейчас доступно %d", ErrTooManyCreators, allowed)
	}
	if in.VideosCount <= 0 {
		return out, fmt.Errorf("%w: объём роликов должен быть больше нуля", ErrInvalidInput)
	}
	if firstOfMonth(in.StartMonth).Before(firstOfMonth(now)) {
		return out, fmt.Errorf("%w: месяц старта в прошлом", ErrInvalidInput)
	}

	seen := make(map[uuid.UUID]bool, len(in.CreatorIDs))
	for _, id := range in.CreatorIDs {
		if id == uuid.Nil {
			return out, fmt.Errorf("%w: пустой id креатора", ErrInvalidInput)
		}
		if seen[id] {
			return out, fmt.Errorf("%w: %s", ErrDuplicateCandidate, id)
		}
		seen[id] = true
	}
	if len(in.CreatorIDs) < in.Needed {
		return out, fmt.Errorf("%w: выбрано %d, нужно минимум %d",
			ErrNotEnoughCandidates, len(in.CreatorIDs), in.Needed)
	}

	// В пакет блогеров попадают только креаторы: люди со своими
	// аккаунтами из категорий blogger и ugc. Монтажёр или продакшн-студия
	// здесь — не придирка, а другая модель работы и другие деньги.
	suitable, err := s.repo.FilterCreators(ctx, in.CreatorIDs)
	if err != nil {
		return out, err
	}
	for _, id := range in.CreatorIDs {
		if !suitable[id] {
			return out, fmt.Errorf("%w: %s", ErrNotACreator, id)
		}
	}

	// Занятость проверяем ДО создания: иначе первым в списке окажется
	// тот, кто взять не может, и заказ провисит трое суток впустую.
	busy, err := s.repo.BusyCreators(ctx, in.CreatorIDs, in.StartMonth)
	if err != nil {
		return out, err
	}
	for _, id := range in.CreatorIDs {
		if busy[id] {
			return out, fmt.Errorf("%w: %s", ErrCreatorBusy, id)
		}
	}

	order, err := s.repo.Create(ctx, in, terms.ID, now)
	if err != nil {
		return out, err
	}
	out.Order = order
	out.WithoutReserve = len(in.CreatorIDs) == in.Needed
	return out, nil
}

// SendInvitations — отправить приглашения первым по приоритету.
func (s *Service) SendInvitations(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	return s.repo.SendInvitations(ctx, orderID, now)
}

// Respond — креатор согласился или отказался.
func (s *Service) Respond(ctx context.Context, orderID, creatorID uuid.UUID, accept bool, now time.Time) (Order, error) {
	return s.repo.Respond(ctx, orderID, creatorID, accept, now)
}

// ByProject — заказ, из которого вырос проект. У заведённого руками
// проекта заказа нет, и это ErrNotFound, а не сбой.
func (s *Service) ByProject(ctx context.Context, projectID uuid.UUID) (Order, error) {
	return s.repo.GetByProject(ctx, projectID)
}

// InviteNext — позвать следующих по приоритету на свободные места.
func (s *Service) InviteNext(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	return s.repo.InviteNext(ctx, orderID, now)
}

// AddCandidates — добрать людей в подборку, когда резерв кончился.
func (s *Service) AddCandidates(ctx context.Context, orderID uuid.UUID, creatorIDs []uuid.UUID, now time.Time) (Order, error) {
	return s.repo.AddCandidates(ctx, orderID, creatorIDs, now)
}

// RunExpiry — фоновый проход по протухшим приглашениям.
func (s *Service) RunExpiry(ctx context.Context, now time.Time) (expired, pinged int, err error) {
	return s.repo.ExpireAndAdvance(ctx, now)
}

func (s *Service) Get(ctx context.Context, orderID uuid.UUID) (Order, error) {
	return s.repo.Get(ctx, orderID)
}

func (s *Service) CurrentTerms(ctx context.Context) (Terms, error) {
	return s.repo.CurrentTerms(ctx)
}

// Consent — зафиксировать согласие с действующей версией правил.
func (s *Service) Consent(ctx context.Context, userID uuid.UUID) (Terms, error) {
	terms, err := s.repo.CurrentTerms(ctx)
	if err != nil {
		return Terms{}, err
	}
	if err := s.repo.Consent(ctx, userID, terms.ID); err != nil {
		return Terms{}, err
	}
	return terms, nil
}

// AllowedCreators — сколько креаторов клиент может взять сейчас.
//
// Клиент видит это ДО подбора, а не при попытке добавить второго:
// ограничение, всплывающее посреди работы, читается как поломка.
func (s *Service) AllowedCreators(ctx context.Context, clientID uuid.UUID, month time.Time) (int, error) {
	return s.repo.AllowedCreators(ctx, clientID, month)
}

// SetAvailability — креатор отмечает занятость в месяце.
func (s *Service) SetAvailability(ctx context.Context, creatorID uuid.UUID, month time.Time, available bool) error {
	return s.repo.SetAvailability(ctx, creatorID, month, available)
}

// BusyCreators — кто из списка занят в месяце.
func (s *Service) BusyCreators(ctx context.Context, ids []uuid.UUID, month time.Time) (map[uuid.UUID]bool, error) {
	return s.repo.BusyCreators(ctx, ids, month)
}

// FilterCreators — кто из списка подходит на роль креатора.
func (s *Service) FilterCreators(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return s.repo.FilterCreators(ctx, ids)
}

func (s *Service) OwnedByClient(ctx context.Context, orderID, clientID uuid.UUID) (bool, error) {
	return s.repo.OwnedByClient(ctx, orderID, clientID)
}

func (s *Service) ListByClient(ctx context.Context, clientID uuid.UUID, limit int) ([]Order, error) {
	return s.repo.ListByClient(ctx, clientID, limit)
}

func (s *Service) InvitationsFor(ctx context.Context, creatorID uuid.UUID) ([]Invitation, error) {
	return s.repo.InvitationsFor(ctx, creatorID)
}

// NeedingAttention — заказы, где резерв кончился, а состав не собран.
func (s *Service) NeedingAttention(ctx context.Context) ([]Order, error) {
	return s.repo.NeedingAttention(ctx)
}

func (s *Service) Cancel(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	return s.repo.Cancel(ctx, orderID, now)
}

// MarkPaid — отметка оплаты вручную: платежей в системе нет.
func (s *Service) MarkPaid(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	return s.repo.MarkPaid(ctx, orderID, now)
}

func (s *Service) LinkProject(ctx context.Context, orderID, projectID uuid.UUID) error {
	return s.repo.LinkProject(ctx, orderID, projectID)
}

// MyAvailability — своя занятость на ближайшие месяцы.
func (s *Service) MyAvailability(ctx context.Context, creatorID uuid.UUID, months int) ([]Availability, error) {
	return s.repo.MyAvailability(ctx, creatorID, months)
}

// CompletedMonths — сколько оплаченных месяцев будет закрыто к указанному
// месяцу. Нужно клиенту, чтобы «доступен один креатор» не выглядело
// произволом: видно, что месяцев работы пока ноль.
func (s *Service) CompletedMonths(ctx context.Context, clientID uuid.UUID, month time.Time) (int, error) {
	return s.repo.CompletedPaidMonths(ctx, clientID, firstOfMonth(month))
}
