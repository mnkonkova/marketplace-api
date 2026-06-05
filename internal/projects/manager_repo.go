package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrAlreadyClaimed — POST /claim на проекте, у которого уже есть
	// assigned_to. Возвращаем 409 — другой менеджер успел раньше.
	ErrAlreadyClaimed = errors.New("project is already claimed")
	// ErrStageBlocked — advance_stage упёрся в незавершённый client-шаг.
	// Соответствует 409 в брифе §4.3.
	ErrStageBlocked = errors.New("stage cannot advance: pending client step")
	// ErrLastStage — попытка advance с последней стадии. На фронте отдадим
	// 409 и тост «проект уже на последней стадии».
	ErrLastStage = errors.New("no next stage to advance to")
	// ErrConflict — patch project прислал устаревший updated_at.
	ErrConflict = errors.New("project updated_at mismatch")
)

// ListInbox — проекты без ответственного (assigned_to_user_id IS NULL),
// для inbox-страницы менеджера. Сортировка — старые сверху, чтобы никого
// не «забыли». cancelled исключаем: админ может cancel неприсвоенный проект
// (CancelProject не требует assigned_to), и без фильтра он бы навсегда висел
// в инбоксе.
//
// P1: LIMIT 500. projects_unassigned_idx (00010) поддерживает фильтр.
func (r *Repo) ListInbox(ctx context.Context) ([]Project, error) {
	return r.listAssignedFilter(ctx,
		"WHERE assigned_to_user_id IS NULL AND status <> 'cancelled' ORDER BY created_at ASC LIMIT 500")
}

