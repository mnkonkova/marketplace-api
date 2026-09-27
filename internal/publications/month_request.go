package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Заявка заказчика на следующий месяц.
//
// В кабинете заказчика есть прикидка «во сколько обойдётся следующий
// месяц»: два ползунка и одно число сверху. Сама по себе она
// справочная — посмотрел и закрыл; дальше человек шёл писать менеджеру
// словами, и половина не доходила вовсе.
//
// Кнопка «Заказать» превращает прикидку в ЗАЯВКУ: у менеджера в проекте
// загорается плашка, в общий чат уходит сообщение. Заявка — это ещё не
// заказ: цену и состав финализирует менеджер. Здесь записано ровно то, о
// чём попросили, и то ЧИСЛО, которое человек видел на экране, — разговор
// пойдёт о нём, а прайс к тому времени может смениться.

// ReminderClientMonthRequest — вид события «заказчик просит следующий
// месяц». Идёт в общий чат: на него отвечает человек, и в CRM он иначе
// узнает об этом, только если зайдёт в проект.
const ReminderClientMonthRequest = "client_month_request"

// MonthRequest — заявка как её видят обе стороны.
type MonthRequest struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	// Month — первое число месяца, о котором просят.
	Month    time.Time `json:"month"`
	Creators int       `json:"creators"`
	Videos   int       `json:"videos"`
	// Ceiling — потолок, показанный заказчику, в копейках.
	Ceiling     int64      `json:"ceiling"`
	CreatedAt   time.Time  `json:"created_at"`
	HandledAt   *time.Time `json:"handled_at,omitempty"`
	RequestedBy uuid.UUID  `json:"requested_by"`
	// ProjectTitle и ClientName нужны сообщению в чат: по одному
	// идентификатору проекта дежурный не поймёт, о ком речь.
	ProjectTitle string `json:"project_title,omitempty"`
	ClientName   string `json:"client_name,omitempty"`
}

// AskMonthInput — что прислал кабинет заказчика.
type AskMonthInput struct {
	ProjectID uuid.UUID
	ClientID  uuid.UUID
	Month     time.Time
	Creators  int
	Videos    int
	Ceiling   int64
}

