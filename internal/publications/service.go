package publications

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
	ErrInvalidInput = errors.New("invalid input")
	// ErrNoLinks — сдача без единой ссылки. Отдельная ошибка: это самая
	// частая опечатка на фронте, и она не должна выглядеть как поломка.
	ErrNoLinks = errors.New("не передано ни одной ссылки")
	// ErrDuplicatePlatform — две ссылки на одну площадку в одной сдаче.
	// В БД это поймает UNIQUE, но тогда одна из ссылок молча перезапишет
	// другую, и креатор не узнает, какая уехала.
	ErrDuplicatePlatform = errors.New("две ссылки на одну площадку")
	// ErrCollectorNotSet — сбор статистики не настроен (нет адреса или
	// ключа instacurl). Ошибка, а не тихий no-op: пустой отчёт читается
	// как «ролики никто не смотрит», и это худшая из возможных подмен.
	ErrCollectorNotSet = errors.New("сбор статистики не настроен")
	// ErrCollapsedNoDetail — у закрытого проекта ежедневный ряд схлопнут
	// в один снимок на весь проект, и разбивки по креаторам больше нет.
	ErrCollapsedNoDetail = errors.New("детализация по креаторам не хранится")
)

// maxBatch — потолок на одну пачку. 3 креатора × 60 дней = 180; 500 даёт
// запас и одновременно ловит случай «выбрал весь каталог».
const maxBatch = 500

type Service struct {
	repo *Repo
	// collector — сбор статистики. nil, если интеграция не настроена:
	// тогда RunCollection честно возвращает ошибку, а не тихо ничего
	// не делает.
	collector Collector
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// CreateBatchByScheme — простановка дат по быстрой схеме. Это основной путь
// менеджера: выбрал креаторов, выбрал «вторник-четверг» и границы месяца.
func (s *Service) CreateBatchByScheme(ctx context.Context, projectID uuid.UUID,
	creatorIDs []uuid.UUID, scheme Scheme, from, to time.Time,
	draftLeadDays int, createdBy uuid.UUID) (BatchResult, error) {

	dates, err := GenerateDates(scheme, from, to)
	if err != nil {
		return BatchResult{}, err
	}
	if len(dates) == 0 {
		return BatchResult{}, fmt.Errorf("%w: в этом диапазоне схема не даёт ни одной даты", ErrInvalidInput)
	}
	return s.CreateBatch(ctx, CreateBatchInput{
		ProjectID:      projectID,
		CreatorUserIDs: creatorIDs,
		Dates:          dates,
		DraftLeadDays:  draftLeadDays,
		CreatedBy:      createdBy,
	})
}

// CreateBatch — простановка произвольного набора дат.
func (s *Service) CreateBatch(ctx context.Context, in CreateBatchInput) (BatchResult, error) {
	in.CreatorUserIDs = dedupeIDs(in.CreatorUserIDs)
	if len(in.CreatorUserIDs) == 0 {
		return BatchResult{}, fmt.Errorf("%w: не выбран ни один креатор", ErrInvalidInput)
	}
	in.Dates = dedupeDates(in.Dates)
	if len(in.Dates) == 0 {
		return BatchResult{}, fmt.Errorf("%w: не выбрано ни одной даты", ErrInvalidInput)
	}
	if total := len(in.CreatorUserIDs) * len(in.Dates); total > maxBatch {
		return BatchResult{}, fmt.Errorf("%w: пачка на %d выкладок, потолок %d", ErrInvalidInput, total, maxBatch)
	}
	if in.DraftLeadDays < 0 || in.DraftLeadDays > 30 {
		return BatchResult{}, fmt.Errorf("%w: срок черновика вне разумных границ", ErrInvalidInput)
	}
	return s.repo.CreateBatch(ctx, in)
}

// PreviewBatch — что будет создано, до создания. Массовое создание —
// единственное место, где одна ошибка стоит ручной чистки шестидесяти
// строк, поэтому предпросмотр обязателен (см. риск в плане).
func (s *Service) PreviewBatch(scheme Scheme, creatorIDs []uuid.UUID, from, to time.Time) ([]time.Time, int, error) {
	dates, err := GenerateDates(scheme, from, to)
	if err != nil {
		return nil, 0, err
	}
	return dates, len(dates) * len(dedupeIDs(creatorIDs)), nil
}

// SubmitLinks — креатор сдаёт ролик ссылками.
func (s *Service) SubmitLinks(ctx context.Context, in SubmitLinksInput) (Publication, error) {
	if len(in.URLs) == 0 {
		return Publication{}, ErrNoLinks
	}
	// Название — человеческая подпись к ролику, а не заголовок статьи:
	// длинное всё равно обрежется в списке, и лучше сказать об этом сразу.
	in.Title = strings.TrimSpace(in.Title)
	if utf8.RuneCountInString(in.Title) > 120 {
		return Publication{}, fmt.Errorf("%w: название ролика длиннее 120 символов", ErrInvalidInput)
	}
	parsed := make([]Link, 0, len(in.URLs))
	seen := make(map[string]bool, len(in.URLs))
	for _, raw := range in.URLs {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		l, err := ParseLink(raw)
		if err != nil {
			return Publication{}, fmt.Errorf("%q: %w", raw, err)
		}
		if seen[l.Platform] {
			return Publication{}, fmt.Errorf("%w: %s", ErrDuplicatePlatform, l.Platform)
		}
		seen[l.Platform] = true
		parsed = append(parsed, l)
	}
	if len(parsed) == 0 {
		return Publication{}, ErrNoLinks
	}
	in.CheckedItemIDs = dedupeIDs(in.CheckedItemIDs)
	return s.repo.SubmitLinks(ctx, in, parsed)
}

// CloseManually — менеджер закрывает неполную выкладку. Причина обязательна.
func (s *Service) CloseManually(ctx context.Context, in CloseManuallyInput) (Publication, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return Publication{}, fmt.Errorf("%w: закрытие неполной выкладки требует причины", ErrInvalidInput)
	}
	if utf8.RuneCountInString(in.Reason) > 500 {
		return Publication{}, fmt.Errorf("%w: причина слишком длинная", ErrInvalidInput)
	}
	return s.repo.CloseManually(ctx, in)
}