// ListAssignedTo — проекты конкретного менеджера для канбана. Берём
// только не-терминальные (draft/active/on_hold/dispute) — done/cancelled
// в канбане не нужны.
//
// P1: жёсткий LIMIT 500. До этого выгружали весь набор — у менеджера с
// 10k проектов = мегабайтный JSON + RAM-удар. 500 покрывает любой
// реалистичный канбан (5 стадий × 100 карточек); если когда-нибудь
// упрёмся — добавим keyset-пагинацию через updated_at cursor.
// Composite index projects_assigned_updated_idx (00019) убирает Sort.
func (r *Repo) ListAssignedTo(ctx context.Context, managerID uuid.UUID) ([]Project, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, lead_id, lead_recipient_specialist_id, client_user_id,
       COALESCE(client_name,''), COALESCE(client_contact,''),
       specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects
WHERE assigned_to_user_id = $1
  AND status IN ('draft','active','on_hold','dispute')
ORDER BY updated_at DESC
LIMIT 500`, managerID)
	if err != nil {
		return nil, fmt.Errorf("list assigned projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

// ListBySpecialist — проекты, в которых данный пользователь — назначенный
// специалист (read-only вкладка в специалистском кабинете). Не зависит от
// assigned_to (менеджер) — это контракт «кому делаешь работу». cancelled
// прячем — симметрично клиенту, у которого тоже не показываем отменённые.
func (r *Repo) ListBySpecialist(ctx context.Context, specialistID uuid.UUID) ([]Project, error) {
	// P1: LIMIT 500 (см. ListAssignedTo); composite index
	// projects_specialist_updated_idx (00019) поддерживает ORDER BY.
	rows, err := r.db.Query(ctx, `
SELECT id, lead_id, lead_recipient_specialist_id, client_user_id,
       COALESCE(client_name,''), COALESCE(client_contact,''),
       specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects
WHERE specialist_user_id = $1 AND status <> 'cancelled'
ORDER BY updated_at DESC
LIMIT 500`, specialistID)
	if err != nil {
		return nil, fmt.Errorf("list specialist projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

// ListAll — все проекты (для админского обзора + канбана). По умолчанию
// прячем cancelled — у них своя retention 30 дней до физического удаления,
// в админке они только засоряют список. Чтобы посмотреть скрытые —
// явно указать statusFilter='cancelled'.
//
// P1: hard cap LIMIT 1000 (адмиский набор шире менеджерского, но
// тоже не бесконечный). При росте проекта понадобится пагинация.
func (r *Repo) ListAll(ctx context.Context, statusFilter string) ([]Project, error) {
	q := `
SELECT id, lead_id, lead_recipient_specialist_id, client_user_id,
       COALESCE(client_name,''), COALESCE(client_contact,''),
       specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects`
	args := []any{}
	if statusFilter != "" {
		q += " WHERE status = $1"
		args = append(args, statusFilter)
	} else {
		q += " WHERE status <> 'cancelled'"
	}
	q += " ORDER BY updated_at DESC LIMIT 1000"
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list all projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

func (r *Repo) listAssignedFilter(ctx context.Context, suffix string) ([]Project, error) {
	q := `
SELECT id, lead_id, lead_recipient_specialist_id, client_user_id,
       COALESCE(client_name,''), COALESCE(client_contact,''),
       specialist_user_id, assigned_to_user_id,
       pipeline_id, title, source, status, revisions_included, revisions_used, budget,
       COALESCE(notes,''), started_at, completed_at, created_at, updated_at
FROM projects ` + suffix
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

func scanProjects(rows pgx.Rows) ([]Project, error) {
	out := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(
			&p.ID, &p.LeadID, &p.LeadRecipientSpecialistID, &p.ClientUserID,
			&p.ClientName, &p.ClientContact,
			&p.SpecialistUserID, &p.AssignedToUserID,
			&p.PipelineID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed, &p.Budget,
			&p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Claim — атомарно присвоить assigned_to_user_id, если ещё NULL. Иначе
// ErrAlreadyClaimed. Idempotent для самого менеджера (если уже его — ОК).
// AssignManager — присвоить проекту менеджера или снять (managerID=nil).
// Без проверки is-claimed (в отличие от Claim) — это админская операция.
// Эмитит event 'assigned' либо 'unassigned' для ленты активности.
func (r *Repo) AssignManager(ctx context.Context, projectID uuid.UUID, managerID *uuid.UUID, actorID uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE projects SET assigned_to_user_id = $2, updated_at = now() WHERE id = $1`,
		projectID, managerID)
	if err != nil {
		return fmt.Errorf("assign: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	eventKind := "assigned"
	payload := map[string]string{}
	if managerID != nil {
		payload["manager_user_id"] = managerID.String()
	} else {
		eventKind = "unassigned"
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', $3, $4)`,
		projectID, actorID, eventKind, mustJSON(payload)); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	outboxPayload := map[string]any{"project_id": projectID.String()}
	if managerID != nil {
		outboxPayload["manager_user_id"] = managerID.String()
		// У менеджеров нет profile-таблицы — берём users.email и кладём
		// часть до '@' как display_name (для Telegram-нотификации).
		var email string
		if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, *managerID).Scan(&email); err == nil && email != "" {
			outboxPayload["manager_email"] = email
			if at := strings.IndexByte(email, '@'); at > 0 {
				outboxPayload["manager_display_name"] = email[:at]
			} else {
				outboxPayload["manager_display_name"] = email
			}
		}
	}
	enrichProjectPayload(ctx, tx, projectID, outboxPayload)
	if err := emit(ctx, tx, projectID, nil, actorID, "project."+eventKind, outboxPayload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrNoProposedSpecialist — попытка аппрувнуть/реджектнуть, когда у проекта
// нет lead_recipient_specialist_id (клиент не выбирал или менеджер уже снял).
var ErrNoProposedSpecialist = errors.New("no proposed specialist on project")

// CancelProject — soft-delete: status='cancelled'. Из админского списка
// сразу исчезает (ListAll фильтрует), но запись физически удалится только
// через retention (по умолчанию 30 дней, см. CleanupOldCompletedProjects).
// Идемпотентно: если проект уже cancelled — возвращаем nil.
func (r *Repo) CancelProject(ctx context.Context, projectID, actorID uuid.UUID, reason string) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus ProjectStatus
	if err := tx.QueryRow(ctx,
		`SELECT status FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&currentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("load status: %w", err)
	}
	if currentStatus == ProjectStatusCancelled {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE projects SET status='cancelled', updated_at=now() WHERE id=$1`,
		projectID); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	// from_status/to_status — step_status enum, project_status туда не лезет
	// (см. миграцию 00010). Статус-переход проекта кладём в payload, в самих
	// колонках оставляем NULL.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, comment, payload)
VALUES ($1, NULL, $2, 'human', 'project_cancelled', $3, $4)`,
		projectID, actorID, reason,
		mustJSON(map[string]string{
			"reason":      reason,
			"from_status": string(currentStatus),
			"to_status":   "cancelled",
		})); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.cancelled",
		map[string]string{
			"project_id":  projectID.String(),
			"from_status": string(currentStatus),
			"reason":      reason,
		}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ApproveProposedSpecialist — копирует lead_recipient_specialist_id в
// specialist_user_id (фиксирует выбор клиента). После этого специалист
// показывается в блоке «Контакты» и получает доступ к проекту через
// `/me/specialist/projects`. Idempotent если specialist уже совпадает.
func (r *Repo) ApproveProposedSpecialist(ctx context.Context, projectID, actorID uuid.UUID) (uuid.UUID, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Берём lead_id заодно с proposed: понадобится ниже для авто-«согласия»
	// спеца на лид (см. autoAcceptLeadRecipient). leadID может быть NULL —
	// если проект создан вручную менеджером, без брифа.
	var leadID, proposed *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT lead_id, lead_recipient_specialist_id FROM projects WHERE id=$1 FOR UPDATE`,
		projectID).Scan(&leadID, &proposed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("load project: %w", err)
	}
	if proposed == nil {
		return uuid.Nil, ErrNoProposedSpecialist
	}
	if _, err := tx.Exec(ctx,
		`UPDATE projects SET specialist_user_id=$2, updated_at=now() WHERE id=$1`,
		projectID, *proposed); err != nil {
		return uuid.Nil, fmt.Errorf("set specialist: %w", err)
	}
	// Менеджер согласовал спеца → автоматически проставляем lead_recipients.status='accepted'.
	// Раньше спец должен был сам ткнуть «принять» в инбоксе лидов, иначе клиент не мог
	// оставить отзыв (reviews.LeadAuthorizesReview, см. internal/reviews/repo.go).
	if err := autoAcceptLeadRecipient(ctx, tx, leadID, *proposed); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'specialist_approved', $3)`,
		projectID, actorID,
		mustJSON(map[string]string{"specialist_user_id": proposed.String()})); err != nil {
		return uuid.Nil, fmt.Errorf("event: %w", err)
	}
	apayload := map[string]any{
		"project_id":         projectID.String(),
		"specialist_user_id": proposed.String(),
	}
	enrichProjectPayload(ctx, tx, projectID, apayload)
	if err := emit(ctx, tx, projectID, nil, actorID, "project.specialist_approved",
		apayload); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return *proposed, nil
}

// AssignSpecialist — назначает конкретного спеца на проект напрямую,
// минуя proposed-flow. Используется в трёх кейсах:
//  1) проект создан вручную менеджером без spec'a (Phase 1 — no-account);
//  2) изначальный proposed-спец был отклонён, менеджер выбрал другого;
//  3) админ переназначает спеца в спорной ситуации.
//
// Валидация: целевой юзер должен быть kind='specialist' и активен.
// step_event/outbox events те же что у ApproveProposedSpecialist, но с
// event_kind='specialist_assigned' — чтобы аналитика различала «клиент
// предложил, мы согласились» от «менеджер назначил сам».
func (r *Repo) AssignSpecialist(ctx context.Context, projectID, actorID, specialistID uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Проверяем что spec существует и валидный.
	var kind string
	var active bool
	if err := tx.QueryRow(ctx,
		`SELECT kind, is_active FROM users WHERE id=$1`,
		specialistID).Scan(&kind, &active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: специалист не найден", ErrInvalidInput)
		}
		return fmt.Errorf("load specialist: %w", err)
	}
	if kind != "specialist" {
		return fmt.Errorf("%w: выбранный юзер не специалист (kind=%s)", ErrInvalidInput, kind)
	}
	if !active {
		return fmt.Errorf("%w: специалист деактивирован", ErrInvalidInput)
	}

	// Берём lock и заодно lead_id — он нужен ниже, чтобы автоматически
	// зачесть назначение как согласие спеца на лид (см. autoAcceptLeadRecipient).
	// При свободном назначении (без брифа) lead_id будет NULL — это ок,
	// helper в таком случае no-op.
	var leadID *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT lead_id FROM projects WHERE id=$1 FOR UPDATE`,
		projectID).Scan(&leadID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("load project: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET specialist_user_id=$2, updated_at=now() WHERE id=$1`,
		projectID, specialistID); err != nil {
		return fmt.Errorf("set specialist: %w", err)
	}
	// Назначение менеджером засчитываем как согласие спеца — иначе клиент
	// не сможет оставить отзыв (reviews.LeadAuthorizesReview). Если менеджер
	// назначил спеца, которого не было в исходных получателях лида,
	// helper сам вставит для него запись lead_recipients со status='accepted'.
	if err := autoAcceptLeadRecipient(ctx, tx, leadID, specialistID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'specialist_assigned', $3)`,
		projectID, actorID,
		mustJSON(map[string]string{"specialist_user_id": specialistID.String()})); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	apayload := map[string]any{
		"project_id":         projectID.String(),
		"specialist_user_id": specialistID.String(),
	}
	enrichProjectPayload(ctx, tx, projectID, apayload)
	if err := emit(ctx, tx, projectID, nil, actorID, "project.specialist_assigned",
		apayload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// autoAcceptLeadRecipient — отмечает специалиста как «принявшего» лид в
// момент, когда менеджер сам подтвердил/назначил его на проект
// (ApproveProposedSpecialist / AssignSpecialist).
//
// Зачем: без этого в lead_recipients статус остаётся 'sent', и клиент не
// может оставить отзыв — reviews.LeadAuthorizesReview требует строго
// status='accepted' (см. internal/reviews/repo.go). Семантика: согласование
// менеджером => дефолтное согласие спеца. Спецу больше не надо самому
// тыкать «принять» в инбоксе лидов после ручного назначения.
//
// Кейсы:
//  1) Спец был в исходном списке получателей (proposed-flow) — UPDATE
//     существующей строки lead_recipients.
//  2) Менеджер назначил спеца, не входившего в получателей лида
//     (свободное назначение через AssignSpecialist) — INSERT новой
//     строки, чтобы review-check потом нашёл связку (lead, spec).
//  3) leadID == nil — проект создан вручную, без лида — no-op.
//
// responded_at сохраняется при апдейте (через COALESCE), чтобы не
// затирать реальное время ответа спеца, если он успел отреагировать.
func autoAcceptLeadRecipient(ctx context.Context, tx pgx.Tx, leadID *uuid.UUID, specialistID uuid.UUID) error {
	if leadID == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO lead_recipients (lead_id, specialist_user_id, status, responded_at)
VALUES ($1, $2, 'accepted', now())
ON CONFLICT (lead_id, specialist_user_id) DO UPDATE
SET status       = 'accepted',
    responded_at = COALESCE(lead_recipients.responded_at, EXCLUDED.responded_at),
    updated_at   = now()`,
		*leadID, specialistID); err != nil {
		return fmt.Errorf("auto-accept lead recipient: %w", err)
	}
	return nil
}