// AskMonth — оставить (или уточнить) заявку.
//
// Вторая кнопка до разбора первой — это УТОЧНЕНИЕ той же просьбы, а не
// новая заявка: иначе у менеджера копится стопка одинаковых плашек, и
// разбирать он будет верхнюю. Поэтому открытая заявка одна на проект
// (уникальный индекс), а повторный запрос переписывает её числа.
//
// Событие эмитим в той же транзакции: заявка, о которой никто не узнал,
// хуже её отсутствия — человек уверен, что попросил.
func (r *Repo) AskMonth(ctx context.Context, in AskMonthInput, now time.Time) (MonthRequest, error) {
	if in.Creators < 1 {
		return MonthRequest{}, fmt.Errorf("%w: creators must be positive", ErrInvalidInput)
	}
	if in.Videos < 0 {
		return MonthRequest{}, fmt.Errorf("%w: videos must not be negative", ErrInvalidInput)
	}
	if in.Ceiling < 0 {
		return MonthRequest{}, fmt.Errorf("%w: ceiling must not be negative", ErrInvalidInput)
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MonthRequest{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out MonthRequest
	err = tx.QueryRow(ctx, `
INSERT INTO project_month_requests
    (project_id, requested_by, month, creators, videos, ceiling, created_at)
VALUES ($1, $2, date_trunc('month', $3::date), $4, $5, $6, $7)
ON CONFLICT (project_id) WHERE handled_at IS NULL DO UPDATE
SET month = EXCLUDED.month, creators = EXCLUDED.creators,
    videos = EXCLUDED.videos, ceiling = EXCLUDED.ceiling, created_at = EXCLUDED.created_at
RETURNING id, project_id, requested_by, month, creators, videos, ceiling, created_at, handled_at`,
		in.ProjectID, in.ClientID, in.Month, in.Creators, in.Videos, in.Ceiling, now).
		Scan(&out.ID, &out.ProjectID, &out.RequestedBy, &out.Month, &out.Creators,
			&out.Videos, &out.Ceiling, &out.CreatedAt, &out.HandledAt)
	if err != nil {
		return MonthRequest{}, fmt.Errorf("ask month: %w", err)
	}

	if err := tx.QueryRow(ctx, `
SELECT COALESCE(p.title, ''), COALESCE(NULLIF(u.display_name, ''), u.email)
FROM projects p LEFT JOIN users u ON u.id = p.client_user_id
WHERE p.id = $1`, in.ProjectID).Scan(&out.ProjectTitle, &out.ClientName); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return MonthRequest{}, fmt.Errorf("month request context: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, in.ProjectID.String(),
		"project."+ReminderClientMonthRequest, out); err != nil {
		return MonthRequest{}, fmt.Errorf("emit month request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MonthRequest{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// OpenMonthRequest — открытая заявка проекта. Пусто — заявки нет, и это
// нормальное состояние, а не ошибка.
func (r *Repo) OpenMonthRequest(ctx context.Context, projectID uuid.UUID) (*MonthRequest, error) {
	var out MonthRequest
	err := r.db.QueryRow(ctx, `
SELECT m.id, m.project_id, m.requested_by, m.month, m.creators, m.videos,
       m.ceiling, m.created_at, m.handled_at,
       COALESCE(p.title, ''), COALESCE(NULLIF(u.display_name, ''), u.email, '')
FROM project_month_requests m
JOIN projects p ON p.id = m.project_id
LEFT JOIN users u ON u.id = m.requested_by
WHERE m.project_id = $1 AND m.handled_at IS NULL`, projectID).
		Scan(&out.ID, &out.ProjectID, &out.RequestedBy, &out.Month, &out.Creators,
			&out.Videos, &out.Ceiling, &out.CreatedAt, &out.HandledAt,
			&out.ProjectTitle, &out.ClientName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open month request: %w", err)
	}
	return &out, nil
}

// HandleMonthRequest — менеджер разобрал заявку: связался и завёл заказ
// (или отказал). Плашка гаснет, а строка остаётся историей разговора.
//
// Идемпотентно: разобранная заявка второй раз ничего не меняет — кнопку
// нажимают дважды чаще, чем кажется.
func (r *Repo) HandleMonthRequest(
	ctx context.Context, projectID, managerID uuid.UUID, now time.Time,
) error {
	_, err := r.db.Exec(ctx, `
UPDATE project_month_requests
SET handled_at = $3, handled_by = $2
WHERE project_id = $1 AND handled_at IS NULL`, projectID, managerID, now)
	if err != nil {
		return fmt.Errorf("handle month request: %w", err)
	}
	return nil
}

// ---- сервис ----

// AskMonth — заявка от заказчика. Проверяет, что проект действительно его.
func (s *Service) AskMonth(ctx context.Context, in AskMonthInput, now time.Time) (MonthRequest, error) {
	ok, err := s.repo.ClientOwnsProject(ctx, in.ProjectID, in.ClientID)
	if err != nil {
		return MonthRequest{}, err
	}
	if !ok {
		return MonthRequest{}, ErrNotFound
	}
	if in.Month.IsZero() {
		// Месяц не прислали — просят следующий. Считаем от сегодняшнего
		// дня, а не от начала периода: заявку оставляют, глядя в
		// календарь, а не в расчёт.
		in.Month = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	}
	return s.repo.AskMonth(ctx, in, now)
}

// OpenMonthRequest — открытая заявка проекта (для карточки менеджера).
func (s *Service) OpenMonthRequest(ctx context.Context, projectID uuid.UUID) (*MonthRequest, error) {
	return s.repo.OpenMonthRequest(ctx, projectID)
}

// HandleMonthRequest — менеджер разобрал заявку.
func (s *Service) HandleMonthRequest(
	ctx context.Context, projectID, managerID uuid.UUID, now time.Time,
) error {
	return s.repo.HandleMonthRequest(ctx, projectID, managerID, now)
}
