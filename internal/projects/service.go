package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrForbidden         = errors.New("forbidden")
	ErrRecipientNotReady = errors.New("recipient not accepted")
	ErrAlreadyStarted    = errors.New("project for this recipient already exists")
)

// UserDirectory — узкий контракт к users-таблице. service-слой
// денормализует email/name в outbox payload (чтобы n8n не делал
// обратных запросов); этот интерфейс изолирует зависимость от auth.Repo.
type UserDirectory interface {
	GetEmailAndName(ctx context.Context, userID uuid.UUID) (email, name string, err error)
}

// RecipientAcceptor — узкий контракт для «менеджер апрувит recipient'a
// за продакшен» через POST /admin/lead_recipients/{id}/accept.
// Адаптер в main.go обращается к leads.Service.UpdateRecipientStatus.
type RecipientAcceptor interface {
	AcceptRecipient(ctx context.Context, leadID, specialistID uuid.UUID) error
}

type Service struct {
	repo       *Repo
	users      UserDirectory
	reviews    ReviewWriter
	recipients RecipientAcceptor
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// WithUserDirectory подключает источник денормализованных полей
// (email, display_name) для outbox payload. nil-safe: без него
// в payload пустые строки, n8n не сможет роутить по email.
func (s *Service) WithUserDirectory(d UserDirectory) *Service {
	s.users = d
	return s
}

// WithRecipientAcceptor подключает мост к leads-домену для
// /admin/lead_recipients/.../accept. nil → endpoint вернёт 503.
func (s *Service) WithRecipientAcceptor(r RecipientAcceptor) *Service {
	s.recipients = r
	return s
}

// ─── Read views (Фаза 2 — для клиента и admin) ──────────────────────

// FunnelView — funnel-дерево для одного проекта: список стадий, каждая
// со своим набором шагов (отфильтрованных по аудитории на стороне БД).
// Поле Progress — расчёт по всем видимым шагам (см. CalcProgress).
type FunnelView struct {
	Stages   []StageView  `json:"stages"`
	Progress ProgressView `json:"progress"`
}

// GetClientFunnel собирает funnel-дерево для клиента. Прогресс считается
// по тем шагам, которые он видит (visible_to_client=TRUE). Если клиент
// не владелец — ErrNotFound (защита от перебора UUID).
func (s *Service) GetClientFunnel(ctx context.Context, projectID, userID uuid.UUID) (FunnelView, error) {
	// Проверка владения через GetClientByID — он сам делает фильтр по
	// client_user_id и возвращает ErrNotFound иначе.
	if _, err := s.repo.GetClientByID(ctx, projectID, userID); err != nil {
		return FunnelView{}, err
	}
	return s.buildFunnel(ctx, projectID, VisibilityForClient)
}

// GetSpecialistFunnel — аналог для назначенного специалиста.
func (s *Service) GetSpecialistFunnel(ctx context.Context, projectID, userID uuid.UUID) (FunnelView, error) {
	if _, err := s.repo.GetSpecialistByID(ctx, projectID, userID); err != nil {
		return FunnelView{}, err
	}
	return s.buildFunnel(ctx, projectID, VisibilityForSpecialist)
}

// GetAdminFunnel — все шаги, без фильтра. Доступно только admin/moderator
// (RequireRoles на роутере).
func (s *Service) GetAdminFunnel(ctx context.Context, projectID uuid.UUID) (FunnelView, error) {
	return s.buildFunnel(ctx, projectID, VisibilityNoFilter)
}

func (s *Service) buildFunnel(ctx context.Context, projectID uuid.UUID, vis VisibilityFilter) (FunnelView, error) {
	stages, err := s.repo.ListStages(ctx, projectID)
	if err != nil {
		return FunnelView{}, err
	}
	steps, err := s.repo.ListStepsWithStage(ctx, projectID, vis)
	if err != nil {
		return FunnelView{}, err
	}

	// Маршрутизация шагов по стадиям через stage_id → index в stages.
	stageIdx := make(map[uuid.UUID]int, len(stages))
	for i := range stages {
		stageIdx[stages[i].ID] = i
		stages[i].Steps = make([]StepView, 0, 4)
	}
	flat := make([]StepView, 0, len(steps))
	for _, s := range steps {
		idx, ok := stageIdx[s.StageID]
		if !ok {
			// Шаг без стадии — баг данных. Пропускаем, чтобы не упасть.
			continue
		}
		stages[idx].Steps = append(stages[idx].Steps, s.StepView)
		flat = append(flat, s.StepView)
	}
	return FunnelView{
		Stages:   stages,
		Progress: CalcProgress(flat),
	}, nil
}

// ListClientProjects — обёртка для красоты HTTP-handler'а.
func (s *Service) ListClientProjects(ctx context.Context, userID uuid.UUID) ([]ProjectClientView, error) {
	return s.repo.ListByClient(ctx, userID)
}

func (s *Service) GetClientProject(ctx context.Context, projectID, userID uuid.UUID) (ProjectClientView, error) {
	return s.repo.GetClientByID(ctx, projectID, userID)
}

func (s *Service) ListClientProjectsByLead(ctx context.Context, userID, leadID uuid.UUID) ([]ProjectClientView, error) {
	return s.repo.ListByClientByLead(ctx, userID, leadID)
}

// ListSpecialistProjects — назначенные проекты для специалиста.
func (s *Service) ListSpecialistProjects(ctx context.Context, userID uuid.UUID) ([]ProjectSpecialistView, error) {
	return s.repo.ListBySpecialist(ctx, userID)
}

// GetSpecialistProject — карточка одного назначенного проекта.
func (s *Service) GetSpecialistProject(ctx context.Context, projectID, userID uuid.UUID) (ProjectSpecialistView, error) {
	return s.repo.GetSpecialistByID(ctx, projectID, userID)
}

// ─── Admin read ────────────────────────────────────────────────────

func (s *Service) ListAdmin(ctx context.Context, f AdminListFilter) ([]ProjectAdminView, error) {
	return s.repo.ListAdmin(ctx, f)
}

func (s *Service) GetAdmin(ctx context.Context, id uuid.UUID) (ProjectAdminView, error) {
	return s.repo.GetAdminByID(ctx, id)
}

func (s *Service) ListEvents(ctx context.Context, projectID uuid.UUID, limit int) ([]StepEvent, error) {
	return s.repo.ListEvents(ctx, projectID, limit)
}

// UpdateAdmin — PATCH whitelist-полей с optimistic-lock через
// updated_at. ErrConflict при stale, ErrNotFound при отсутствии.
// Сюда же входит изменение status; валидируем через
// IsValidProjectStatusUpdate (запрещаем active→done вручную, это
// должно идти через CompleteStep на последнем шаге).
func (s *Service) UpdateAdmin(ctx context.Context, id uuid.UUID, in PatchAdminInput) (ProjectAdminView, error) {
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || len(t) > 200 {
			return ProjectAdminView{}, fmt.Errorf("%w: title 1..200", ErrInvalidInput)
		}
		in.Title = &t
	}
	if in.Notes != nil {
		n := strings.TrimSpace(*in.Notes)
		if len(n) > 5000 {
			return ProjectAdminView{}, fmt.Errorf("%w: notes ≤5000", ErrInvalidInput)
		}
		in.Notes = &n
	}

	// Если меняется status — проверяем граф (требует чтения текущего).
	if in.Status != nil {
		cur, err := s.repo.GetAdminByID(ctx, id)
		if err != nil {
			return ProjectAdminView{}, err
		}
		if !IsValidProjectStatusUpdate(cur.Status, *in.Status) {
			return ProjectAdminView{}, fmt.Errorf("%w: project status %s → %s", ErrInvalidTransition, cur.Status, *in.Status)
		}
	}

	var result ProjectAdminView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := s.repo.UpdateAdminInTx(ctx, tx, id, in)
		if err != nil {
			return err
		}
		result = p
		return nil
	})
	return result, err
}

