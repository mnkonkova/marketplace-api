package publications

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Медиана просмотров креатора.
//
// Это то число, которым креатора меряют, когда решают, кого ставить на
// проект, и которым он сам меряет следующий ролик («добавит примерно
// столько-то»). Среднее здесь не годится: один залетевший ролик на
// миллион поднимает среднее вдвое и обещает заказчику то, чего обычно
// не бывает. Медиана отвечает на вопрос «сколько обычно», а спрашивают
// именно это.
//
// Считается по роликам, у которых статистика ЕСТЬ. Ролик, который вышел
// вчера и ещё не собран, — не ноль просмотров, а отсутствие измерения;
// посчитав его нулём, медиану можно уронить вдвое одной свежей
// выкладкой.

// medianMinBasis — сколько измеренных роликов нужно, чтобы медиана
// что-то значила. По двум роликам это не медиана, а полусумма, и
// показывать её как «обычно даёт столько» — врать.
const medianMinBasis = 3

// CreatorMedian — медиана и на скольких роликах она посчитана.
type CreatorMedian struct {
	// Views — медиана просмотров одного ролика.
	Views int64 `json:"views"`
	// Basis — сколько роликов легло в расчёт. Идёт вместе с числом:
	// «медиана 190 тыс. по 12 роликам» и «по 3» — разной силы
	// утверждения, и читателю нужно их различать.
	Basis int `json:"basis"`
}

// medianWindowDays — за какой срок считаем медиану.
//
// Год: «обычно даёт» — это про нынешнюю форму креатора, а не про его
// первый проект. Заодно это потолок стоимости запроса.
const medianWindowDays = 365

// CreatorMedians — медианы пачкой, по всем проектам каждого креатора.
//
// По всем, а не по текущему: спрашивают «сколько он обычно даёт», а не
// «сколько дал здесь». В ответе нет тех, у кого измеренных роликов
// меньше medianMinBasis, — отсутствие честнее числа, которому нельзя
// верить.
func (r *Repo) CreatorMedians(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CreatorMedian, error) {
	out := make(map[uuid.UUID]CreatorMedian, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `
WITH pub_views AS (
    SELECT pub.creator_user_id, pub.id,
           COALESCE(SUM(cur.views), 0)::bigint AS views
    FROM project_publications pub
    JOIN projects pr ON pr.id = pub.project_id
     AND pr.is_test = FALSE AND pr.status <> 'cancelled'
    JOIN publication_links l ON l.publication_id = pub.id
    LEFT JOIN LATERAL (
        SELECT d.views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    WHERE pub.creator_user_id = ANY($1) AND pub.status <> 'cancelled'
      -- Медиана — про «сколько он даёт СЕЙЧАС», и старые ролики к
      -- этому вопросу отношения не имеют. Граница заодно снимает
      -- главную цену запроса: без неё каждый креатор тянул за собой
      -- всю свою историю, по латеральному заходу в замеры на каждую
      -- ссылку, — а зовут это и на подсказке в админском поиске.
      AND pub.created_at >= now() - make_interval(days => $3)
    GROUP BY pub.creator_user_id, pub.id
)
SELECT creator_user_id,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY views)::bigint,
       COUNT(*)::int
FROM pub_views
WHERE views > 0
GROUP BY creator_user_id
HAVING COUNT(*) >= $2`, ids, medianMinBasis, medianWindowDays)
	if err != nil {
		return nil, fmt.Errorf("creator medians: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var m CreatorMedian
		if err := rows.Scan(&id, &m.Views, &m.Basis); err != nil {
			return nil, fmt.Errorf("scan creator median: %w", err)
		}
		out[id] = m
	}
	return out, rows.Err()
}
