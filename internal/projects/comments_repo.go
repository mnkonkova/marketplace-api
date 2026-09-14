package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrCommentEmpty = errors.New("comment body is empty")

// authorName — общий фолбэк-ладдер имени автора: specialist_profile →
// client_profile → префикс email до '@'. Повторяется в трёх запросах, и
// расходившиеся копии уже приводили к пустому имени в одном месте из трёх.
const authorName = `COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       )`

const commentCols = `pc.id, pc.project_id, pc.author_id, ` + authorName + ` AS author_name,
       pc.body, pc.body_format, pc.body_text, pc.thread, pc.thread_user_id,
       pc.is_internal, pc.created_at, pc.updated_at`

// ListThread — одна ветка переписки. Для ThreadCreator threadUserID
// обязателен: ветка адресная, и запрос без него вернул бы переписку всех
// креаторов проекта разом — ровно то, чего креатор видеть не должен.
func (r *Repo) ListThread(ctx context.Context, projectID uuid.UUID, thread string, threadUserID *uuid.UUID) ([]Comment, error) {
	if thread == ThreadCreator && threadUserID == nil {
		return nil, fmt.Errorf("%w: creator thread requires a user", ErrInvalidInput)
	}
	// $3 IS NULL OR ... — для не-креаторских веток условие вырождается
	// в TRUE, а thread_user_id там и так NULL.
	return r.queryComments(ctx, `
SELECT `+commentCols+`
FROM project_comments pc
LEFT JOIN users u ON u.id = pc.author_id
LEFT JOIN specialist_profiles sp ON sp.user_id = pc.author_id
LEFT JOIN client_profiles cp ON cp.user_id = pc.author_id
WHERE pc.project_id = $1 AND pc.deleted_at IS NULL
  AND pc.thread = $2
  AND ($3::uuid IS NULL OR pc.thread_user_id = $3)
ORDER BY pc.created_at ASC`, projectID, thread, threadUserID)
}

// ListAllComments — все ветки сразу: менеджер и админ видят и клиентскую,
// и каждую креаторскую, и внутреннюю. Фильтровать по ветке им можно на
// выдаче, но право читать есть на всё.
func (r *Repo) ListAllComments(ctx context.Context, projectID uuid.UUID) ([]Comment, error) {
	return r.queryComments(ctx, `
SELECT `+commentCols+`
FROM project_comments pc
LEFT JOIN users u ON u.id = pc.author_id
LEFT JOIN specialist_profiles sp ON sp.user_id = pc.author_id
LEFT JOIN client_profiles cp ON cp.user_id = pc.author_id
WHERE pc.project_id = $1 AND pc.deleted_at IS NULL
ORDER BY pc.created_at ASC`, projectID)
}

func (r *Repo) queryComments(ctx context.Context, q string, args ...any) ([]Comment, error) {
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	out := make([]Comment, 0)
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.AuthorID, &c.AuthorName,
			&c.Body, &c.BodyFormat, &c.BodyText, &c.Thread, &c.ThreadUserID,
			&c.IsInternal, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	// Упоминания — вторым запросом на всю пачку, а не по одному на
	// комментарий: переписка читается целиком и в ней сотни сообщений.
	byComment, err := r.loadMentions(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Mentions = byComment[out[i].ID]
	}
	return out, nil
}