// AcceptRecipient — обёртка над leads-доменом для «менеджер апрувит
// recipient'а за свой продакшен». Реализация в адаптере main.go.
func (s *Service) AcceptRecipient(ctx context.Context, leadID, specialistID uuid.UUID) error {
	if s.recipients == nil {
		return fmt.Errorf("%w: recipient acceptor not configured", ErrInvalidInput)
	}
	return s.recipients.AcceptRecipient(ctx, leadID, specialistID)
}

// ─── Создание проекта (write) ───────────────────────────────────────

// StartProjectManual создаёт проект «вручную» (source ∈ manual/referral/
// returning_client) с автоматическим snapshot'ом шаблона.
//
// Шаги:
//  1. Валидация: client_user_id существует и role=client; title непустой;
//     template_code/version указывают на активный шаблон.
//  2. В одной tx: INSERT projects (status=active, started_at=now()) →
//     LoadActiveTemplate → writeSnapshot (project_stages + project_steps).
//  3. emitProjectEvent EventProjectCreated.
//  4. Возвращает Admin-view созданного проекта.
//
// title trim, длина 1..200. На дубль-ресипиент сюда не попадает (это
// другой эндпоинт), поэтому unique-violation не ожидаем.
func (s *Service) StartProjectManual(ctx context.Context, in CreateManualInput, actorUserID uuid.UUID) (ProjectAdminView, error) {
	if err := s.validateCreateInput(in.Title, in.TemplateCode); err != nil {
		return ProjectAdminView{}, err
	}
	// Принудительная коррекция source: lead_id всегда nil в manual
	// (для marketplace используется StartFromRecipient).
	src := DetectSource(nil, in.Source)

	var created ProjectAdminView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		snap, err := s.loadTemplateInTx(ctx, tx, in.TemplateCode, in.TemplateVersion)
		if err != nil {
			return err
		}

		// INSERT projects → возвращаем все поля для view.
		id := uuid.New()
		now := time.Now().UTC()
		const insQ = `
INSERT INTO projects (
    id, lead_id, client_user_id, specialist_user_id, assigned_to_user_id,
    template_id, title, source, status, revisions_included,
    budget, notes, started_at
)
VALUES ($1, NULL, $2, $3, $4, $5, $6, $7, 'active', $8, $9, NULLIF($10, ''), $11)
RETURNING id, lead_id, client_user_id, specialist_user_id, assigned_to_user_id,
          template_id, title, source, status, revisions_included, revisions_used,
          budget, COALESCE(notes, ''), started_at, completed_at, created_at, updated_at`
		err = tx.QueryRow(ctx, insQ,
			id, in.ClientUserID, in.SpecialistUserID, in.AssignedToUserID,
			snap.TemplateID, strings.TrimSpace(in.Title), src,
			snap.RevisionsIncluded, in.Budget, in.Notes, now,
		).Scan(
			&created.ID, &created.LeadID, &created.ClientUserID, &created.SpecialistUserID, &created.AssignedToUserID,
			&created.TemplateID, &created.Title, &created.Source, &created.Status,
			&created.RevisionsIncluded, &created.RevisionsUsed,
			&created.Budget, &created.Notes, &created.StartedAt, &created.CompletedAt,
			&created.CreatedAt, &created.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert project: %w", err)
		}

		if err := s.writeSnapshot(ctx, tx, created.ID, snap); err != nil {
			return err
		}
		return s.emitCreatedInTx(ctx, tx, created, actorUserID)
	})
	if err != nil {
		return ProjectAdminView{}, err
	}
	return created, nil
}

