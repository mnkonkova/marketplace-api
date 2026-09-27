package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrSpecialistUnavailable — выбранный исполнитель не годится: нет
	// такого пользователя, профиль не опубликован, аккаунт выключен или
	// это менеджер/админ.
	ErrSpecialistUnavailable = errors.New("specialist cannot take this project")
	// ErrDeliveryPending — уже есть сданная и не разобранная работа.
	ErrDeliveryPending = errors.New("delivery awaits client decision")
	// ErrNoPendingDelivery — клиент отвечает на сдачу, которой нет.
	ErrNoPendingDelivery = errors.New("nothing to accept or send back")
	// ErrProjectClosed — проект уже принят или отменён.
	ErrProjectClosed = errors.New("project is closed")
	// ErrReworkLimit — правки по проекту закончились.
	ErrReworkLimit = errors.New("rework limit reached")
)

// CreateGeneral — завести общий проект.
//
// Воронку не материализуем: у этого вида её нет, pipeline_id остаётся NULL
// (см. 00034). Пригодность исполнителя проверяем в той же транзакции, что
// и вставку: между проверкой отдельным запросом и вставкой профиль успевает
// уйти с публикации, и проект уезжает человеку, которого клиент в выдаче
// уже не увидел бы.
func (r *Repo) CreateGeneral(ctx context.Context, in CreateGeneralInput) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Исполнитель: активный опубликованный специалист, не менеджер и не
	// админ. FOR SHARE держит строку до конца транзакции.
	var ok bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM users u
  JOIN specialist_profiles sp ON sp.user_id = u.id
  WHERE u.id = $1 AND u.is_active
    AND NOT u.is_manager AND NOT u.is_admin
    AND sp.is_published
)`, in.SpecialistID).Scan(&ok); err != nil {
		return uuid.Nil, fmt.Errorf("check specialist: %w", err)
	}
	if !ok {
		return uuid.Nil, ErrSpecialistUnavailable
	}

	var projectID uuid.UUID
	if err := tx.QueryRow(ctx, `
INSERT INTO projects
  (client_user_id, specialist_user_id, kind, status, source,
   title, notes, due_date, budget, started_at)
VALUES ($1, $2, 'general', 'active', 'manual', $3, $4, $5, $6, now())
RETURNING id`,
		in.ClientID, in.SpecialistID, in.Title, in.Brief,
		in.DueDate, in.Budget).Scan(&projectID); err != nil {
		return uuid.Nil, fmt.Errorf("insert general project: %w", err)
	}

	// «Уходит исполнителю в бот» — это событие. Менеджера в проекте нет,
	// и других способов узнать о задаче у исполнителя тоже нет.
	payload := map[string]any{
		"project_id":         projectID.String(),
		"client_user_id":     in.ClientID.String(),
		"specialist_user_id": in.SpecialistID.String(),
		"due_date":           in.DueDate.Format("2006-01-02"),
		"kind":               "general",
	}
	enrichProjectPayload(ctx, tx, projectID, payload)
	if err := emit(ctx, tx, projectID, nil, in.ClientID, "project.general_created", payload); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return projectID, nil
}

const generalCols = `p.id, p.title, COALESCE(p.notes, ''), p.status::text, p.due_date,
       p.budget, p.client_user_id, COALESCE(cp.display_name, ''),
       p.specialist_user_id, COALESCE(sp.display_name, ''),
       p.revisions_included, p.revisions_used, p.created_at, p.completed_at`

// GetGeneral — карточка со сдачами. viewer должен быть клиентом или
// исполнителем этого проекта: сравнение идёт в WHERE, а не после выборки,
// чтобы «не мой проект» и «нет такого» отвечали одинаково.
func (r *Repo) GetGeneral(ctx context.Context, projectID, viewer uuid.UUID) (GeneralProject, error) {
	p, err := r.scanGeneral(ctx, `
SELECT `+generalCols+`
FROM projects p
LEFT JOIN client_profiles cp     ON cp.user_id = p.client_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = p.specialist_user_id
WHERE p.id = $1 AND p.kind = 'general'
  AND $2 IN (p.client_user_id, p.specialist_user_id)`, projectID, viewer)
	if err != nil {
		return GeneralProject{}, err
	}
	p.Deliveries, err = r.listDeliveries(ctx, projectID)
	if err != nil {
		return GeneralProject{}, err
	}
	return p, nil
}

func (r *Repo) scanGeneral(ctx context.Context, q string, args ...any) (GeneralProject, error) {
	var p GeneralProject
	err := r.db.QueryRow(ctx, q, args...).Scan(
		&p.ID, &p.Title, &p.Brief, &p.Status, &p.DueDate, &p.Budget,
		&p.ClientID, &p.ClientName, &p.SpecialistID, &p.SpecialistName,
		&p.RevisionsIncluded, &p.RevisionsUsed, &p.CreatedAt, &p.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GeneralProject{}, ErrNotFound
	}
	if err != nil {
		return GeneralProject{}, fmt.Errorf("load general project: %w", err)
	}
	p.Overdue = overdue(p)
	return p, nil
}

// overdue — срок прошёл, а работа не принята. Отменённый проект просрочкой
// не считается: его никто не ждёт.
func overdue(p GeneralProject) bool {
	if p.Status != "active" {
		return false
	}
	return p.DueDate.Before(time.Now().UTC().Truncate(24 * time.Hour))
}

// ListGeneral — «мои общие проекты». asClient=true — те, где я заказчик,
// иначе — те, где я исполнитель.
func (r *Repo) ListGeneral(ctx context.Context, userID uuid.UUID, asClient bool) ([]GeneralProject, error) {
	col := "p.specialist_user_id"
	if asClient {
		col = "p.client_user_id"
	}
	rows, err := r.db.Query(ctx, `
