package publications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Документы: шаблоны с версиями (библиотека админа, рядом с прайсом) и
// документы конкретному человеку — креатору проекта или его заказчику.
//
// Хранение — как у проектов и прайса: ничего не удаляется. Шаблон уходит
// в архив и возвращается; версия неизменна, правка — новая версия;
// выданный документ отзывается, а не стирается. Документы — только
// ссылки (решение владельца от 2 октября 2026): файлы с паспортными
// данными держать у себя — отдельная история с закрытым хранилищем.
//
// Выдача — всегда руками менеджера. Ответа «подписал» в сервисе нет:
// подписанный экземпляр человек присылает в комментарии проекта.

// Виды документа. Совпадают с CHECK в 00081.
const (
	DocKindContract = "contract"
	DocKindAct      = "act"
	DocKindNDA      = "nda"
	DocKindOther    = "other"
)

const (
	docTitleMax = 200
	docURLMax   = 2000
	docNoteMax  = 1000
)

// События выдачи — project.document_delivered (креатору) и
// project.client_document_delivered (заказчику). Два типа, а не один с
// полем: бот выбирается по типу события (eventroute/bot.go), а креатору
// и заказчику пишут разные боты.

// ErrTemplateArchived — шаблон в архиве: выдавать и выпускать по нему
// новые версии нельзя, пока его не вернут.
var ErrTemplateArchived = errors.New("шаблон в архиве — сначала верните его")

// DocumentTemplate — шаблон документа в библиотеке.
type DocumentTemplate struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Title      string     `json:"title"`
	Audience   string     `json:"audience"`
	CreatedAt  time.Time  `json:"created_at"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	// Current — действующая версия: последняя по номеру.
	Current *TemplateVersion `json:"current,omitempty"`
	// Versions — история, свежие сверху. Только в выдаче админу.
	Versions []TemplateVersion `json:"versions,omitempty"`
}

// TemplateVersion — версия шаблона. Не меняется после публикации.
type TemplateVersion struct {
	ID          uuid.UUID `json:"id"`
	TemplateID  uuid.UUID `json:"template_id"`
	Version     int       `json:"version"`
	URL         string    `json:"url"`
	Note        string    `json:"note,omitempty"`
	PublishedAt time.Time `json:"published_at"`
}

// UserDocument — документ, выданный конкретному человеку.
type UserDocument struct {
	ID              uuid.UUID  `json:"id"`
	RecipientUserID uuid.UUID  `json:"recipient_user_id"`
	RecipientName   string     `json:"recipient_name,omitempty"`
	ProjectID       uuid.UUID  `json:"project_id"`
	ProjectTitle    string     `json:"project_title,omitempty"`
	Audience        string     `json:"audience"`
	TemplateID      *uuid.UUID `json:"template_id,omitempty"`
	// TemplateVersion — номер версии шаблона, по которой выдан. Пусто —
	// менеджер приложил свою ссылку.
	TemplateVersion *int       `json:"template_version,omitempty"`
	Kind            string     `json:"kind"`
	Title           string     `json:"title"`
	URL             string     `json:"url"`
	Note            string     `json:"note,omitempty"`
	SentBy          *uuid.UUID `json:"sent_by,omitempty"`
	SentByName      string     `json:"sent_by_name,omitempty"`
	SentAt          time.Time  `json:"sent_at"`
	OpenedAt        *time.Time `json:"opened_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