// StartFromRecipient создаёт marketplace-проект из принятого recipient'а.
// Проверяет recipient.status='accepted' (иначе ErrRecipientNotReady).
// Партиал-уникальный индекс по (lead_id, specialist_user_id) защищает
// от двойного старта; при конфликте → ErrAlreadyStarted.
//
// title берётся из lead.brief обрезанным (или передаётся явно — для
// bulk-сценария Directus может проставить осмысленное «Проект: {category}
// от {date}»; на MVP — простое значение из input.Title).
func (s *Service) StartFromRecipient(ctx context.Context, in CreateFromRecipientInput, actorUserID uuid.UUID) (ProjectAdminView, error) {
	if err := s.validateCreateInput(in.Title, in.TemplateCode); err != nil {
		return ProjectAdminView{}, err
	}

	var created ProjectAdminView
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Проверка recipient.status — без него старт запрещён (бриф §2.2).
		var status string
		var clientUserID uuid.UUID
		err := tx.QueryRow(ctx, `
SELECT lr.status, l.client_user_id
FROM lead_recipients lr
JOIN leads l ON l.id = lr.lead_id
WHERE lr.lead_id = $1 AND lr.specialist_user_id = $2`,
			in.LeadID, in.SpecialistUserID,
		).Scan(&status, &clientUserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: lead recipient not found", ErrInvalidInput)
		}
		if err != nil {
			return fmt.Errorf("query recipient: %w", err)
		}
		if status != "accepted" {
			return ErrRecipientNotReady
		}
		if clientUserID == uuid.Nil {
			// Бриф разрешает анонимного клиента в lead'е (см. leads.go),
			// но проект без клиента создать нельзя.
			return fmt.Errorf("%w: lead has no client_user_id, can't start project", ErrInvalidInput)
		}

		snap, err := s.loadTemplateInTx(ctx, tx, in.TemplateCode, in.TemplateVersion)
		if err != nil {
			return err
		}

		id := uuid.New()
		now := time.Now().UTC()
		const insQ = `
INSERT INTO projects (
    id, lead_id, client_user_id, specialist_user_id, assigned_to_user_id,
    template_id, title, source, status, revisions_included,
    budget, notes, started_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'marketplace', 'active', $8, $9, NULLIF($10, ''), $11)
RETURNING id, lead_id, client_user_id, specialist_user_id, assigned_to_user_id,
          template_id, title, source, status, revisions_included, revisions_used,
          budget, COALESCE(notes, ''), started_at, completed_at, created_at, updated_at`
		err = tx.QueryRow(ctx, insQ,
			id, in.LeadID, clientUserID, in.SpecialistUserID, in.AssignedToUserID,
			snap.TemplateID, strings.TrimSpace(in.Title),
			snap.RevisionsIncluded, in.Budget, in.Notes, now,
		).Scan(
			&created.ID, &created.LeadID, &created.ClientUserID, &created.SpecialistUserID, &created.AssignedToUserID,
			&created.TemplateID, &created.Title, &created.Source, &created.Status,
			&created.RevisionsIncluded, &created.RevisionsUsed,
			&created.Budget, &created.Notes, &created.StartedAt, &created.CompletedAt,
			&created.CreatedAt, &created.UpdatedAt,
		)
		if err != nil {
			if isUniqueViolation(err, "projects_recipient_unique_idx") {
				return ErrAlreadyStarted
			}
			return fmt.Errorf("insert marketplace project: %w", err)
		}

		if err := s.writeSnapshot(ctx, tx, created.ID, snap); err != nil {
			return err
		}
		return s.emitCreatedInTx(ctx, tx, created, actorUserID)
	})
	if err != nil {
		return ProjectAdminView{}, err
	}
	return created, nil
}

