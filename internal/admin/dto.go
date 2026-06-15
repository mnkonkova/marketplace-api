package admin

import (
	"time"

	"github.com/google/uuid"
)

// ManagerInfo — карточка менеджера в админ-списке. Поверх email/role/
// is_approved добавляем display_name (если есть в specialist_profiles)
// и счётчик assigned проектов для приоритизации аппрува.
type ManagerInfo struct {
	UserID          uuid.UUID `json:"user_id"`
	Email           string    `json:"email,omitempty"`
	DisplayName     string    `json:"display_name,omitempty"`
	IsActive        bool      `json:"is_active"`
	IsApproved      bool      `json:"is_approved"`
	EmailVerified   bool      `json:"email_verified"`
	AssignedProjects int      `json:"assigned_projects"`
	CreatedAt       time.Time `json:"created_at"`
}

// CreateClientInput — админ заводит клиента вручную (kind=client, role=client).
// Email обязателен — иначе magic-link не сможем отправить.
type CreateClientInput struct {
	Email       string
	DisplayName string
}

// CreateClientResult — id созданного юзера + опциональный raw-токен инвайта
// (только в этом ответе). Если WithInvite=false — Token пустой.
type CreateClientResult struct {
	UserID      uuid.UUID `json:"user_id"`
	InviteToken string    `json:"invite_token,omitempty"`
	InviteURL   string    `json:"invite_url,omitempty"`
}

// InviteGenerateResult — выдан новый magic-link для существующего юзера.
type InviteGenerateResult struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// UserSearchResult — компактное представление юзера для лукапов
// в admin/manager интерфейсе (создать проект для существующего клиента,
// назначить спеца). Имя берётся из client_profiles или specialist_profiles
// в зависимости от kind; пустая строка если профиля нет.
type UserSearchResult struct {
	UserID      uuid.UUID `json:"user_id"`
	Email       string    `json:"email,omitempty"`
	Phone       string    `json:"phone,omitempty"`
	Kind        string    `json:"kind"`
	DisplayName string    `json:"display_name,omitempty"`
}

// UserListItem — строка для полного admin-листинга /admin/users.
// Объединяет users + LEFT JOIN на оба профиля для display_name.
type UserListItem struct {
	UserID        uuid.UUID `json:"user_id"`
	Email         string    `json:"email,omitempty"`
	Phone         string    `json:"phone,omitempty"`
	DisplayName   string    `json:"display_name,omitempty"`
	Kind          string    `json:"kind"`
	IsAdmin       bool      `json:"is_admin"`
	IsManager     bool      `json:"is_manager"`
	IsApproved    bool      `json:"is_approved"`
	IsActive      bool      `json:"is_active"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

// ListAllUsersParams — фильтры и пагинация для /admin/users.
// Все поля опциональны. Limit принудительно clamp'ится 1..100.
type ListAllUsersParams struct {
	Q      string // поиск ILIKE по email/phone/display_name; <2 символов игнорируется
	Kind   string // "client" | "specialist" | "" (без фильтра)
	Role   string // "manager" | "admin" | "regular" | "" (regular = !is_manager && !is_admin)
	Limit  int
	Offset int
}

// UserListResult — ответ пагинированного листинга. Total — общее количество
// под текущими фильтрами (без limit/offset), для отрисовки пагинатора.
type UserListResult struct {
	Items  []UserListItem `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}
