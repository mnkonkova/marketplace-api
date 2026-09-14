package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
	"marketpclce/internal/httpx"
)

// Карточка человека в админке.
//
// Раньше «посмотреть человека» означало искать его в четырёх местах:
// строка в /admin/users, решение модерации — в очереди, проекты — поиском
// по имени в списке проектов, история — в логах. Ровно на этом рассыпался
// разбор любого спорного случая: половину данных искали руками, и
// половину не находили.

// UserProjectRef — проект, в котором человек участвует.
type UserProjectRef struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	// Role — кем он в этом проекте: client | specialist | manager. Один и
	// тот же человек бывает и заказчиком, и исполнителем, поэтому проект
	// может попасть в список дважды с разными ролями.
	Role      string    `json:"role"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	IsTest    bool      `json:"is_test"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ModerationInfo — решение модерации по специалисту. nil у тех, у кого
// профиля специалиста нет: у клиента модерации не бывает.
type ModerationInfo struct {
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy  *uuid.UUID `json:"reviewed_by,omitempty"`
	IsPublished bool       `json:"is_published"`
}

// UserCard — всё о человеке на одном экране.
type UserCard struct {
	UserListItem
	// EmailVerifiedAt — когда подтверждена почта. Сам факт лежит в
	// UserListItem.EmailVerified; дата нужна там, где выясняют,
	// подтверждал ли человек почту сам или это сделал админ.
	EmailVerifiedAt *time.Time      `json:"email_verified_at,omitempty"`
	Moderation      *ModerationInfo `json:"moderation,omitempty"`
	// Projects — участие во всех ролях, свежие сверху. Никогда не null.
	Projects []UserProjectRef `json:"projects"`
	// Audit — записи журнала: и те, где он объект, и его собственные
	// действия. Пустой список, если журнал по нему молчит.
	Audit []audit.Entry `json:"audit"`
}

// userProjectsLimit — сколько проектов показываем в карточке. У клиента
// их единицы, у менеджера — сотни, а карточка не место для сотни строк:
// дальше идут в список проектов с фильтром по менеджеру.
const userProjectsLimit = 50

// GetUserCard — профиль, роли, решение модерации и проекты человека.
// Журнал прикладывает сервис: он живёт в отдельном пакете.
func (r *Repo) GetUserCard(ctx context.Context, userID uuid.UUID) (UserCard, error) {
	var (
		c         UserCard
		modStatus *string
		modReason *string
		modAt     *time.Time
		modBy     *uuid.UUID
		published bool
	)
	err := r.db.QueryRow(ctx, `
SELECT u.id, COALESCE(u.email::text, ''), COALESCE(u.phone, ''),
       COALESCE(NULLIF(u.display_name, ''), NULLIF(cp.display_name, ''), NULLIF(sp.display_name, ''), ''),
       u.kind, u.is_admin, u.is_manager, u.is_approved, u.is_active,
       u.email_verified_at IS NOT NULL, u.email_verified_at,
       u.created_at, u.last_login_at, u.is_test,
       sp.moderation_status, sp.moderation_reason, sp.moderation_reviewed_at,
       sp.moderation_reviewed_by, COALESCE(sp.is_published, FALSE)
FROM users u
LEFT JOIN client_profiles     cp ON cp.user_id = u.id
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE u.id = $1`, userID).Scan(
		&c.UserID, &c.Email, &c.Phone, &c.DisplayName,
		&c.Kind, &c.IsAdmin, &c.IsManager, &c.IsApproved, &c.IsActive,
		&c.EmailVerified, &c.EmailVerifiedAt,
		&c.CreatedAt, &c.LastLoginAt, &c.IsTest,
		&modStatus, &modReason, &modAt, &modBy, &published)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserCard{}, ErrNotFound
	}
	if err != nil {
		return UserCard{}, fmt.Errorf("get user card: %w", err)
	}
	c.IsPublished = published
	if modStatus != nil {
		c.ModerationStatus = *modStatus
		m := ModerationInfo{Status: *modStatus, ReviewedAt: modAt, ReviewedBy: modBy, IsPublished: published}
		if modReason != nil {
			m.Reason = *modReason
		}
		c.Moderation = &m
	}

	// Три роли — три условия в одном запросе. UNION ALL, а не OR по трём
	// колонкам: проект, где человек и заказчик, и исполнитель, должен
	// попасть в список дважды — иначе одна из его ролей исчезает.
	rows, err := r.db.Query(ctx, `
SELECT id, title, role, kind::text, status::text, is_test, updated_at
FROM (
    SELECT p.id, p.title, 'client'     AS role, p.kind, p.status, p.is_test, p.updated_at
    FROM projects p WHERE p.client_user_id = $1
    UNION ALL
    SELECT p.id, p.title, 'specialist' AS role, p.kind, p.status, p.is_test, p.updated_at
    FROM projects p WHERE p.specialist_user_id = $1
    UNION ALL
    SELECT p.id, p.title, 'manager'    AS role, p.kind, p.status, p.is_test, p.updated_at
    FROM projects p WHERE p.assigned_to_user_id = $1
) t
ORDER BY updated_at DESC
LIMIT $2`, userID, userProjectsLimit)
	if err != nil {
		return UserCard{}, fmt.Errorf("user projects: %w", err)
	}
	defer rows.Close()
	c.Projects = []UserProjectRef{}
	for rows.Next() {
		var p UserProjectRef
		if err := rows.Scan(&p.ID, &p.Title, &p.Role, &p.Kind, &p.Status, &p.IsTest, &p.UpdatedAt); err != nil {
			return UserCard{}, fmt.Errorf("scan user project: %w", err)
		}
		c.Projects = append(c.Projects, p)
	}
	return c, rows.Err()
}

// GetUserCard — карточка человека вместе с журналом по нему.
func (s *Service) GetUserCard(ctx context.Context, userID uuid.UUID) (UserCard, error) {
	card, err := s.repo.GetUserCard(ctx, userID)
	if err != nil {
		return UserCard{}, err
	}
	card.Audit = []audit.Entry{}
	if s.audit == nil {
		// Журнал не подключён (сборка без него) — карточка всё равно
		// полезна, пустой список честнее 500-й ошибки.
		return card, nil
	}
	entries, err := s.audit.ListAboutUser(ctx, userID, userCardAuditLimit)
	if err != nil {
		return UserCard{}, err
	}
	card.Audit = entries
	return card, nil
}

// userCardAuditLimit — сколько записей журнала кладём в карточку.
// Полная история — в /admin/audit с фильтром по этому человеку.
const userCardAuditLimit = 30

// AdminGetUser godoc
// @Summary  Карточка человека: профиль, роли, модерация, проекты, журнал
// @Description Проекты — во всех ролях сразу (заказчик, исполнитель,
// @Description ответственный менеджер). last_login_at отсутствует =
// @Description не входил ни разу.
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id"
// @Success  200 {object} UserCard
// @Failure  404 {object} errorResponse "not_found"
// @Router   /admin/users/{id} [get]
func (h *Handler) AdminGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	card, err := h.svc.GetUserCard(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, card)
}