func (r *Repo) loadMentions(ctx context.Context, commentIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := r.db.Query(ctx,
		`SELECT comment_id, user_id FROM comment_mentions WHERE comment_id = ANY($1)`,
		commentIDs)
	if err != nil {
		return nil, fmt.Errorf("load mentions: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID][]uuid.UUID)
	for rows.Next() {
		var cid, uid uuid.UUID
		if err := rows.Scan(&cid, &uid); err != nil {
			return nil, fmt.Errorf("scan mention: %w", err)
		}
		out[cid] = append(out[cid], uid)
	}
	return out, rows.Err()
}

// CreateComment — атомарно: запись в project_comments + упоминания +
// событие в ленте + событие в outbox + бамп projects.updated_at.
// Тело приезжает уже очищенным, разметку репозиторий не разбирает.
func (r *Repo) CreateComment(ctx context.Context, in CreateCommentInput) (Comment, error) {
	if strings.TrimSpace(in.BodyText) == "" {
		return Comment{}, ErrCommentEmpty
	}
	if in.BodyFormat == "" {
		in.BodyFormat = FormatPlain
	}
	if in.Thread == "" {
		in.Thread = ThreadClient
	}
	isInternal := in.Thread == ThreadInternal

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Comment{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	c := Comment{
		ProjectID:    in.ProjectID,
		AuthorID:     in.AuthorID,
		Body:         in.Body,
		BodyFormat:   in.BodyFormat,
		BodyText:     in.BodyText,
		Thread:       in.Thread,
		ThreadUserID: in.ThreadUserID,
		IsInternal:   isInternal,
		Mentions:     in.Mentions,
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO project_comments
  (project_id, author_id, body, body_format, body_text, thread, thread_user_id, is_internal)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at, updated_at`,
		in.ProjectID, in.AuthorID, in.Body, in.BodyFormat, in.BodyText,
		in.Thread, in.ThreadUserID, isInternal).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}

	for _, m := range in.Mentions {
		if _, err := tx.Exec(ctx,
			`INSERT INTO comment_mentions (comment_id, user_id) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`, c.ID, m); err != nil {
			return Comment{}, fmt.Errorf("insert mention: %w", err)
		}
	}

	// Внутренние комментарии не пишем в ленту — иначе они всплывут в
	// activity-фиде мимо фильтра /comments. В ленту кладём текстовую
	// проекцию: виджет ленты показывает строку, а не разметку.
	// thread в payload — чтобы будущая клиентская лента могла отфильтровать
	// креаторские ветки, не переделывая таблицу.
	if !isInternal {
		if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, comment, payload)
VALUES ($1, NULL, $2, 'human', 'comment', $3, $4)`,
			in.ProjectID, in.AuthorID, c.BodyText,
			mustJSON(map[string]string{"comment_id": c.ID.String(), "thread": in.Thread})); err != nil {
			return Comment{}, fmt.Errorf("insert comment event: %w", err)
		}
	}

	// Событие в outbox (n8n → Telegram «новое сообщение в проекте»).
	// Внутренние не нотифицируем: менеджерская переписка не должна спамить
	// общий канал. Упомянутых передаём отдельным полем — по нему n8n шлёт
	// адресное «вас упомянули», а не общее уведомление всем.
	if !isInternal {
		mentions := make([]string, 0, len(in.Mentions))
		for _, m := range in.Mentions {
			mentions = append(mentions, m.String())
		}
		payload := map[string]any{
			"project_id": in.ProjectID.String(),
			"comment_id": c.ID.String(),
			"author_id":  in.AuthorID.String(),
			"thread":     in.Thread,
			"body":       c.BodyText,
			"mentions":   mentions,
		}
		if in.ThreadUserID != nil {
			payload["thread_user_id"] = in.ThreadUserID.String()
		}
		enrichProjectPayload(ctx, tx, in.ProjectID, payload)
		if err := emit(ctx, tx, in.ProjectID, nil, in.AuthorID, "project.comment_added", payload); err != nil {
			return Comment{}, fmt.Errorf("emit comment event: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET updated_at = now() WHERE id = $1`, in.ProjectID); err != nil {
		return Comment{}, fmt.Errorf("bump project: %w", err)
	}

	// R9: display_name автора берём ВНУТРИ tx — между Commit и отдельным
	// SELECT'ом профиль мог быть удалён (или авторские поля стёрты), и в
	// ответе уходил пустой AuthorName. Внутри tx видим консистентный
	// снимок, нагрузка та же — один лёгкий SELECT с двумя LEFT JOIN'ами.
	if err := tx.QueryRow(ctx, `
SELECT `+authorName+`
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE u.id = $1`, in.AuthorID).Scan(&c.AuthorName); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.AuthorName = ""
	}

	if err := tx.Commit(ctx); err != nil {
		return Comment{}, fmt.Errorf("commit: %w", err)
	}
	return c, nil
}

// ThreadParticipants — кто читает ветку. Он же список кандидатов в
// упоминания: упомянуть можно только того, кто сообщение и так увидит,
// иначе @-меню превращается в способ дёрнуть уведомлением любого
// пользователя площадки.
//
// Админы в клиентскую и креаторскую ветку не попадают намеренно. Читать
// они их могут (админ видит всё), но показывать клиенту список админов
// площадки в выпадашке @ незачем, а написавший админ и так виден автором.
func (r *Repo) ThreadParticipants(
	ctx context.Context, projectID uuid.UUID, thread string, threadUserID *uuid.UUID,
) ([]Participant, error) {
	if thread == ThreadInternal {
		// Внутренняя ветка — вся команда: менеджер проекта тут не
		// выделен, внутри обсуждают всем составом.
		return r.queryParticipants(ctx, `
SELECT u.id, `+authorName+`,
       CASE WHEN u.is_admin THEN 'admin' ELSE 'manager' END
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE (u.is_manager OR u.is_admin) AND u.is_active
ORDER BY 2
LIMIT 200`)
	}
	// Клиентская ветка: клиент, назначенный менеджер и — в общем проекте —
	// исполнитель. Менеджера в general нет, разговор идёт напрямую.
	// Креаторская: сам креатор и назначенный менеджер.
	return r.queryParticipants(ctx, `
SELECT u.id, `+authorName+`,
       CASE
         WHEN u.is_admin   THEN 'admin'
         WHEN u.is_manager THEN 'manager'
         WHEN u.id = p.client_user_id THEN 'client'
         WHEN u.id = p.specialist_user_id THEN 'specialist'
         ELSE 'creator'
       END
FROM projects p
JOIN users u ON u.id IN (
       CASE WHEN $2 = '`+ThreadClient+`' THEN p.client_user_id END,
       CASE WHEN $2 = '`+ThreadClient+`' AND p.kind = 'general' THEN p.specialist_user_id END,
       p.assigned_to_user_id,
       $3::uuid
     ) AND u.is_active
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE p.id = $1
ORDER BY 3, 2`, projectID, thread, threadUserID)
}

func (r *Repo) queryParticipants(ctx context.Context, q string, args ...any) ([]Participant, error) {
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("thread participants: %w", err)
	}
	defer rows.Close()
	out := make([]Participant, 0)
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.UserID, &p.Name, &p.Role); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolveCreatorThread — какая ветка принадлежит этому исполнителю.
//
// Развилка одна и она из бизнес-требований: в общем проекте менеджера нет
// вовсе, поэтому исполнитель говорит с клиентом в клиентской ветке. В
// проекте с креаторами под ключ у каждого креатора своя адресная ветка с
// менеджером, и клиент её не видит.
func (r *Repo) ResolveCreatorThread(
	ctx context.Context, projectID, userID uuid.UUID,
) (string, *uuid.UUID, error) {
	var kind string
	var specialist *uuid.UUID
	if err := r.db.QueryRow(ctx,
		`SELECT kind::text, specialist_user_id FROM projects WHERE id = $1`,
		projectID).Scan(&kind, &specialist); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, ErrNotFound
		}
		return "", nil, fmt.Errorf("load project kind: %w", err)
	}
	if kind == "general" && specialist != nil && *specialist == userID {
		return ThreadClient, nil, nil
	}
	var member bool
	if err := r.db.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM project_creators
  WHERE project_id = $1 AND creator_user_id = $2 AND removed_at IS NULL
)`, projectID, userID).Scan(&member); err != nil {
		return "", nil, fmt.Errorf("check creator membership: %w", err)
	}
	if !member {
		return "", nil, ErrNotFound
	}
	return ThreadCreator, &userID, nil
}

// ListEvents — лента активности проекта. Включает display_name актора для UI.
// limit/offset — пагинация (фронт может крутить «загрузить ещё»).
func (r *Repo) ListEvents(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.db.Query(ctx, `
SELECT e.id, e.project_id, e.step_id, e.actor_user_id,
       COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       ) AS actor_display_name,
       e.actor_type, e.event_kind, e.from_status, e.to_status,
       COALESCE(e.comment, ''), e.payload, e.created_at
FROM project_step_events e
LEFT JOIN users u ON u.id = e.actor_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = e.actor_user_id
LEFT JOIN client_profiles cp ON cp.user_id = e.actor_user_id
WHERE e.project_id = $1
ORDER BY e.created_at DESC, e.id DESC
LIMIT $2 OFFSET $3`, projectID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.StepID, &e.ActorID, &e.ActorName,
			&e.ActorType, &e.EventKind, &e.FromStatus, &e.ToStatus,
			&e.Comment, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
