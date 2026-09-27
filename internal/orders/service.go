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

type Service struct {
	repo *Repo
	// projects — кто заводит проект под заявку. Может быть пустым: см.
	// WithProjects.
	projects ProjectStarter
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// CreateResult — что получилось из заказа.
type CreateResult struct {
	Order Order `json:"order"`
	// BusyCreators — кого из отмеченных мы всё равно взяли в заявку,
	// хотя он отметил себя занятым на этот месяц. Это предупреждение, а
	// не отказ: приглашение ему уйдёт, а решать будет он сам.
	BusyCreators []uuid.UUID `json:"busy_creators,omitempty"`
}

// ProjectStarter — кто умеет завести проект под заявку.
//
// Интерфейсом, а не прямой зависимостью на пакет projects: обратная
// стрелка уже есть, и вторая замкнула бы импорты в кольцо. Поля —
// простыми типами по той же причине: общий DTO пришлось бы держать в
// одном из пакетов, и кольцо вернулось бы через него.
type ProjectStarter interface {
	// StartOrderProject — проект заявки: вид «креаторы под ключ»,
	// заказчик известен, менеджера ещё нет.
	StartOrderProject(
		ctx context.Context, clientID uuid.UUID, title, notes string, monthlyPlan int,
	) (uuid.UUID, error)
	// CancelProject — компенсация, если заявка не записалась.
	CancelOrderProject(ctx context.Context, projectID, clientID uuid.UUID) error
	// UpdateProjectNotes — бриф дописали: менеджер читает его в
	// заметках проекта, и два текста расходиться не должны.
	UpdateProjectNotes(ctx context.Context, projectID uuid.UUID, notes string) error
}

// WithProjects — подключить заведение проектов. Без него заявка
// создаётся по-старому, без проекта: так собран, например, тест,
// которому проект не нужен.
func (s *Service) WithProjects(p ProjectStarter) *Service {
	s.projects = p
	return s
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

	// Лимита «один креатор в первый месяц» больше нет.
	//
	// Он был обещанием клиенту: «берём одного, проверяете формат на
	// небольшой сумме». В новой логике заказчик никого не «берёт» — он
	// отмечает, кого хочет, а состав утверждает менеджер после ответов
	// креаторов. Резать отметки лимитом значит запрещать хотеть.
	//
	// Needed теперь = сколько отметили; ноль допустим: «покажите, кто у
	// вас есть» — это тоже заявка, и менеджер соберёт состав сам.
	if in.Needed < 0 || in.Needed > 50 {
		return out, fmt.Errorf("%w: отметить можно до 50 креаторов", ErrInvalidInput)
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

	// Занятость больше не отказ, а ПРЕДУПРЕЖДЕНИЕ.
	//
	// Раньше занятый в списке ронял всю заявку: приглашения уходили по
	// очереди, и звать занятого было некуда. Теперь приглашение уходит
	// всем, а состав утверждает менеджер — и «этот отметил себя занятым
	// на сентябрь» это факт для разговора, а не повод не принять
	// заявку. Отказать человеку в заявке из-за чужой галочки — худшее,
	// что можно сделать на входе.
	busy, err := s.repo.BusyCreators(ctx, in.CreatorIDs, in.StartMonth)
	if err != nil {
		return out, err
	}
	for _, id := range in.CreatorIDs {
		if busy[id] {
			out.BusyCreators = append(out.BusyCreators, id)
		}
	}

	// Проект заводится ПЕРВЫМ и живёт даже если заказ не запишется.
	//
	// Порядок фиксирован: упадёт второй шаг — останется пустой проект,
	// который заказчик увидит и о котором напишет. Обратный порядок
	// оставил бы заявку без проекта, то есть ровно ту тишину, ради ухода
	// от которой всё и затевается.
	projectID := uuid.Nil
	if s.projects != nil {
		id, err := s.projects.StartOrderProject(
			ctx, in.ClientUserID, projectTitle(in.Brief), in.Brief.Text(), in.VideosCount)
		if err != nil {
			return out, fmt.Errorf("start project: %w", err)
		}
		projectID = id
	}

	order, err := s.repo.Create(ctx, in, terms.ID, projectID, now)
	if err != nil {
		// Компенсация: пустой проект без заявки — мусор в кабинете.
		// Отменённый ListForClient прячет.
		if projectID != uuid.Nil && s.projects != nil {
			_ = s.projects.CancelOrderProject(ctx, projectID, in.ClientUserID)
		}
		return out, err
	}
	out.Order = order
	return out, nil
}

// projectTitle — как назвать проект заявки.
//
// Из брифа, а не «Заявка от 26.09»: в списке проектов заказчика их
// может быть несколько, и различать их по дате — значит открывать
// каждый, чтобы вспомнить, который из них про корм для кошек.
func projectTitle(b OrderBrief) string {
	if b.Product != "" {
		return b.Product
	}
	if b.Goal != "" {
		return b.Goal
	}
	return "Новый проект"
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

// Brief — бриф заявки. Чужую заявку не отдаём: бриф — это описание
// продукта, и заглянуть в него по одному id нельзя.
func (s *Service) Brief(ctx context.Context, orderID, clientID uuid.UUID) (OrderBrief, error) {
	order, err := s.repo.Get(ctx, orderID)
	if err != nil {
		return OrderBrief{}, err
	}
	if order.ClientUserID != clientID {
		return OrderBrief{}, ErrNotFound
	}
	return s.repo.LoadBrief(ctx, orderID)
}

// SaveBrief — дописать бриф. Заодно обновляем заметки проекта: менеджер
// читает бриф там, и разойтись эти два текста не должны.
func (s *Service) SaveBrief(
	ctx context.Context, orderID, clientID uuid.UUID, b OrderBrief,
) (OrderBrief, error) {
	order, err := s.repo.Get(ctx, orderID)
	if err != nil {
		return OrderBrief{}, err
	}
	if order.ClientUserID != clientID {
		return OrderBrief{}, ErrNotFound
	}
	if err := s.repo.SaveBrief(ctx, orderID, b); err != nil {
		return OrderBrief{}, err
	}
	if order.ProjectID != nil && s.projects != nil {
		if err := s.projects.UpdateProjectNotes(ctx, *order.ProjectID, b.Text()); err != nil {
			return OrderBrief{}, err
		}
	}
	return b, nil
}

// RemoveCandidate — менеджер убирает человека из заявки: не подходит, и
// звать его незачем. Состав проекта при этом не меняется.
func (s *Service) RemoveCandidate(ctx context.Context, orderID, creatorID uuid.UUID) (Order, error) {
	if err := s.repo.RemoveCandidate(ctx, orderID, creatorID); err != nil {
		return Order{}, err
	}
	return s.repo.Get(ctx, orderID)
}