// RequestDateChange — креатор просит перенос.
func (s *Service) RequestDateChange(ctx context.Context, pubID, actorID uuid.UUID,
	newDate time.Time, reason string) (DateRequest, error) {

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DateRequest{}, fmt.Errorf("%w: нужна причина переноса", ErrInvalidInput)
	}
	if utf8.RuneCountInString(reason) > 500 {
		return DateRequest{}, fmt.Errorf("%w: причина слишком длинная", ErrInvalidInput)
	}
	if truncateDay(newDate).Before(truncateDay(time.Now())) {
		return DateRequest{}, fmt.Errorf("%w: перенос в прошлое", ErrInvalidInput)
	}
	return s.repo.RequestDateChange(ctx, pubID, actorID, newDate, reason)
}

func (s *Service) DecideDateRequest(ctx context.Context, requestID, managerID uuid.UUID, approve bool) error {
	return s.repo.DecideDateRequest(ctx, requestID, managerID, approve)
}

func (s *Service) CancelBatch(ctx context.Context, projectID, batchID uuid.UUID) (int, error) {
	return s.repo.CancelBatch(ctx, projectID, batchID)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Publication, error) {
	return s.repo.Get(ctx, id)
}

// ListForManager — все выкладки проекта.
func (s *Service) ListForManager(ctx context.Context, projectID uuid.UUID) ([]Publication, error) {
	return s.repo.ListByProject(ctx, projectID)
}

// ListForCreator — только свои выкладки. Отдельный метод, а не флаг у
// ListForManager: так труднее случайно отдать креатору чужое.
func (s *Service) ListForCreator(ctx context.Context, projectID, creatorID uuid.UUID) ([]Publication, error) {
	return s.repo.ListByCreator(ctx, projectID, creatorID)
}

func (s *Service) AddCreator(ctx context.Context, projectID, creatorID, addedBy uuid.UUID) error {
	return s.repo.AddCreator(ctx, projectID, creatorID, addedBy)
}

func (s *Service) RemoveCreator(ctx context.Context, projectID, creatorID uuid.UUID) error {
	return s.repo.RemoveCreator(ctx, projectID, creatorID)
}

func (s *Service) SnapshotChecklist(ctx context.Context, projectID, templateID, actor uuid.UUID) (int, error) {
	return s.repo.SnapshotChecklist(ctx, projectID, templateID, actor)
}

func (s *Service) ProjectChecklist(ctx context.Context, projectID uuid.UUID) ([]ChecklistItem, error) {
	return s.repo.ProjectChecklist(ctx, projectID)
}

// ---- helpers ----

func dedupeIDs(in []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(in))
	out := make([]uuid.UUID, 0, len(in))
	for _, id := range in {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func dedupeDates(in []time.Time) []time.Time {
	seen := make(map[time.Time]bool, len(in))
	out := make([]time.Time, 0, len(in))
	for _, d := range in {
		day := truncateDay(d)
		if seen[day] {
			continue
		}
		seen[day] = true
		out = append(out, day)
	}
	return out
}

func (s *Service) ManagerHasAccess(ctx context.Context, projectID, managerID uuid.UUID) error {
	return s.repo.ManagerHasAccess(ctx, projectID, managerID)
}

func (s *Service) ProjectOfPublication(ctx context.Context, pubID uuid.UUID) (uuid.UUID, error) {
	return s.repo.ProjectOfPublication(ctx, pubID)
}

func (s *Service) ProjectOfDateRequest(ctx context.Context, reqID uuid.UUID) (uuid.UUID, error) {
	return s.repo.ProjectOfDateRequest(ctx, reqID)
}

// ChecklistMeta — какой шаблон и какой версии подключён к проекту.
func (s *Service) ChecklistMeta(ctx context.Context, projectID uuid.UUID) (*ChecklistSnapshotMeta, error) {
	return s.repo.ChecklistMeta(ctx, projectID)
}
