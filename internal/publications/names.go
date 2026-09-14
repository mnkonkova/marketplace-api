package publications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Имена людей в выдаче.
//
// До этого во всех ответах домена стоял только creator_user_id, и в ростере
// менеджера читалось «Креатор 3f2a91b8». Имя живёт не в users, а в
// профилях, поэтому собирается лесенкой: специалист → клиент → префикс
// email. Та же лесенка, что в internal/projects — расходиться им нельзя,
// иначе один и тот же человек подписан по-разному на соседних экранах.
const displayNameExpr = `COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       )`

// Person — участник проекта: кто это и где его аккаунты.
type Person struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"display_name"`
	// AccountLinks — ссылки на аккаунты по пяти площадкам, из профиля
	// специалиста. Менеджеру они нужны, чтобы проверить выкладку глазами,
	// не спрашивая креатора. Площадки, которых у человека нет, просто
	// отсутствуют в карте.
	AccountLinks map[string]string `json:"account_links,omitempty"`
	// AddedAt — когда включён в состав проекта.
	AddedAt time.Time `json:"added_at"`
}

// resolveNames — имена пачкой. Один запрос на весь список, а не запрос на
// строку: списки здесь по 60 выкладок, и N+1 тут заметен сразу.
func (r *Repo) resolveNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT u.id, `+displayNameExpr+`
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp     ON cp.user_id = u.id
WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan name: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ListProjectCreators — состав проекта с именами и ссылками на аккаунты.
// Пришёл на смену ListCreators, который отдавал голые uuid.
func (r *Repo) ListProjectCreators(ctx context.Context, projectID uuid.UUID) ([]Person, error) {
	rows, err := r.db.Query(ctx, `
SELECT pc.creator_user_id, `+displayNameExpr+`, sp.social_links, pc.added_at
FROM project_creators pc
JOIN users u ON u.id = pc.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = pc.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = pc.creator_user_id
WHERE pc.project_id = $1 AND pc.removed_at IS NULL
ORDER BY 2`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project creators: %w", err)
	}
	defer rows.Close()
	out := make([]Person, 0)
	for rows.Next() {
		var p Person
		var links []byte
		if err := rows.Scan(&p.UserID, &p.Name, &links, &p.AddedAt); err != nil {
			return nil, fmt.Errorf("scan project creator: %w", err)
		}
		p.AccountLinks = platformLinks(links)
		out = append(out, p)
	}
	return out, rows.Err()
}

// platformLinks — оставить из social_links только пять наших площадок.
// В профиле лежат ещё behance, сайт и телеграм — в проекте они не при чём,
// а лишние ключи фронт молча покажет как «площадку».
func platformLinks(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var all map[string]string
	if err := json.Unmarshal(raw, &all); err != nil {
		// Профиль с битым JSON не должен ронять выдачу состава проекта.
		return nil
	}
	out := make(map[string]string, len(AllPlatforms))
	for _, p := range AllPlatforms {
		if v := all[p]; v != "" {
			out[p] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
