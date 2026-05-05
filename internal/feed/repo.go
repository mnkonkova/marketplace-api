package feed

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// LoadVideosByUsers — батчевый запрос видео-портфолио для набора user_id.
// Возвращает map user_id → отсортированный список видео (sort_order, created_at DESC),
// обрезанный до perSpecialist на спеца. Делаем один SQL вместо N запросов —
// при K=10 спецов это критично для p50.
func (r *Repo) LoadVideosByUsers(
	ctx context.Context,
	userIDs []uuid.UUID,
	perSpecialist int,
) (map[uuid.UUID][]Video, error) {
	if len(userIDs) == 0 || perSpecialist <= 0 {
		return map[uuid.UUID][]Video{}, nil
	}

	// row_number разрезает выдачу по спецу — берём первые perSpecialist
	// в нужном порядке. Так не тянем «лишние» видео для тех, у кого их 50.
	const q = `
SELECT user_id, id, COALESCE(video_url, ''), COALESCE(thumbnail_url, ''),
       title, description,
       duration_sec, COALESCE(aspect, ''),
       created_at
FROM (
    SELECT *, row_number() OVER (PARTITION BY user_id ORDER BY sort_order, created_at DESC) AS rn
    FROM portfolio_items
    WHERE kind = 'video'
      AND user_id = ANY($1)
      AND COALESCE(video_url, '') <> ''
) t
WHERE rn <= $2
ORDER BY user_id, sort_order, created_at DESC`

	rows, err := r.db.Query(ctx, q, userIDs, perSpecialist)
	if err != nil {
		return nil, fmt.Errorf("load videos: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]Video, len(userIDs))
	for rows.Next() {
		var (
			uid uuid.UUID
			v   Video
			dur *int
		)
		if err := rows.Scan(
			&uid, &v.ID, &v.URL, &v.Thumb,
			&v.Title, &v.Description,
			&dur, &v.Aspect,
			&v.CreatedAt,
		); err != nil {
			return nil, err
		}
		v.DurationSec = dur
		out[uid] = append(out[uid], v)
	}
	return out, rows.Err()
}