// RejectProposedSpecialist — снимает «предложение». specialist_user_id не
// трогаем (он и так NULL — иначе спец был бы уже подтверждён). Менеджер
// после реджекта подберёт другого спеца сам (через AssignSpecialist).
func (r *Repo) RejectProposedSpecialist(ctx context.Context, projectID, actorID uuid.UUID, reason string) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var proposed *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT lead_recipient_specialist_id FROM projects WHERE id=$1 FOR UPDATE`,
		projectID).Scan(&proposed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("load: %w", err)
	}
	if proposed == nil {
		return ErrNoProposedSpecialist
	}
	if _, err := tx.Exec(ctx,
		`UPDATE projects SET lead_recipient_specialist_id=NULL, updated_at=now() WHERE id=$1`,
		projectID); err != nil {
		return fmt.Errorf("clear proposed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, comment, payload)
VALUES ($1, NULL, $2, 'human', 'specialist_rejected', $3, $4)`,
		projectID, actorID, reason,
		mustJSON(map[string]string{"specialist_user_id": proposed.String()})); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.specialist_rejected",
		map[string]string{
			"project_id":         projectID.String(),
			"specialist_user_id": proposed.String(),
		}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) Claim(ctx context.Context, projectID, managerID uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
UPDATE projects SET assigned_to_user_id = $2, updated_at = now()
WHERE id = $1 AND (assigned_to_user_id IS NULL OR assigned_to_user_id = $2)`,
		projectID, managerID)
	if err != nil {
		return fmt.Errorf("claim project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// R2: probe-SELECT с FOR UPDATE на конкретной строке. Раньше probe
		// шёл без лока и между UPDATE-fail и probe админ мог сделать
		// unassign → existsTaken=false → возвращали ErrNotFound, хотя проект
		// существует и теперь свободен. С локом probe видит стабильный
		// снэпшот и одновременная мутация уже невозможна.
		// FOR UPDATE нельзя засунуть в SELECT EXISTS — берём прямой SELECT.
		var assignedTo *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT assigned_to_user_id FROM projects WHERE id = $1 FOR UPDATE`,
			projectID).Scan(&assignedTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("probe project: %w", err)
		}
		if assignedTo != nil && *assignedTo != managerID {
			return ErrAlreadyClaimed
		}
		// Проект свободен (NULL) или уже наш — идемпотентно достраиваем
		// до claimed-состояния. Без этого вернули бы ErrNotFound в гонке.
		if _, err := tx.Exec(ctx,
			`UPDATE projects SET assigned_to_user_id = $2, updated_at = now() WHERE id = $1`,
			projectID, managerID); err != nil {
			return fmt.Errorf("retry claim: %w", err)
		}
	}

	// Событие assigned для ленты активности.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'assigned', $3)`,
		projectID, managerID,
		mustJSON(map[string]string{"manager_user_id": managerID.String()})); err != nil {
		return fmt.Errorf("insert assigned event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, managerID, "project.assigned",
		map[string]string{"project_id": projectID.String(), "manager_user_id": managerID.String()},
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PatchProject — title/budget/notes c optimistic-lock. Bool+value тот же
// паттерн что в profiles, чтобы можно было занулять.
//
// R5: UPDATE и probe-SELECT (NotFound vs Conflict) идут в ОДНОЙ
// транзакции — раньше probe выполнялся через r.db (другой коннект
// пула), и между UPDATE-fail и probe проект могли удалить → отдавали
// ErrConflict вместо ErrNotFound. Внутри одной tx видим консистентный
// снимок.
func (r *Repo) PatchProject(ctx context.Context, projectID uuid.UUID, in ManagerPatchInput) (Project, error) {
	const q = `
UPDATE projects SET
  title      = COALESCE($2, title),
  budget     = CASE WHEN $3::boolean THEN $4 ELSE budget END,
  notes      = COALESCE($5, notes),
  updated_at = now()
WHERE id = $1 AND ($6::timestamptz IS NULL OR updated_at = $6)
RETURNING id, lead_id, lead_recipient_specialist_id, client_user_id,
          COALESCE(client_name,''), COALESCE(client_contact,''),
          specialist_user_id, assigned_to_user_id,
          pipeline_id, title, source, status, revisions_included, revisions_used, budget,
          COALESCE(notes,''), started_at, completed_at, created_at, updated_at`
	var trimmedTitle, trimmedNotes *string
	if in.Title != nil {
		v := strings.TrimSpace(*in.Title)
		trimmedTitle = &v
	}
	if in.Notes != nil {
		v := strings.TrimSpace(*in.Notes)
		trimmedNotes = &v
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Project{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var p Project
	err = tx.QueryRow(ctx, q,
		projectID,
		trimmedTitle,
		in.Budget != nil, in.Budget,
		trimmedNotes,
		in.UpdatedAt,
	).Scan(
		&p.ID, &p.LeadID, &p.LeadRecipientSpecialistID, &p.ClientUserID,
		&p.ClientName, &p.ClientContact,
		&p.SpecialistUserID, &p.AssignedToUserID,
		&p.PipelineID, &p.Title, &p.Source, &p.Status, &p.RevisionsIncluded, &p.RevisionsUsed, &p.Budget,
		&p.Notes, &p.StartedAt, &p.CompletedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// либо нет, либо updated_at не совпал — probe в той же tx.
		if in.UpdatedAt != nil {
			var exists bool
			if perr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)`,
				projectID).Scan(&exists); perr == nil && exists {
				return Project{}, ErrConflict
			}
		}
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("patch project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return p, nil
}

// stageInfo — текущая стадия проекта (минимум для канбана и advance_stage).
type stageInfo struct {
	ID         uuid.UUID
	Name       string
	SortOrder  int
	StartedAt  *time.Time
	Completed  bool
}

// CurrentAndNextStage — текущая активная стадия (первая с шагами не в
// (done|skipped)) и следующая. Если current=nil — все шаги завершены
// (проект готов к закрытию). Используется в канбане и в advance_stage.
func (r *Repo) CurrentAndNextStage(ctx context.Context, projectID uuid.UUID) (current, next *stageInfo, err error) {
	rows, err := r.db.Query(ctx, `
SELECT st.id, st.name, st.sort_order, st.started_at,
       BOOL_AND(ps.status IN ('done','skipped')) FILTER (WHERE ps.id IS NOT NULL) AS all_done
FROM project_stages st
LEFT JOIN project_steps ps ON ps.stage_id = st.id
WHERE st.project_id = $1
GROUP BY st.id, st.name, st.sort_order, st.started_at
ORDER BY st.sort_order`, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("load stages with status: %w", err)
	}
	defer rows.Close()
	var stages []stageInfo
	for rows.Next() {
		var s stageInfo
		var allDone *bool
		if err := rows.Scan(&s.ID, &s.Name, &s.SortOrder, &s.StartedAt, &allDone); err != nil {
			return nil, nil, fmt.Errorf("scan stage: %w", err)
		}
		// Стадия без шагов — считаем not_done (защита от пустых).
		if allDone != nil && *allDone {
			s.Completed = true
		}
		stages = append(stages, s)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range stages {
		if !stages[i].Completed {
			c := stages[i]
			current = &c
			if i+1 < len(stages) {
				n := stages[i+1]
				next = &n
			}
			return
		}
	}
	// все стадии завершены → current=nil
	return nil, nil, nil
}

// AdvanceStage — переход на следующую стадию. Бизнес-логика (бриф §4.3):
//   1. Если в текущей стадии есть незавершённый client-шаг → ErrStageBlocked.
//   2. Все pending/in_progress team-шаги текущей стадии → done.
//   3. Текущая стадия completed_at = now.
//   4. Первый шаг следующей стадии активируется:
//      owner=team/system → in_progress; owner=client → waiting_client.
//      Для is_review+client дополнительно ставится review_deadline.
//   5. Следующая стадия started_at = now.
//   6. Event stage_advance + outbox.
// Если следующей стадии нет — ErrLastStage.
//
// reviewDeadline — длительность дедлайна для review-шага (передаётся
// сервисом, см. Service.reviewDeadline). 0 = не ставить.
// expectedUpdatedAt — optimistic-lock версии projects.updated_at; nil = без
// проверки. Если задан и не совпадает — ErrConflict (защищает от двух
// менеджеров двигающих стадию одного проекта одновременно).
func (r *Repo) AdvanceStage(ctx context.Context, projectID, actorID uuid.UUID, reviewDeadline time.Duration, expectedUpdatedAt *time.Time) (Project, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Project{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Используем CurrentAndNext через ту же таблицу — но в транзакции,
	// чтобы не было гонки. Перечитываем стадии под FOR UPDATE.
	rows, err := tx.Query(ctx, `
SELECT st.id, st.sort_order
FROM project_stages st
WHERE st.project_id = $1
ORDER BY st.sort_order
FOR UPDATE`, projectID)
	if err != nil {
		return Project{}, fmt.Errorf("lock stages: %w", err)
	}
	type stRow struct {
		ID    uuid.UUID
		Order int
	}
	var stages []stRow
	for rows.Next() {
		var s stRow
		if err := rows.Scan(&s.ID, &s.Order); err != nil {
			rows.Close()
			return Project{}, fmt.Errorf("scan stage row: %w", err)
		}
		stages = append(stages, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Project{}, err
	}
	if len(stages) == 0 {
		return Project{}, ErrNotFound
	}

	// 1. Найти первую стадию, где НЕ все шаги done/skipped.
	//
	// P2: было N запросов BOOL_AND по каждой стадии под FOR UPDATE — каждая
	// округлая трип держала лок дольше. Заменили на один GROUP BY: сразу
	// получаем все стадии со статусом done/incomplete и проходим в Go.
	// COALESCE(BOOL_AND, TRUE): стадия без шагов считается done (нечего ждать).
	stageIDs := make([]uuid.UUID, 0, len(stages))
	for _, st := range stages {
		stageIDs = append(stageIDs, st.ID)
	}
	stageDoneRows, err := tx.Query(ctx, `
SELECT s.id, COALESCE(BOOL_AND(ps.status IN ('done','skipped')), TRUE)
FROM project_stages s
LEFT JOIN project_steps ps ON ps.stage_id = s.id
WHERE s.id = ANY($1)
GROUP BY s.id`, stageIDs)
	if err != nil {
		return Project{}, fmt.Errorf("check stages done: %w", err)
	}
	allDoneByID := make(map[uuid.UUID]bool, len(stages))
	for stageDoneRows.Next() {
		var sid uuid.UUID
		var done bool
		if err := stageDoneRows.Scan(&sid, &done); err != nil {
			stageDoneRows.Close()
			return Project{}, fmt.Errorf("scan stage done: %w", err)
		}
		allDoneByID[sid] = done
	}
	stageDoneRows.Close()
	if err := stageDoneRows.Err(); err != nil {
		return Project{}, err
	}

	var currentIdx = -1
	for i, st := range stages {
		if !allDoneByID[st.ID] {
			currentIdx = i
			break
		}
	}
	if currentIdx == -1 {
		return Project{}, ErrLastStage
	}
	current := stages[currentIdx]
	if currentIdx+1 >= len(stages) {
		return Project{}, ErrLastStage
	}
	next := stages[currentIdx+1]

	// 2. Проверка: в текущей стадии нет client-шага в pending/in_progress/waiting_client.
	var hasClientPending bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM project_steps
              WHERE stage_id = $1 AND owner = 'client'
                AND status IN ('pending','in_progress','waiting_client','rejected'))`,
		current.ID).Scan(&hasClientPending); err != nil {
		return Project{}, fmt.Errorf("check client pending: %w", err)
	}
	if hasClientPending {
		return Project{}, ErrStageBlocked
	}

	// 3. Все team/system pending|in_progress|rejected текущей стадии → done.
	if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status = 'done', completed_at = COALESCE(completed_at, now()), updated_at = now()
WHERE stage_id = $1 AND owner IN ('team','system')
  AND status IN ('pending','in_progress','rejected')`,
		current.ID); err != nil {
		return Project{}, fmt.Errorf("complete team steps: %w", err)
	}

	// 4. completed_at у текущей стадии.
	if _, err := tx.Exec(ctx,
		`UPDATE project_stages SET completed_at = COALESCE(completed_at, now())
		 WHERE id = $1`, current.ID); err != nil {
		return Project{}, fmt.Errorf("complete stage: %w", err)
	}

	// 5. Активация первого шага следующей стадии (по sort_order).
	var firstStepID uuid.UUID
	var firstStepOwner string
	var firstStepIsReview bool
	err = tx.QueryRow(ctx, `
SELECT id, owner, is_review FROM project_steps
WHERE stage_id = $1 ORDER BY sort_order LIMIT 1`, next.ID).Scan(&firstStepID, &firstStepOwner, &firstStepIsReview)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Project{}, fmt.Errorf("first step of next stage: %w", err)
	}
	if err == nil {
		newStatus := StepStatusInProgress
		if firstStepOwner == OwnerClient {
			newStatus = StepStatusWaitingClient
		}
		var deadline *time.Time
		if firstStepIsReview && newStatus == StepStatusWaitingClient && reviewDeadline > 0 {
			d := time.Now().Add(reviewDeadline)
			deadline = &d
		}
		if _, err := tx.Exec(ctx, `
UPDATE project_steps
SET status = $2,
    started_at = COALESCE(started_at, now()),
    review_deadline = COALESCE($3, review_deadline),
    updated_at = now()
WHERE id = $1 AND status = 'pending'`, firstStepID, string(newStatus), deadline); err != nil {
			return Project{}, fmt.Errorf("activate next step: %w", err)
		}
	}

	// 6. started_at у новой стадии.
	if _, err := tx.Exec(ctx,
		`UPDATE project_stages SET started_at = COALESCE(started_at, now())
		 WHERE id = $1`, next.ID); err != nil {
		return Project{}, fmt.Errorf("start next stage: %w", err)
	}

	// 7. Бамп projects.updated_at с optimistic-lock-проверкой.
	tag, err := tx.Exec(ctx,
		`UPDATE projects SET updated_at = now() WHERE id = $1
		   AND ($2::timestamptz IS NULL OR updated_at = $2)`,
		projectID, expectedUpdatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("bump project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Либо проект исчез (вряд ли — он был в FOR UPDATE через stages),
		// либо updated_at не совпал с ожидаемым.
		return Project{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, payload)
VALUES ($1, NULL, $2, 'human', 'stage_advance', $3)`,
		projectID, actorID,
		mustJSON(map[string]string{"from_stage_id": current.ID.String(), "to_stage_id": next.ID.String()})); err != nil {
		return Project{}, fmt.Errorf("insert stage_advance event: %w", err)
	}
	if err := emit(ctx, tx, projectID, nil, actorID, "project.stage_advanced",
		map[string]string{
			"project_id":    projectID.String(),
			"from_stage_id": current.ID.String(),
			"to_stage_id":   next.ID.String(),
		},
	); err != nil {
		return Project{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetByID(ctx, projectID)
}

// GetByIDForManager — проект, видимый менеджеру:
//   - assigned_to_user_id = me            — свои проекты (полный доступ);
//   - assigned_to_user_id IS NULL         — «общий» инбокс (можно взять на себя);
//   - assigned_to_user_id = другой        — НЕ виден (ErrNotFound).
//
// Чужие assigned-проекты прятаны, чтобы менеджер не лез в чужую работу.
// Если managerID=uuid.Nil — отдаём любой проект (для admin-вызовов).
func (r *Repo) GetByIDForManager(ctx context.Context, projectID, managerID uuid.UUID) (Project, error) {
	if managerID == uuid.Nil {
		return r.GetByID(ctx, projectID)
	}
	p, err := r.GetByID(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	// Своё — ок.
	if p.AssignedToUserID != nil && *p.AssignedToUserID == managerID {
		return p, nil
	}
	// Unassigned — видно всем менеджерам (для claim'а).
	if p.AssignedToUserID == nil {
		return p, nil
	}
	// Чужой assigned — прячем за not_found (не выдаём факт существования).
	return Project{}, ErrNotFound
}

// MoveProjectToStage — фактическая реализация лежит в move_stage.go ради
// читаемости (отдельная история, отличная от advance_stage).

// LoadClientDisplayNames — батч display_name для списка projects. Возвращает
// map user_id → display_name с тем же фолбэк-ладдером, что и в комментариях:
// specialist_profiles → client_profiles → префикс email. Где никаких источников
// нет — пустая строка.
func (r *Repo) LoadClientDisplayNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT u.id, COALESCE(
  NULLIF(sp.display_name, ''),
  NULLIF(cp.display_name, ''),
  split_part(u.email, '@', 1),
  ''
)
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load display names: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]string, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// LoadPartyContacts — батч контактов (display_name + email + phone + telegram)
// для участников проектов. Возвращает map user_id → PartyContact. Контакты
// клиента берутся из client_profiles, специалиста — из specialist_profiles
// (contact_email/contact_phone — заполняется самим спецом в кабинете).
func (r *Repo) LoadPartyContacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PartyContact, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]PartyContact{}, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT u.id,
       COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       ) AS display_name,
       COALESCE(u.email, '')        AS email,
       COALESCE(
         NULLIF(sp.contact_phone, ''),
         NULLIF(cp.phone, ''),
         COALESCE(u.phone, ''),
         ''
       ) AS phone,
       COALESCE(NULLIF(cp.telegram, ''), '') AS telegram
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load party contacts: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]PartyContact, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var c PartyContact
		if err := rows.Scan(&id, &c.DisplayName, &c.Email, &c.Phone, &c.Telegram); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}