// StartFromAllAcceptedRecipients — для bulk-кнопки в Directus.
// Делает по StartFromRecipient на каждый accepted recipient одного
// лида. Возвращает список созданных + список skipped (уже создан).
// Атомарность не нужна на уровне всего bulk: если для одного спеца
// уже есть проект, бросаем skip и идём дальше.
type BulkStartResult struct {
	Created []ProjectAdminView
	Skipped []uuid.UUID // specialist_user_id-ы, у которых проект уже был
}

func (s *Service) StartFromAllAcceptedRecipients(ctx context.Context, leadID uuid.UUID, templateCode string, templateVersion int, title string, assignedTo *uuid.UUID, actorUserID uuid.UUID) (BulkStartResult, error) {
	if err := s.validateCreateInput(title, templateCode); err != nil {
		return BulkStartResult{}, err
	}

	// Список accepted recipients по этому лиду.
	rows, err := s.repo.db.Query(ctx, `
SELECT specialist_user_id FROM lead_recipients
WHERE lead_id = $1 AND status = 'accepted'`,
		leadID,
	)
	if err != nil {
		return BulkStartResult{}, fmt.Errorf("query accepted recipients: %w", err)
	}
	specIDs := make([]uuid.UUID, 0, 4)
	for rows.Next() {
		var sid uuid.UUID
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return BulkStartResult{}, err
		}
		specIDs = append(specIDs, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return BulkStartResult{}, err
	}

	var result BulkStartResult
	for _, sid := range specIDs {
		created, startErr := s.StartFromRecipient(ctx, CreateFromRecipientInput{
			LeadID:           leadID,
			SpecialistUserID: sid,
			AssignedToUserID: assignedTo,
			TemplateCode:     templateCode,
			TemplateVersion:  templateVersion,
			Title:            title,
		}, actorUserID)
		switch {
		case errors.Is(startErr, ErrAlreadyStarted):
			result.Skipped = append(result.Skipped, sid)
		case startErr != nil:
			// Ломать весь bulk на одной ошибке (например, бракованный
			// шаблон) — правильно: вернём ошибку, остальное менеджер
			// поправит.
			return result, fmt.Errorf("start for specialist %s: %w", sid, startErr)
		default:
			result.Created = append(result.Created, created)
		}
	}
	return result, nil
}

