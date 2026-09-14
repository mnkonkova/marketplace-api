package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"marketpclce/internal/httpx"
)

// Быстрый поиск по всей админке — то, что открывается на ⌘K.
//
// Отдельная ручка, а не два запроса с фронта: человек, которого ищут,
// почти всегда ищется вместе со своим проектом («Петров» — это и клиент,
// и его проект), и два независимых запроса с разными лимитами дают
// перекошенную выдачу — десять пользователей и ни одного проекта.

// globalSearchLimit — сколько строк отдаём в каждой части. Это не список,
// а подсказка: дальше идут в полный список с тем же запросом.
const globalSearchLimit = 10

// SearchProjectHit — проект в быстром поиске.
type SearchProjectHit struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	// ClientName — по чьему имени проект чаще всего и ищут.
	ClientName string    `json:"client_name,omitempty"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	IsTest     bool      `json:"is_test"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SearchUserHit — человек в быстром поиске.
type SearchUserHit struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email,omitempty"`
	Phone       string    `json:"phone,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Kind        string    `json:"kind"`
	IsAdmin     bool      `json:"is_admin"`
	IsManager   bool      `json:"is_manager"`
	IsActive    bool      `json:"is_active"`
}

// GlobalSearchResult — обе части сразу. Списки никогда не null: пустой
// список означает «не нашли», отсутствующий — «не искали», и для
// подсказки это разные ответы.
type GlobalSearchResult struct {
	Projects []SearchProjectHit `json:"projects"`
	Users    []SearchUserHit    `json:"users"`
}

// GlobalSearch — проекты и люди по одной строке. Короче двух символов —
// пусто: по одной букве совпадёт всё, и подсказка станет шумом.
//
// Тестовые не показываем: в подсказке они вытесняют настоящие строки,
// а ищут в ней всегда настоящее.
func (r *Repo) GlobalSearch(ctx context.Context, q string) (GlobalSearchResult, error) {
	out := GlobalSearchResult{Projects: []SearchProjectHit{}, Users: []SearchUserHit{}}
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) < 2 {
		return out, nil
	}
	// data-sec D11: экранируем метасимволы LIKE — иначе '%' в строке
	// поиска означает «любая строка», а '_' — любой символ.
	likeEsc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	pattern := "%" + likeEsc.Replace(q) + "%"

	rows, err := r.db.Query(ctx, `
SELECT p.id, p.title,
       COALESCE(NULLIF(cp.display_name, ''), NULLIF(p.client_name, ''), COALESCE(u.email::text, '')),
       p.kind::text, p.status::text, p.is_test, p.updated_at
FROM projects p
LEFT JOIN users           u  ON u.id = p.client_user_id
LEFT JOIN client_profiles cp ON cp.user_id = p.client_user_id
WHERE p.is_test = FALSE
  AND (
      p.title ILIKE $1 ESCAPE '\'
   OR COALESCE(p.client_name, '') ILIKE $1 ESCAPE '\'
   OR COALESCE(cp.display_name, '') ILIKE $1 ESCAPE '\'
   OR COALESCE(u.email::text, '') ILIKE $1 ESCAPE '\'
  )
ORDER BY p.updated_at DESC
LIMIT $2`, pattern, globalSearchLimit)
	if err != nil {
		return GlobalSearchResult{}, fmt.Errorf("search projects: %w", err)
	}
	for rows.Next() {
		var h SearchProjectHit
		if err := rows.Scan(&h.ID, &h.Title, &h.ClientName, &h.Kind, &h.Status,
			&h.IsTest, &h.UpdatedAt); err != nil {
			rows.Close()
			return GlobalSearchResult{}, fmt.Errorf("scan project hit: %w", err)
		}
		out.Projects = append(out.Projects, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GlobalSearchResult{}, err
	}

	// Людей ищем и неактивных: «куда делся Петров» — законный вопрос к
	// поиску, и ответ «заблокирован» полезнее, чем пустая выдача.
	urows, err := r.db.Query(ctx, `
SELECT u.id, COALESCE(u.email::text, ''), COALESCE(u.phone, ''),
       COALESCE(NULLIF(u.display_name, ''), NULLIF(cp.display_name, ''), NULLIF(sp.display_name, ''), ''),
       u.kind, u.is_admin, u.is_manager, u.is_active
FROM users u
LEFT JOIN client_profiles     cp ON cp.user_id = u.id
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE u.is_test = FALSE
  AND (
      u.email::text ILIKE $1 ESCAPE '\'
   OR COALESCE(u.phone, '') ILIKE $1 ESCAPE '\'
   OR COALESCE(u.display_name, '') ILIKE $1 ESCAPE '\'
   OR COALESCE(cp.display_name, '') ILIKE $1 ESCAPE '\'
   OR COALESCE(sp.display_name, '') ILIKE $1 ESCAPE '\'
  )
ORDER BY u.is_active DESC, u.created_at DESC
LIMIT $2`, pattern, globalSearchLimit)
	if err != nil {
		return GlobalSearchResult{}, fmt.Errorf("search users: %w", err)
	}
	defer urows.Close()
	for urows.Next() {
		var h SearchUserHit
		if err := urows.Scan(&h.ID, &h.Email, &h.Phone, &h.DisplayName, &h.Kind,
			&h.IsAdmin, &h.IsManager, &h.IsActive); err != nil {
			return GlobalSearchResult{}, fmt.Errorf("scan user hit: %w", err)
		}
		out.Users = append(out.Users, h)
	}
	return out, urows.Err()
}

// GlobalSearch — быстрый поиск по проектам и людям.
func (s *Service) GlobalSearch(ctx context.Context, q string) (GlobalSearchResult, error) {
	return s.repo.GlobalSearch(ctx, q)
}

// AdminGlobalSearch godoc
// @Summary  Быстрый поиск по проектам и людям (⌘K)
// @Description Одна строка ищется и по названию проекта с именем клиента,
// @Description и по почте, телефону и имени человека. Короче двух символов —
// @Description пустой ответ. Тестовые записи не показываются.
// @Tags     admin-search
// @Produce  json
// @Security BearerAuth
// @Param    q query string true "строка поиска, мин 2 символа"
// @Success  200 {object} GlobalSearchResult
// @Router   /admin/search [get]
func (h *Handler) AdminGlobalSearch(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.GlobalSearch(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
