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

// ListComments — список не-удалённых комментариев проекта с display_name автора.
// Имя ищем последовательно: specialist_profile → client_profile → префикс email
// (до '@'). includeInternal=false скрывает is_internal=true (клиентская выдача).
func (r *Repo) ListComments(ctx context.Context, projectID uuid.UUID, includeInternal bool) ([]Comment, error) {
	rows, err := r.db.Query(ctx, `
SELECT pc.id, pc.project_id, pc.author_id,
       COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       ) AS author_name,
       pc.body, pc.body_format, pc.is_internal, pc.created_at, pc.updated_at
FROM project_comments pc
LEFT JOIN users u ON u.id = pc.author_id
LEFT JOIN specialist_profiles sp ON sp.user_id = pc.author_id
LEFT JOIN client_profiles cp ON cp.user_id = pc.author_id
WHERE pc.project_id = $1 AND pc.deleted_at IS NULL
  AND ($2::bool OR pc.is_internal = FALSE)
ORDER BY pc.created_at ASC`, projectID, includeInternal)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	out := make([]Comment, 0)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.AuthorID, &c.AuthorName,
			&c.Body, &c.BodyFormat, &c.IsInternal, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateComment — атомарно: вставка в project_comments + событие
// event_kind='comment' в project_step_events (для ленты активности
// в одном запросе с другими событиями) + бамп projects.updated_at.
// isInternal=true: комментарий не видит клиент (только manager/admin).
func (r *Repo) CreateComment(ctx context.Context, projectID, authorID uuid.UUID, body, bodyFormat string, isInternal bool) (Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Comment{}, ErrCommentEmpty
	}
	if bodyFormat == "" {
		bodyFormat = "plain"
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Comment{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c Comment
	c.ProjectID = projectID
	c.AuthorID = authorID
	c.Body = body
	c.BodyFormat = bodyFormat
	c.IsInternal = isInternal
	if err := tx.QueryRow(ctx, `
INSERT INTO project_comments (project_id, author_id, body, body_format, is_internal)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, created_at, updated_at`,
		projectID, authorID, body, bodyFormat, isInternal).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}

	// Внутренние комментарии не пишем в ленту — иначе клиент увидит их в
	// activity-фиде, даже если /comments отфильтрован.
	if !isInternal {
		if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, comment, payload)
VALUES ($1, NULL, $2, 'human', 'comment', $3, $4)`,
			projectID, authorID, body,
			mustJSON(map[string]string{"comment_id": c.ID.String()})); err != nil {
			return Comment{}, fmt.Errorf("insert comment event: %w", err)
		}
	}

	// Внешний комментарий — событие в outbox (для n8n → Telegram «новое
	// сообщение в проекте»). Внутренние не нотифицируем — клиент их не
	// видит, менеджерская переписка не должна спамить общий канал.
	if !isInternal {
		payload := map[string]any{
			"project_id":  projectID.String(),
			"comment_id":  c.ID.String(),
			"author_id":   authorID.String(),
			"body":        body,
		}
		enrichProjectPayload(ctx, tx, projectID, payload)
		if err := emit(ctx, tx, projectID, nil, authorID, "project.comment_added", payload); err != nil {
			return Comment{}, fmt.Errorf("emit comment event: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET updated_at = now() WHERE id = $1`, projectID); err != nil {
		return Comment{}, fmt.Errorf("bump project: %w", err)
	}

	// R9: display_name автора берём ВНУТРИ tx — между Commit и отдельным
	// SELECT'ом профиль мог быть удалён (или авторские поля стёрты), и в
	// ответе уходил пустой AuthorName. Внутри tx видим консистентный
	// снимок, нагрузка та же — один лёгкий SELECT с двумя LEFT JOIN'ами.
	// Фолбэк-ладдер тот же что в ListComments: specialist_profile →
	// client_profile → email-префикс.
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(
  NULLIF(sp.display_name, ''),
  NULLIF(cp.display_name, ''),
  split_part(u.email, '@', 1),
  ''
)
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp ON cp.user_id = u.id
WHERE u.id = $1`, authorID).Scan(&c.AuthorName); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.AuthorName = ""
	}

	if err := tx.Commit(ctx); err != nil {
		return Comment{}, fmt.Errorf("commit: %w", err)
	}
	return c, nil
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