// ─── Внутренности ───────────────────────────────────────────────────

func (s *Service) validateCreateInput(title, templateCode string) error {
	t := strings.TrimSpace(title)
	if t == "" || len(t) > 200 {
		return fmt.Errorf("%w: title required, ≤200 chars", ErrInvalidInput)
	}
	if strings.TrimSpace(templateCode) == "" {
		return fmt.Errorf("%w: template_code required", ErrInvalidInput)
	}
	return nil
}

func (s *Service) loadTemplateInTx(ctx context.Context, tx pgx.Tx, code string, version int) (SnapshotTemplate, error) {
	// LoadActiveTemplate использует pool, не tx — но это read-only,
	// safe вне tx (никаких локов мы тут не берём). Если в будущем
	// захотим читать из той же tx (snapshot isolation для apply-фазы) —
	// добавим LoadActiveTemplateInTx.
	_ = tx
	return s.repo.LoadActiveTemplate(ctx, code, version)
}

// writeSnapshot копирует стадии и шаги из шаблона в project_stages/_steps.
// Идентификаторы стадий генерируются заново, для шагов тоже. project_id —
// общий. Возможные ошибки FK/unique индекса прокидываются наверх.
func (s *Service) writeSnapshot(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, snap SnapshotTemplate) error {
	for _, st := range snap.Stages {
		stageID := uuid.New()
		if _, err := tx.Exec(ctx, `
INSERT INTO project_stages (id, project_id, code, title, sort_order)
VALUES ($1, $2, $3, $4, $5)`,
			stageID, projectID, st.Code, st.Title, st.SortOrder,
		); err != nil {
			return fmt.Errorf("insert project_stage %s: %w", st.Code, err)
		}

		for _, step := range st.Steps {
			if _, err := tx.Exec(ctx, `
INSERT INTO project_steps (
    project_id, stage_id, code, title, owner, status,
    duration_days, visible_to_client, visible_to_specialist,
    weight, sort_order
)
VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, $8, $9, $10)`,
				projectID, stageID, step.Code, step.Title, string(step.Owner),
				step.DurationDays, step.VisibleToClient, step.VisibleToSpecialist,
				step.Weight, step.SortOrder,
			); err != nil {
				return fmt.Errorf("insert project_step %s/%s: %w", st.Code, step.Code, err)
			}
		}
	}
	return nil
}

func (s *Service) emitCreatedInTx(ctx context.Context, tx pgx.Tx, p ProjectAdminView, actorUserID uuid.UUID) error {
	payload := ProjectEventPayload{
		ProjectID:        p.ID,
		ProjectSource:    p.Source,
		LeadID:           p.LeadID,
		Title:            p.Title,
		ActorUserID:      &actorUserID,
		ActorType:        "human",
		ClientUserID:     p.ClientUserID,
		SpecialistUserID: p.SpecialistUserID,
		AssignedToUserID: p.AssignedToUserID,
		OccurredAt:       time.Now().UTC(),
	}
	// Денормализованные адресные поля. Best-effort: если users
	// репо не подключён или юзер не найден — оставляем пустыми
	// и логика n8n среагирует ("нет email — не шлём").
	if s.users != nil {
		if email, name, err := s.users.GetEmailAndName(ctx, p.ClientUserID); err == nil {
			payload.ClientEmail, payload.ClientName = email, name
		}
		if p.SpecialistUserID != nil {
			if email, _, err := s.users.GetEmailAndName(ctx, *p.SpecialistUserID); err == nil {
				payload.SpecialistEmail = email
			}
		}
		if p.AssignedToUserID != nil {
			if email, _, err := s.users.GetEmailAndName(ctx, *p.AssignedToUserID); err == nil {
				payload.ManagerEmail = email
			}
		}
	}
	return emitProjectEvent(ctx, tx, EventProjectCreated, payload)
}

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.repo.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isUniqueViolation — SQLSTATE 23505 с привязкой к имени индекса.
// Используется для отлова partial unique по (lead_id, specialist_user_id)
// при двойном старте marketplace-проекта.
func isUniqueViolation(err error, indexName string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == indexName
}
