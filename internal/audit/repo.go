package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo — чтение журнала. Писать через него нельзя намеренно: запись
// живёт в транзакции действия (см. Write), и метод «записать пулом»
// рядом с ним первым же копипастом превратил бы журнал в приблизительный.
type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// Filter — отбор записей журнала. Все поля опциональны.
type Filter struct {
	ActorID    *uuid.UUID
	Action     string
	ObjectType string
	ObjectID   string
	// From/To — окно по created_at, полуинтервал [From, To).
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

// List — страница журнала, свежие сверху. Возвращает (items, total), где
// total — количество под теми же фильтрами без limit/offset.
func (r *Repo) List(ctx context.Context, f Filter) ([]Entry, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	// Тот же порог, что у /admin/users и /admin/projects: большие OFFSET
	// в PG дорогие, а до 200-й страницы журнала не долистывают — туда
	// приходят фильтром.
	if f.Offset > 10000 {
		return nil, 0, fmt.Errorf("invalid offset: max 10000 (используйте фильтры)")
	}

	args := []any{}
	conds := []string{}
	if f.ActorID != nil {
		args = append(args, *f.ActorID)
		conds = append(conds, fmt.Sprintf("a.actor_user_id = $%d", len(args)))
	}
	if f.Action != "" {
		args = append(args, f.Action)
		conds = append(conds, fmt.Sprintf("a.action = $%d", len(args)))
	}
	if f.ObjectType != "" {
		args = append(args, f.ObjectType)
		conds = append(conds, fmt.Sprintf("a.object_type = $%d", len(args)))
	}
	if f.ObjectID != "" {
		args = append(args, f.ObjectID)
		conds = append(conds, fmt.Sprintf("a.object_id = $%d", len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		conds = append(conds, fmt.Sprintf("a.created_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		conds = append(conds, fmt.Sprintf("a.created_at < $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := r.db.QueryRow(ctx,
		"SELECT COUNT(*) FROM admin_audit_log a "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit: %w", err)
	}

	args = append(args, f.Limit, f.Offset)
	q := fmt.Sprintf(`
SELECT a.id, a.actor_user_id, a.action, a.object_type, a.object_id, a.payload, a.created_at,
       COALESCE(u.email::text, ''), COALESCE(NULLIF(u.display_name, ''), NULLIF(sp.display_name, ''), '')
FROM admin_audit_log a
LEFT JOIN users u ON u.id = a.actor_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = a.actor_user_id
%s
ORDER BY a.created_at DESC, a.id DESC
LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	items := make([]Entry, 0, f.Limit)
	for rows.Next() {
		var (
			e   Entry
			raw []byte
		)
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.Action, &e.ObjectType, &e.ObjectID,
			&raw, &e.CreatedAt, &e.ActorEmail, &e.ActorDisplayName); err != nil {
			return nil, 0, fmt.Errorf("scan audit: %w", err)
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &e.Payload)
		}
		items = append(items, e)
	}
	return items, total, rows.Err()
}

// ListAboutUser — записи, касающиеся конкретного человека: и те, где он
// объект (роль сняли, почту подтвердили), и те, где он сам действовал.
// Оба ответа нужны в одной карточке: сотрудник в ней и тот, кого решают,
// и тот, кто решает.
func (r *Repo) ListAboutUser(ctx context.Context, userID uuid.UUID, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
SELECT a.id, a.actor_user_id, a.action, a.object_type, a.object_id, a.payload, a.created_at,
       COALESCE(u.email::text, ''), COALESCE(NULLIF(u.display_name, ''), NULLIF(sp.display_name, ''), '')
FROM admin_audit_log a
LEFT JOIN users u ON u.id = a.actor_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = a.actor_user_id
WHERE (a.object_type = $1 AND a.object_id = $2) OR a.actor_user_id = $3
ORDER BY a.created_at DESC, a.id DESC
LIMIT $4`, ObjectUser, userID.String(), userID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit about user: %w", err)
	}
	defer rows.Close()
	out := make([]Entry, 0, limit)
	for rows.Next() {
		var (
			e   Entry
			raw []byte
		)
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.Action, &e.ObjectType, &e.ObjectID,
			&raw, &e.CreatedAt, &e.ActorEmail, &e.ActorDisplayName); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
