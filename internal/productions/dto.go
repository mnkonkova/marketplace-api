package productions

import (
	"time"

	"github.com/google/uuid"
)

// Production — полная запись справочника, возвращается в admin-ответах.
// UpdatedAt — версия для optimistic locking в PATCH /admin/productions/{id}.
type Production struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PublicProduction — урезанная форма для публичного /productions:
// фронту достаточно id и name для выпадающего списка. Description прячем,
// чтобы не светить внутренние пометки админа на публичных страницах.
type PublicProduction struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type CreateInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpdateInput — частичный апдейт. Указатели = «поле задано».
// UpdatedAt — обязателен для concurrent-edit защиты; несовпадение → 409.
type UpdateInput struct {
	Name        *string    `json:"name"`
	Description *string    `json:"description"`
	IsActive    *bool      `json:"is_active"`
	UpdatedAt   *time.Time `json:"updated_at"`
}