SELECT `+generalCols+`
FROM projects p
LEFT JOIN client_profiles cp     ON cp.user_id = p.client_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = p.specialist_user_id
WHERE p.kind = 'general' AND `+col+` = $1
ORDER BY p.due_date, p.created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list general projects: %w", err)
	}
	defer rows.Close()
	out := make([]GeneralProject, 0)
	for rows.Next() {
		var p GeneralProject
		if err := rows.Scan(
			&p.ID, &p.Title, &p.Brief, &p.Status, &p.DueDate, &p.Budget,
			&p.ClientID, &p.ClientName, &p.SpecialistID, &p.SpecialistName,
			&p.RevisionsIncluded, &p.RevisionsUsed, &p.CreatedAt, &p.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan general project: %w", err)
		}
		p.Overdue = overdue(p)
		out = append(out, p)
	}
	// Сдачи в списке не грузим: список показывает срок и статус, а разбор
	// попыток нужен только в карточке.
	return out, rows.Err()
}

func (r *Repo) listDeliveries(ctx context.Context, projectID uuid.UUID) ([]Delivery, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, attempt, submitted_by, submitted_at, note,
       decision::text, decided_at, rework_reason
FROM general_deliveries
WHERE project_id = $1
ORDER BY attempt`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	out := make([]Delivery, 0)
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.Attempt, &d.SubmittedBy, &d.SubmittedAt,
			&d.Note, &d.Decision, &d.DecidedAt, &d.ReworkReason); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, d)
		ids = append(ids, d.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	// Материалы — одним запросом на все попытки, а не запросом на каждую.
	mrows, err := r.db.Query(ctx, `
SELECT delivery_id, id, kind, title, url
FROM project_materials
WHERE delivery_id = ANY($1)
ORDER BY sort_order, created_at`, ids)
	if err != nil {
		return nil, fmt.Errorf("list delivery materials: %w", err)
	}
	defer mrows.Close()
	byDelivery := make(map[uuid.UUID][]Material)
	for mrows.Next() {
		var did uuid.UUID
		var m Material
		if err := mrows.Scan(&did, &m.ID, &m.Kind, &m.Title, &m.URL); err != nil {
			return nil, fmt.Errorf("scan material: %w", err)
		}
		byDelivery[did] = append(byDelivery[did], m)
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("list delivery materials: %w", err)
	}
	for i := range out {
		out[i].Materials = byDelivery[out[i].ID]
	}
	return out, nil
}

// Deliver — исполнитель сдаёт работу.
func (r *Repo) Deliver(ctx context.Context, in DeliverInput) (Delivery, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Delivery{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE — иначе две одновременные сдачи посчитают одинаковый
	// номер попытки, и одна упадёт на UNIQUE вместо внятного ответа.
	var status string
	if err := tx.QueryRow(ctx, `
SELECT status::text FROM projects
WHERE id = $1 AND kind = 'general' AND specialist_user_id = $2
FOR UPDATE`, in.ProjectID, in.UserID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Delivery{}, ErrNotFound
		}
		return Delivery{}, fmt.Errorf("lock project: %w", err)
	}
	if status != "active" {
		return Delivery{}, ErrProjectClosed
	}

	var attempt int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(attempt), 0) + 1 FROM general_deliveries WHERE project_id = $1`,
		in.ProjectID).Scan(&attempt); err != nil {
		return Delivery{}, fmt.Errorf("count attempts: %w", err)
	}

	d := Delivery{Attempt: attempt, SubmittedBy: in.UserID, Note: in.Note, Decision: DeliveryPending}
	if err := tx.QueryRow(ctx, `
INSERT INTO general_deliveries (project_id, attempt, submitted_by, note)
VALUES ($1, $2, $3, $4)
RETURNING id, submitted_at`,
		in.ProjectID, attempt, in.UserID, in.Note).Scan(&d.ID, &d.SubmittedAt); err != nil {
		// Частичный уникальный индекс по pending: двойной клик приходит
		// сюда, и это не 500, а «уже сдано».
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Delivery{}, ErrDeliveryPending
		}
		return Delivery{}, fmt.Errorf("insert delivery: %w", err)
	}

	for i, m := range in.Materials {
		var mat Material
		if err := tx.QueryRow(ctx, `
-- audience='client': приложенное к сдаче — это результат работы,
-- заказчик его и смотрит. Материалы проекта по умолчанию наоборот,
-- для креаторов (см. 00036).
INSERT INTO project_materials
  (project_id, delivery_id, kind, title, url, sort_order, created_by, audience)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'client')
RETURNING id, kind, title, url`,
			in.ProjectID, d.ID, m.Kind, m.Title, m.URL, i, in.UserID).
			Scan(&mat.ID, &mat.Kind, &mat.Title, &mat.URL); err != nil {
			return Delivery{}, fmt.Errorf("insert material: %w", err)
		}
		d.Materials = append(d.Materials, mat)
	}

	if err := r.emitGeneral(ctx, tx, in.ProjectID, in.UserID, "project.general_delivered",
		map[string]any{"delivery_id": d.ID.String(), "attempt": attempt}); err != nil {
		return Delivery{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Delivery{}, fmt.Errorf("commit: %w", err)
	}
	return d, nil
}

