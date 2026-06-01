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

// ListComments — список не-удалённых комментариев проекта с display_name автора
// (best-effort: левый join на specialist_profiles; для клиентов без профиля
// поле останется пустым).
func (r *Repo) ListComments(ctx context.Context, projectID uuid.UUID) ([]Comment, error) {
	rows, err := r.db.Query(ctx, `
SELECT pc.id, pc.project_id, pc.author_id, COALESCE(sp.display_name, ''),
       pc.body, pc.body_format, pc.created_at, pc.updated_at
FROM project_comments pc
LEFT JOIN specialist_profiles sp ON sp.user_id = pc.author_id
WHERE pc.project_id = $1 AND pc.deleted_at IS NULL
ORDER BY pc.created_at ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	out := make([]Comment, 0)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.AuthorID, &c.AuthorName,
			&c.Body, &c.BodyFormat, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateComment — атомарно: вставка в project_comments + событие
// event_kind='comment' в project_step_events (для ленты активности
// в одном запросе с другими событиями) + бамп projects.updated_at.
func (r *Repo) CreateComment(ctx context.Context, projectID, authorID uuid.UUID, body, bodyFormat string) (Comment, error) {
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
	if err := tx.QueryRow(ctx, `
INSERT INTO project_comments (project_id, author_id, body, body_format)
VALUES ($1, $2, $3, $4)
RETURNING id, created_at, updated_at`,
		projectID, authorID, body, bodyFormat).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_step_events
  (project_id, step_id, actor_user_id, actor_type, event_kind, comment, payload)
VALUES ($1, NULL, $2, 'human', 'comment', $3, $4)`,
		projectID, authorID, body,
		fmt.Sprintf(`{"comment_id":"%s"}`, c.ID.String())); err != nil {
		return Comment{}, fmt.Errorf("insert comment event: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE projects SET updated_at = now() WHERE id = $1`, projectID); err != nil {
		return Comment{}, fmt.Errorf("bump project: %w", err)
	}

	// display_name автора — best-effort (вне tx, в Get-ответе).
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, fmt.Errorf("commit: %w", err)
	}

	if err := r.db.QueryRow(ctx,
		`SELECT COALESCE(display_name, '') FROM specialist_profiles WHERE user_id = $1`,
		authorID).Scan(&c.AuthorName); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// не критично — оставим пустым
		c.AuthorName = ""
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
SELECT e.id, e.project_id, e.step_id, e.actor_user_id, COALESCE(sp.display_name, ''),
       e.actor_type, e.event_kind, e.from_status, e.to_status,
       COALESCE(e.comment, ''), e.payload, e.created_at
FROM project_step_events e
LEFT JOIN specialist_profiles sp ON sp.user_id = e.actor_user_id
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