// MyDocument — строка «Моих документов»: и выданное лично, и договор,
// лежащий в материалах проекта, — одним списком.
type MyDocument struct {
	ID uuid.UUID `json:"id"`
	// Source — personal (выдан лично, можно отметить открытым) или
	// project (договор из материалов проекта, общий для состава).
	Source       string     `json:"source"`
	ProjectID    uuid.UUID  `json:"project_id"`
	ProjectTitle string     `json:"project_title"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	Note         string     `json:"note,omitempty"`
	SentAt       time.Time  `json:"sent_at"`
	OpenedAt     *time.Time `json:"opened_at,omitempty"`
}

// DeliverInput — что выдаёт менеджер.
type DeliverInput struct {
	ProjectID uuid.UUID
	// Audience — креаторам или заказчику.
	Audience string
	// RecipientIDs — кому. Пусто: всем креаторам состава или заказчику
	// проекта — смотря по Audience.
	RecipientIDs []uuid.UUID
	// TemplateID — выдать действующую версию шаблона. Если пусто, нужны
	// Kind, Title и URL — менеджер прикладывает свою ссылку.
	TemplateID uuid.UUID
	Kind       string
	Title      string
	URL        string
	Note       string
	SentBy     uuid.UUID
}

// ---- проверка ввода ----

func validDocKind(k string) bool {
	switch k {
	case DocKindContract, DocKindAct, DocKindNDA, DocKindOther:
		return true
	}
	return false
}

func validAudience(a string) bool {
	return a == AudienceCreators || a == AudienceClient
}

// checkDocURL — ссылка документа: длина и та же проверка на http(s),
// что у материалов проекта (isHTTPLink).
func checkDocURL(raw string) error {
	if raw == "" || len(raw) > docURLMax {
		return fmt.Errorf("%w: url must be 1-%d chars", ErrInvalidInput, docURLMax)
	}
	if !isHTTPLink(raw) {
		return fmt.Errorf("%w: url must be an http(s) link", ErrInvalidInput)
	}
	return nil
}

func checkDocTitle(t string) error {
	if t == "" || len([]rune(t)) > docTitleMax {
		return fmt.Errorf("%w: title must be 1-%d chars", ErrInvalidInput, docTitleMax)
	}
	return nil
}

func checkDocNote(n string) error {
	if len([]rune(n)) > docNoteMax {
		return fmt.Errorf("%w: note must be at most %d chars", ErrInvalidInput, docNoteMax)
	}
	return nil
}

// ---- шаблоны: репозиторий ----

// ListDocumentTemplates — библиотека шаблонов. withArchived — для
// админки: архив там видно отдельным фильтром. Версии подгружаются
// только когда нужны (withVersions): менеджеру история ни к чему.
func (r *Repo) ListDocumentTemplates(
	ctx context.Context, withArchived, withVersions bool, audience string,
) ([]DocumentTemplate, error) {
	rows, err := r.db.Query(ctx, `
SELECT t.id, t.kind, t.title, t.audience, t.created_at, t.archived_at,
       v.id, v.version, v.url, v.note, v.published_at
FROM document_templates t
JOIN LATERAL (
    SELECT id, version, url, note, published_at
    FROM document_template_versions
    WHERE template_id = t.id
    ORDER BY version DESC LIMIT 1
) v ON TRUE
WHERE ($1 OR t.archived_at IS NULL)
  AND ($2 = '' OR t.audience = $2)
ORDER BY t.archived_at IS NOT NULL, t.audience, t.title`, withArchived, audience)
	if err != nil {
		return nil, fmt.Errorf("list document templates: %w", err)
	}
	defer rows.Close()
	out := make([]DocumentTemplate, 0)
	for rows.Next() {
		var t DocumentTemplate
		var v TemplateVersion
		if err := rows.Scan(&t.ID, &t.Kind, &t.Title, &t.Audience, &t.CreatedAt, &t.ArchivedAt,
			&v.ID, &v.Version, &v.URL, &v.Note, &v.PublishedAt); err != nil {
			return nil, fmt.Errorf("scan document template: %w", err)
		}
		v.TemplateID = t.ID
		t.Current = &v
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !withVersions || len(out) == 0 {
		return out, nil
	}

	ids := make([]uuid.UUID, len(out))
	for i, t := range out {
		ids[i] = t.ID
	}
	vrows, err := r.db.Query(ctx, `
SELECT id, template_id, version, url, note, published_at
FROM document_template_versions
WHERE template_id = ANY($1)
ORDER BY template_id, version DESC`, ids)
	if err != nil {
		return nil, fmt.Errorf("list template versions: %w", err)
	}
	defer vrows.Close()
	byID := make(map[uuid.UUID][]TemplateVersion, len(out))
	for vrows.Next() {
		var v TemplateVersion
		if err := vrows.Scan(&v.ID, &v.TemplateID, &v.Version, &v.URL, &v.Note, &v.PublishedAt); err != nil {
			return nil, fmt.Errorf("scan template version: %w", err)
		}
		byID[v.TemplateID] = append(byID[v.TemplateID], v)
	}
	if err := vrows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Versions = byID[out[i].ID]
	}
	return out, nil
}

// CreateDocumentTemplate — шаблон и его первая версия, одной транзакцией:
// шаблон без версии выдать нечем, а в списке он выглядел бы рабочим.
func (r *Repo) CreateDocumentTemplate(
	ctx context.Context, actor uuid.UUID, kind, title, audience, docURL, note string,
) (DocumentTemplate, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DocumentTemplate{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t := DocumentTemplate{Kind: kind, Title: title, Audience: audience}
	if err := tx.QueryRow(ctx, `
INSERT INTO document_templates (kind, title, audience, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id, created_at`, kind, title, audience, actor).Scan(&t.ID, &t.CreatedAt); err != nil {
		return DocumentTemplate{}, fmt.Errorf("insert document template: %w", err)
	}
	v := TemplateVersion{TemplateID: t.ID, Version: 1, URL: docURL, Note: note}
	if err := tx.QueryRow(ctx, `
INSERT INTO document_template_versions (template_id, version, url, note, published_by)
VALUES ($1, 1, $2, $3, $4)
RETURNING id, published_at`, t.ID, docURL, note, actor).Scan(&v.ID, &v.PublishedAt); err != nil {
		return DocumentTemplate{}, fmt.Errorf("insert first version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DocumentTemplate{}, fmt.Errorf("commit: %w", err)
	}
	t.Current = &v
	t.Versions = []TemplateVersion{v}
	return t, nil
}

// PublishTemplateVersion — новая версия. Номер — следующий за последним,
// под блокировкой шаблона: две публикации подряд не получат один номер.
func (r *Repo) PublishTemplateVersion(
	ctx context.Context, actor, templateID uuid.UUID, docURL, note string,
) (TemplateVersion, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TemplateVersion{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var archived *time.Time
	err = tx.QueryRow(ctx, `
SELECT archived_at FROM document_templates WHERE id = $1 FOR UPDATE`, templateID).Scan(&archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return TemplateVersion{}, ErrNotFound
	}
	if err != nil {
		return TemplateVersion{}, fmt.Errorf("lock template: %w", err)
	}
	if archived != nil {
		return TemplateVersion{}, ErrTemplateArchived
	}
	v := TemplateVersion{TemplateID: templateID, URL: docURL, Note: note}
	if err := tx.QueryRow(ctx, `
INSERT INTO document_template_versions (template_id, version, url, note, published_by)
SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3, $4
FROM document_template_versions WHERE template_id = $1
RETURNING id, version, published_at`, templateID, docURL, note, actor).
		Scan(&v.ID, &v.Version, &v.PublishedAt); err != nil {
		return TemplateVersion{}, fmt.Errorf("insert version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TemplateVersion{}, fmt.Errorf("commit: %w", err)
	}
	return v, nil
}

// SetTemplateArchived — в архив или обратно. Выданных документов не
// касается: у них своя ссылка на версию.
func (r *Repo) SetTemplateArchived(ctx context.Context, actor, templateID uuid.UUID, archived bool) error {
	tag, err := r.db.Exec(ctx, `
UPDATE document_templates
SET archived_at = CASE WHEN $2::boolean THEN COALESCE(archived_at, now()) ELSE NULL END,
    archived_by = CASE WHEN $2::boolean THEN $3::uuid ELSE NULL END
WHERE id = $1`, templateID, archived, actor)
	if err != nil {
		return fmt.Errorf("archive template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- выдача: репозиторий ----

// docRecipients — кому выдаём. Пустой список — все по аудитории: весь
// действующий состав или заказчик проекта. Непустой — каждый обязан
// быть в проекте именно в этой роли: id сам по себе права не даёт, и
// выдать договор постороннему значило бы показать ему проект.
func docRecipients(
	ctx context.Context, tx pgx.Tx, projectID uuid.UUID, audience string, wanted []uuid.UUID,
) ([]uuid.UUID, error) {
	var q string
	if audience == AudienceClient {
		q = `SELECT client_user_id FROM projects WHERE id = $1 AND client_user_id IS NOT NULL`
	} else {
		q = `SELECT creator_user_id FROM project_creators
             WHERE project_id = $1 AND removed_at IS NULL`
	}
	rows, err := tx.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("document recipients: %w", err)
	}
	inProject := make(map[uuid.UUID]bool)
	all := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		inProject[id] = true
		all = append(all, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		if len(all) == 0 {
			if audience == AudienceClient {
				return nil, fmt.Errorf("%w: у проекта нет заказчика", ErrInvalidInput)
			}
			return nil, fmt.Errorf("%w: в составе проекта никого нет", ErrInvalidInput)
		}
		return all, nil
	}
	seen := make(map[uuid.UUID]bool, len(wanted))
	out := make([]uuid.UUID, 0, len(wanted))
	for _, id := range wanted {
		if !inProject[id] {
			if audience == AudienceClient {
				return nil, fmt.Errorf("%w: адресат не заказчик этого проекта", ErrInvalidInput)
			}
			return nil, fmt.Errorf("%w: адресат не в составе проекта", ErrInvalidInput)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// documentDelivered — что уходит боту при выдаче.
type documentDelivered struct {
	ProjectID     uuid.UUID   `json:"project_id"`
	ProjectTitle  string      `json:"project_title"`
	DocumentTitle string      `json:"document_title"`
	Kind          string      `json:"kind"`
	Note          string      `json:"note,omitempty"`
	RecipientIDs  []uuid.UUID `json:"recipient_ids"`
}

// DeliverDocument — выдать документ: по строке на адресата и одно
// событие боту на всех. Всё в одной транзакции: сообщение о документе,
// которого не выдали, — худший вид вранья.
func (r *Repo) DeliverDocument(ctx context.Context, in DeliverInput) ([]UserDocument, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var versionID *uuid.UUID
	var versionNo *int
	var templateID *uuid.UUID
	kind, title, docURL := in.Kind, in.Title, in.URL
	if in.TemplateID != uuid.Nil {
		var (
			vid      uuid.UUID
			vno      int
			audience string
			archived *time.Time
		)
		err := tx.QueryRow(ctx, `
SELECT t.kind, t.title, t.audience, t.archived_at, v.id, v.version, v.url
FROM document_templates t
JOIN LATERAL (
    SELECT id, version, url FROM document_template_versions
    WHERE template_id = t.id ORDER BY version DESC LIMIT 1
) v ON TRUE
WHERE t.id = $1`, in.TemplateID).Scan(&kind, &title, &audience, &archived, &vid, &vno, &docURL)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("load template: %w", err)
		}
		if archived != nil {
			return nil, ErrTemplateArchived
		}
		if audience != in.Audience {
			return nil, fmt.Errorf("%w: шаблон для другой стороны проекта", ErrInvalidInput)
		}
		tid := in.TemplateID
		templateID, versionID, versionNo = &tid, &vid, &vno
	}

	recipients, err := docRecipients(ctx, tx, in.ProjectID, in.Audience, in.RecipientIDs)
	if err != nil {
		return nil, err
	}

	var projectTitle string
	if err := tx.QueryRow(ctx, `SELECT title FROM projects WHERE id = $1`, in.ProjectID).
		Scan(&projectTitle); err != nil {
		return nil, fmt.Errorf("project title: %w", err)
	}

	out := make([]UserDocument, 0, len(recipients))
	for _, rid := range recipients {
		d := UserDocument{
			RecipientUserID: rid, ProjectID: in.ProjectID, ProjectTitle: projectTitle,
			Audience: in.Audience, TemplateID: templateID, TemplateVersion: versionNo,
			Kind: kind, Title: title, URL: docURL, Note: in.Note,
		}
		sentBy := in.SentBy
		d.SentBy = &sentBy
		if err := tx.QueryRow(ctx, `
INSERT INTO user_documents
  (recipient_user_id, project_id, audience, template_version_id, kind, title, url, note, sent_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, sent_at`, rid, in.ProjectID, in.Audience, versionID, kind, title, docURL, in.Note,
			in.SentBy).Scan(&d.ID, &d.SentAt); err != nil {
			return nil, fmt.Errorf("insert user document: %w", err)
		}
		out = append(out, d)
	}

	payload := documentDelivered{
		ProjectID: in.ProjectID, ProjectTitle: projectTitle, DocumentTitle: title,
		Kind: kind, Note: in.Note, RecipientIDs: recipients,
	}
	// Тип — литералом в каждой ветке: охранный тест маршрутизации
	// (eventroute/chat_test.go) читает типы событий из исходников, и
	// собранный в переменной тип для него невидим.
	if in.Audience == AudienceClient {
		err = outbox.Emit(ctx, tx, outbox.AggregateProject, in.ProjectID.String(),
			"project.client_document_delivered", payload)
	} else {
		err = outbox.Emit(ctx, tx, outbox.AggregateProject, in.ProjectID.String(),
			"project.document_delivered", payload)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

const userDocumentSelect = `
SELECT d.id, d.recipient_user_id, ` + displayNameExpr + `, d.project_id, pr.title, d.audience,
       v.template_id, v.version, d.kind, d.title, d.url, d.note,
       d.sent_by, COALESCE(NULLIF(sbu.display_name, ''), split_part(sbu.email, '@', 1), ''),
       d.sent_at, d.opened_at, d.revoked_at
FROM user_documents d
JOIN projects pr ON pr.id = d.project_id
JOIN users u ON u.id = d.recipient_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp     ON cp.user_id = u.id
LEFT JOIN document_template_versions v ON v.id = d.template_version_id
LEFT JOIN users sbu ON sbu.id = d.sent_by`

func scanUserDocuments(rows pgx.Rows) ([]UserDocument, error) {
	defer rows.Close()
	out := make([]UserDocument, 0)
	for rows.Next() {
		var d UserDocument
		if err := rows.Scan(&d.ID, &d.RecipientUserID, &d.RecipientName, &d.ProjectID,
			&d.ProjectTitle, &d.Audience, &d.TemplateID, &d.TemplateVersion, &d.Kind, &d.Title,
			&d.URL, &d.Note, &d.SentBy, &d.SentByName, &d.SentAt, &d.OpenedAt, &d.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan user document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ProjectDocuments — история выдачи по проекту, с отозванными: менеджер
// видит, кому что и когда давали, даже если документ уже забрали.
func (r *Repo) ProjectDocuments(ctx context.Context, projectID uuid.UUID) ([]UserDocument, error) {
	rows, err := r.db.Query(ctx, userDocumentSelect+`
WHERE d.project_id = $1
ORDER BY d.sent_at DESC, d.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("project documents: %w", err)
	}
	return scanUserDocuments(rows)
}

// SetDocumentRevoked — отозвать или вернуть. Документ обязан быть из
// этого проекта: id сам по себе права не даёт.
func (r *Repo) SetDocumentRevoked(
	ctx context.Context, projectID, docID, actor uuid.UUID, revoked bool,
) error {
	tag, err := r.db.Exec(ctx, `
UPDATE user_documents
SET revoked_at = CASE WHEN $3::boolean THEN COALESCE(revoked_at, now()) ELSE NULL END,
    revoked_by = CASE WHEN $3::boolean THEN $4::uuid ELSE NULL END
WHERE id = $1 AND project_id = $2`, docID, projectID, revoked, actor)
	if err != nil {
		return fmt.Errorf("revoke document: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// stillInProjectOf — адресат личного документа по-прежнему в проекте:
// креатор не убран из состава, заказчик не сменён. Ушедшему документы
// проекта не показываем — ровно как договоры из материалов.
func stillInProjectOf(t string) string {
	return `(
       (` + t + `.audience = 'creators' AND EXISTS (
            SELECT 1 FROM project_creators pc
            WHERE pc.project_id = ` + t + `.project_id
              AND pc.creator_user_id = ` + t + `.recipient_user_id
              AND pc.removed_at IS NULL))
    OR (` + t + `.audience = 'client' AND EXISTS (
            SELECT 1 FROM projects p
            WHERE p.id = ` + t + `.project_id
              AND p.client_user_id = ` + t + `.recipient_user_id))
  )`
}

var stillInProject = stillInProjectOf("d")

// MyDocumentsFilter — сужение «Моих документов».
type MyDocumentsFilter struct {
	// ProjectID — только этот проект (карточка проекта); nil — все.
	ProjectID *uuid.UUID
	// PersonalOnly — только выданное лично, без договоров из материалов:
	// для страницы, где материалы проекта и так стоят рядом.
	PersonalOnly bool
}

// MyDocuments — «Мои документы»: выданное лично (не отозванное) и
// договоры из материалов проектов, где человек сейчас работает.
//
// Договоры из материалов остаются как есть, без переноса: до таблицы
// user_documents договор клали материалом всему составу, и человек
// должен видеть и их тоже.
func (r *Repo) MyDocuments(ctx context.Context, userID uuid.UUID, f MyDocumentsFilter) ([]MyDocument, error) {
	// Порядок: в карточке одного проекта договор первым — он один на
	// проект, и ищут его чаще остального. По всем проектам — свежие
	// сверху: договоров столько же, сколько проектов, и первым нужен
	// тот, что пришёл последним.
	rows, err := r.db.Query(ctx, `
SELECT * FROM (
SELECT d.id, 'personal', d.project_id, pr.title, d.kind, d.title, d.url, d.note,
       d.sent_at, d.opened_at
FROM user_documents d
JOIN projects pr ON pr.id = d.project_id
WHERE d.recipient_user_id = $1 AND d.revoked_at IS NULL
  AND ($3::uuid IS NULL OR d.project_id = $3)
  AND `+stillInProject+`
UNION ALL
SELECT m.id, 'project', m.project_id, pr.title, m.kind, m.title, m.url, '',
       m.created_at, NULL
FROM project_materials m
JOIN projects pr ON pr.id = m.project_id
WHERE m.kind = ANY($2)
  AND NOT $4::boolean
  AND ($3::uuid IS NULL OR m.project_id = $3)
  AND m.delivery_id IS NULL
  AND m.deleted_at IS NULL
  AND (
       (m.audience = 'creators' AND EXISTS (
            SELECT 1 FROM project_creators pc
            WHERE pc.project_id = m.project_id AND pc.creator_user_id = $1
              AND pc.removed_at IS NULL))
    OR (m.audience = 'client' AND pr.client_user_id = $1)
  )
) docs (id, source, project_id, project_title, kind, title, url, note, sent_at, opened_at)
ORDER BY ($3::uuid IS NOT NULL AND kind <> 'contract'), sent_at DESC, id`,
		userID, DocumentKinds, f.ProjectID, f.PersonalOnly)
	if err != nil {
		return nil, fmt.Errorf("my documents: %w", err)
	}
	defer rows.Close()
	out := make([]MyDocument, 0)
	for rows.Next() {
		var d MyDocument
		if err := rows.Scan(&d.ID, &d.Source, &d.ProjectID, &d.ProjectTitle, &d.Kind, &d.Title,
			&d.URL, &d.Note, &d.SentAt, &d.OpenedAt); err != nil {
			return nil, fmt.Errorf("scan my document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDocumentOpened — адресат открыл документ. Ставится один раз:
// менеджеру важно, что документ дошёл, а не сколько раз его смотрели.
// Чужой или отозванный — ErrNotFound, как будто его нет.
func (r *Repo) MarkDocumentOpened(ctx context.Context, userID, docID uuid.UUID) (time.Time, error) {
	var at time.Time
	err := r.db.QueryRow(ctx, `
UPDATE user_documents
SET opened_at = COALESCE(opened_at, now())
WHERE id = $1 AND recipient_user_id = $2 AND revoked_at IS NULL
  AND `+stillInProjectOf("user_documents")+`
RETURNING opened_at`, docID, userID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("mark opened: %w", err)
	}
	return at, nil
}

// ---- сервис ----

// ListDocumentTemplates — библиотека шаблонов.
func (s *Service) ListDocumentTemplates(
	ctx context.Context, withArchived, withVersions bool, audience string,
) ([]DocumentTemplate, error) {
	if audience != "" && !validAudience(audience) {
		return nil, fmt.Errorf("%w: audience must be creators or client", ErrInvalidInput)
	}
	return s.repo.ListDocumentTemplates(ctx, withArchived, withVersions, audience)
}

// CreateDocumentTemplate — проверить и завести шаблон с первой версией.
func (s *Service) CreateDocumentTemplate(
	ctx context.Context, actor uuid.UUID, kind, title, audience, docURL, note string,
) (DocumentTemplate, error) {
	kind, title = strings.TrimSpace(kind), strings.TrimSpace(title)
	docURL, note = strings.TrimSpace(docURL), strings.TrimSpace(note)
	if !validDocKind(kind) {
		return DocumentTemplate{}, fmt.Errorf("%w: kind must be contract, act, nda or other", ErrInvalidInput)
	}
	if !validAudience(audience) {
		return DocumentTemplate{}, fmt.Errorf("%w: audience must be creators or client", ErrInvalidInput)
	}
	if err := checkDocTitle(title); err != nil {
		return DocumentTemplate{}, err
	}
	if err := checkDocURL(docURL); err != nil {
		return DocumentTemplate{}, err
	}
	if err := checkDocNote(note); err != nil {
		return DocumentTemplate{}, err
	}
	return s.repo.CreateDocumentTemplate(ctx, actor, kind, title, audience, docURL, note)
}

// PublishTemplateVersion — новая версия шаблона.
func (s *Service) PublishTemplateVersion(
	ctx context.Context, actor, templateID uuid.UUID, docURL, note string,
) (TemplateVersion, error) {
	docURL, note = strings.TrimSpace(docURL), strings.TrimSpace(note)
	if err := checkDocURL(docURL); err != nil {
		return TemplateVersion{}, err
	}
	if err := checkDocNote(note); err != nil {
		return TemplateVersion{}, err
	}
	return s.repo.PublishTemplateVersion(ctx, actor, templateID, docURL, note)
}

// SetTemplateArchived — в архив или обратно.
func (s *Service) SetTemplateArchived(ctx context.Context, actor, templateID uuid.UUID, archived bool) error {
	return s.repo.SetTemplateArchived(ctx, actor, templateID, archived)
}

// DeliverDocument — проверить и выдать.
func (s *Service) DeliverDocument(ctx context.Context, in DeliverInput) ([]UserDocument, error) {
	in.Kind, in.Title = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Title)
	in.URL, in.Note = strings.TrimSpace(in.URL), strings.TrimSpace(in.Note)
	if !validAudience(in.Audience) {
		return nil, fmt.Errorf("%w: audience must be creators or client", ErrInvalidInput)
	}
	if err := checkDocNote(in.Note); err != nil {
		return nil, err
	}
	if in.TemplateID == uuid.Nil {
		if !validDocKind(in.Kind) {
			return nil, fmt.Errorf("%w: kind must be contract, act, nda or other", ErrInvalidInput)
		}
		if err := checkDocTitle(in.Title); err != nil {
			return nil, err
		}
		if err := checkDocURL(in.URL); err != nil {
			return nil, err
		}
	}
	return s.repo.DeliverDocument(ctx, in)
}

// ProjectDocuments — история выдачи по проекту.
func (s *Service) ProjectDocuments(ctx context.Context, projectID uuid.UUID) ([]UserDocument, error) {
	return s.repo.ProjectDocuments(ctx, projectID)
}

// SetDocumentRevoked — отозвать или вернуть.
func (s *Service) SetDocumentRevoked(
	ctx context.Context, projectID, docID, actor uuid.UUID, revoked bool,
) error {
	return s.repo.SetDocumentRevoked(ctx, projectID, docID, actor, revoked)
}

// MyDocuments — «Мои документы» человека.
func (s *Service) MyDocuments(ctx context.Context, userID uuid.UUID, f MyDocumentsFilter) ([]MyDocument, error) {
	return s.repo.MyDocuments(ctx, userID, f)
}

// MarkDocumentOpened — отметка «открыл».
func (s *Service) MarkDocumentOpened(ctx context.Context, userID, docID uuid.UUID) (time.Time, error) {
	return s.repo.MarkDocumentOpened(ctx, userID, docID)
}