// DecideDelivery — клиент принимает работу или возвращает на доработку.
// Два действия в одном методе, потому что различаются они одной ветвью, а
// совпадают во всём остальном: та же блокировка, та же незакрытая сдача,
// та же проверка, что решает именно заказчик.
func (r *Repo) DecideDelivery(
	ctx context.Context, projectID, clientID uuid.UUID, accept bool, reason string,
) (GeneralProject, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return GeneralProject{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var included, used int
	if err := tx.QueryRow(ctx, `
SELECT status::text, revisions_included, revisions_used FROM projects
WHERE id = $1 AND kind = 'general' AND client_user_id = $2
FOR UPDATE`, projectID, clientID).Scan(&status, &included, &used); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GeneralProject{}, ErrNotFound
		}
		return GeneralProject{}, fmt.Errorf("lock project: %w", err)
	}
	if status != "active" {
		return GeneralProject{}, ErrProjectClosed
	}
	if !accept && used >= included {
		return GeneralProject{}, ErrReworkLimit
	}

	decision := DeliveryAccepted
	if !accept {
		decision = DeliveryRework
	}
	var deliveryID uuid.UUID
	if err := tx.QueryRow(ctx, `
UPDATE general_deliveries
SET decision = $3, decided_by = $2, decided_at = now(), rework_reason = $4
WHERE project_id = $1 AND decision = 'pending'
RETURNING id`, projectID, clientID, decision, reason).Scan(&deliveryID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GeneralProject{}, ErrNoPendingDelivery
		}
		return GeneralProject{}, fmt.Errorf("decide delivery: %w", err)
	}

	event := "project.general_rework"
	if accept {
		// Принято — проект закрыт. Другого конца у общего проекта нет.
		if _, err := tx.Exec(ctx,
			`UPDATE projects SET status = 'done', completed_at = now(), updated_at = now()
			 WHERE id = $1`, projectID); err != nil {
			return GeneralProject{}, fmt.Errorf("close project: %w", err)
		}
		event = "project.general_accepted"
	} else if _, err := tx.Exec(ctx,
		`UPDATE projects SET revisions_used = revisions_used + 1, updated_at = now()
		 WHERE id = $1`, projectID); err != nil {
		return GeneralProject{}, fmt.Errorf("bump revisions: %w", err)
	}

	if err := r.emitGeneral(ctx, tx, projectID, clientID, event, map[string]any{
		"delivery_id": deliveryID.String(),
		"reason":      reason,
	}); err != nil {
		return GeneralProject{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return GeneralProject{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetGeneral(ctx, projectID, clientID)
}

// CancelGeneral — клиент отменяет проект.
//
// Отменить проект со сданной и неразобранной работой нельзя: исполнитель
// её уже сделал, и «отменено» вместо ответа — это способ не платить.
// Сначала принять или вернуть на доработку.
func (r *Repo) CancelGeneral(ctx context.Context, projectID, clientID uuid.UUID, reason string) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx, `
SELECT status::text FROM projects
WHERE id = $1 AND kind = 'general' AND client_user_id = $2
FOR UPDATE`, projectID, clientID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock project: %w", err)
	}
	if status != "active" {
		return ErrProjectClosed
	}

	var pending bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM general_deliveries
               WHERE project_id = $1 AND decision = 'pending')`, projectID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending delivery: %w", err)
	}
	if pending {
		return ErrDeliveryPending
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET status = 'cancelled', updated_at = now() WHERE id = $1`,
		projectID); err != nil {
		return fmt.Errorf("cancel project: %w", err)
	}
	if err := r.emitGeneral(ctx, tx, projectID, clientID, "project.general_cancelled",
		map[string]any{"reason": reason}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// emitGeneral — событие общего проекта в outbox. Обогащение общее с
// остальными project.*: n8n собирает из него текст уведомления, и поля
// «кто клиент, как называется проект» ему нужны те же.
func (r *Repo) emitGeneral(
	ctx context.Context, tx pgx.Tx, projectID, actorID uuid.UUID,
	event string, payload map[string]any,
) error {
	payload["project_id"] = projectID.String()
	enrichProjectPayload(ctx, tx, projectID, payload)
	return emit(ctx, tx, projectID, nil, actorID, event, payload)
}
