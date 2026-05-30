package invites

import (
	"time"

	"github.com/google/uuid"
)

// Invite — DB-форма записи клиентского инвайта. token_hash хранится
// как bcrypt от raw_token (см. репо). raw_token уходит ОДИН раз в email
// при генерации и больше нигде не лежит — даже дамп БД его не восстановит.
type Invite struct {
	ID        uuid.UUID  `json:"id"`
	UserID    uuid.UUID  `json:"user_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// GenerateInput — что нужно для выпуска нового инвайта.
type GenerateInput struct {
	UserID    uuid.UUID
	CreatedBy uuid.UUID     // менеджер, который сгенерил; 0 → service-token (auto)
	TTL       time.Duration // 0 → default 7 дней (см. service.go)
}

// GenerateResult — то, что отдаём наружу при выпуске. RawToken УЕЗЖАЕТ
// клиенту в email и в БД не сохраняется (в БД только bcrypt от него).
// Формат RawToken: "<invite_id>.<random_b64url>" — compound для
// O(1) lookup при redeem (см. repo.Redeem).
type GenerateResult struct {
	InviteID  uuid.UUID `json:"invite_id"`
	RawToken  string    `json:"token"` // "<invite_id>.<random>"
	ExpiresAt time.Time `json:"expires_at"`
}
