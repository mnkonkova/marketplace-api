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
