package profiles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ClientProfile — контактная информация клиента. Отдельно от
// specialist_profiles — там витрина спецов, у клиента она ни к чему.
// Заполняется самим клиентом в /me, при создании брифа подтягивается
// автоматически (вместо ввода контактов прямо в форме).
type ClientProfile struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Phone       string    `json:"phone"`
	Telegram    string    `json:"telegram"`
}

// ClientProfilePatch — частичный апдейт. nil = не трогать, "" = сбросить.
type ClientProfilePatch struct {
	DisplayName *string `json:"display_name,omitempty"`
	Phone       *string `json:"phone,omitempty"`
	Telegram    *string `json:"telegram,omitempty"`
}

// GetClientProfile — вернёт ClientProfile для userID. Если записи нет,
// возвращает пустую (display_name="" и т.д.) — это валидно, юзер просто
// ничего не заполнил.
func (r *Repo) GetClientProfile(ctx context.Context, userID uuid.UUID) (ClientProfile, error) {
	cp := ClientProfile{UserID: userID}
	err := r.db.QueryRow(ctx, `
SELECT display_name, phone, telegram FROM client_profiles WHERE user_id = $1`,
		userID).Scan(&cp.DisplayName, &cp.Phone, &cp.Telegram)
	if err == nil {
		return cp, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return cp, nil // ещё не заполнял — это норм, отдаём пустую
	}
	return ClientProfile{}, fmt.Errorf("get client profile: %w", err)
}

// PatchClientProfile — upsert. INSERT если нет, UPDATE только переданных полей.
func (r *Repo) PatchClientProfile(ctx context.Context, userID uuid.UUID, in ClientProfilePatch) (ClientProfile, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ClientProfile{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Гарантируем существование строки.
	if _, err := tx.Exec(ctx, `
INSERT INTO client_profiles (user_id) VALUES ($1)
ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return ClientProfile{}, fmt.Errorf("upsert: %w", err)
	}

	// Применяем переданные.
	if in.DisplayName != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE client_profiles SET display_name = $2, updated_at = now() WHERE user_id = $1`,
			userID, strings.TrimSpace(*in.DisplayName)); err != nil {
			return ClientProfile{}, fmt.Errorf("update display_name: %w", err)
		}
	}
	if in.Phone != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE client_profiles SET phone = $2, updated_at = now() WHERE user_id = $1`,
			userID, strings.TrimSpace(*in.Phone)); err != nil {
			return ClientProfile{}, fmt.Errorf("update phone: %w", err)
		}
	}
	if in.Telegram != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE client_profiles SET telegram = $2, updated_at = now() WHERE user_id = $1`,
			userID, strings.TrimSpace(*in.Telegram)); err != nil {
			return ClientProfile{}, fmt.Errorf("update telegram: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ClientProfile{}, fmt.Errorf("commit: %w", err)
	}
	return r.GetClientProfile(ctx, userID)
}

// Валидация: разумные пределы длин.
func ValidateClientPatch(in ClientProfilePatch) error {
	if in.DisplayName != nil && utf8.RuneCountInString(*in.DisplayName) > 120 {
		return fmt.Errorf("%w: display_name too long", ErrInvalidInput)
	}
	if in.Phone != nil && utf8.RuneCountInString(*in.Phone) > 32 {
		return fmt.Errorf("%w: phone too long", ErrInvalidInput)
	}
	if in.Telegram != nil && utf8.RuneCountInString(*in.Telegram) > 64 {
		return fmt.Errorf("%w: telegram too long", ErrInvalidInput)
	}
	return nil
}

// GetClientProfile / PatchClientProfile на сервисе — без extra-логики,
// просто прокидываем.

func (s *Service) GetClientProfile(ctx context.Context, userID uuid.UUID) (ClientProfile, error) {
	return s.repo.GetClientProfile(ctx, userID)
}

func (s *Service) PatchClientProfile(ctx context.Context, userID uuid.UUID, in ClientProfilePatch) (ClientProfile, error) {
	if err := ValidateClientPatch(in); err != nil {
		return ClientProfile{}, err
	}
	return s.repo.PatchClientProfile(ctx, userID, in)
}
