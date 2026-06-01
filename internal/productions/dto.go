package productions

import (
	"time"

	"github.com/google/uuid"
)

// Production — справочник продакшен-студий. Управляется админом, выбирается
// специалистом в профиле (production_id XOR is_freelance). Имя уникально
// среди активных (можно деактивировать и завести нового с тем же именем).
type Production struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CreateInput struct {
	Name        string
	Description string
}

type PatchInput struct {
	Name        *string
	Description *string
	IsActive    *bool
}
