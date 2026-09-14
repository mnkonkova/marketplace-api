package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Команда: кто работает в CRM и сколько на ком висит.
//
// Список менеджеров уже был (/admin/managers), но отвечал на другой
// вопрос — «кого одобрить». Здесь нужен состав целиком, вместе с
// админами, и нагрузка: сколько проектов ведёт и сколько из них
// просрочено. Без второго числа «двадцать проектов» ничего не значит:
// двадцать идущих по графику — это норма, три просроченных — нет.

// overdueProjectCond — SQL-условие «проект просрочен». Одно на весь
// пакет: нагрузка в списке команды и пункт сводки должны считать
// просрочку одинаково, иначе два экрана рядом показывают разные числа.
//
// Просрочка бывает двух видов, и обе одинаково означают «сроки сорваны»:
// прошёл срок самого проекта либо прошла дата невыложенной выкладки.
const overdueProjectCond = `(
    (p.due_date IS NOT NULL AND p.due_date < CURRENT_DATE)
 OR EXISTS (SELECT 1 FROM project_publications pub
            WHERE pub.project_id = p.id
              AND pub.status IN ('planned','partial')
              AND pub.due_date < CURRENT_DATE)
)`

// TeamMember — строка списка команды.
type TeamMember struct {
	UserID      uuid.UUID `json:"user_id"`
	Email       string    `json:"email,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	IsAdmin     bool      `json:"is_admin"`
	IsManager   bool      `json:"is_manager"`
	IsApproved  bool      `json:"is_approved"`
	IsActive    bool      `json:"is_active"`
	// ActiveProjects — незавершённые проекты, где человек ответственный.
	ActiveProjects int `json:"active_projects"`
	// OverdueProjects — из них те, где сроки уже сорваны.
	OverdueProjects int `json:"overdue_projects"`
	// LastLoginAt — nil означает «не входил ни разу», а не «давно»:
	// сотрудника, который так и не зашёл, надо позвать.
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ListTeam — админы и менеджеры. Тестовых не показываем: их в базе после
// прогонов больше, чем настоящих сотрудников.
func (r *Repo) ListTeam(ctx context.Context) ([]TeamMember, error) {
	rows, err := r.db.Query(ctx, `
SELECT u.id, COALESCE(u.email::text, ''),
       COALESCE(NULLIF(u.display_name, ''), NULLIF(sp.display_name, ''), ''),
       u.is_admin, u.is_manager, u.is_approved, u.is_active,
       (SELECT COUNT(*) FROM projects p
        WHERE p.assigned_to_user_id = u.id AND p.is_test = FALSE
          AND p.status IN ('draft','active','on_hold','dispute')),
       (SELECT COUNT(*) FROM projects p
        WHERE p.assigned_to_user_id = u.id AND p.is_test = FALSE
          AND p.status IN ('draft','active','on_hold','dispute')
          AND `+overdueProjectCond+`),
       u.last_login_at, u.created_at
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE (u.is_admin = TRUE OR u.is_manager = TRUE) AND u.is_test = FALSE
ORDER BY u.is_approved ASC, u.is_admin DESC, u.created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list team: %w", err)
	}
	defer rows.Close()
	out := make([]TeamMember, 0)
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName,
			&m.IsAdmin, &m.IsManager, &m.IsApproved, &m.IsActive,
			&m.ActiveProjects, &m.OverdueProjects, &m.LastLoginAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan team member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListTeam — состав CRM с нагрузкой.
func (s *Service) ListTeam(ctx context.Context) ([]TeamMember, error) {
	return s.repo.ListTeam(ctx)
}

type teamResp struct {
	Items []TeamMember `json:"items"`
}

// AdminListTeam godoc
// @Summary  Команда: админы и менеджеры с нагрузкой
// @Description У каждого — сколько незавершённых проектов он ведёт,
// @Description сколько из них просрочено (прошёл срок проекта либо дата
// @Description невыложенной выкладки) и когда он последний раз входил.
// @Description last_login_at отсутствует = не входил ни разу.
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} teamResp
// @Router   /admin/team [get]
func (h *Handler) AdminListTeam(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListTeam(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, teamResp{Items: items})
}
