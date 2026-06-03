package profiles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ClientProfile — контактная информация клиента. Отдельно от
// specialist_profiles — там витрина спецов, у клиента она ни к чему.
// Заполняется самим клиентом в /me, при создании брифа подтягивается
// автоматически (вместо ввода контактов прямо в форме).
type ClientProfile struct {
	UserID      uuid.UUID  `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Phone       string     `json:"phone"`
	Telegram    string     `json:"telegram"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// ClientProfilePatch — частичный апдейт. nil = не трогать, "" = сбросить.
// UpdatedAt — optimistic-lock версия client_profiles.updated_at, прислана
// фронтом из GET. nil = без проверки версии (back-compat для старого клиента).
type ClientProfilePatch struct {
	DisplayName *string    `json:"display_name,omitempty"`
	Phone       *string    `json:"phone,omitempty"`
	Telegram    *string    `json:"telegram,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// GetClientProfile — вернёт ClientProfile для userID. Если записи нет,
// возвращает пустую (display_name="" и т.д., UpdatedAt=nil) — это валидно,
// юзер просто ничего не заполнил.
func (r *Repo) GetClientProfile(ctx context.Context, userID uuid.UUID) (ClientProfile, error) {
	cp := ClientProfile{UserID: userID}
	var updatedAt time.Time
	err := r.db.QueryRow(ctx, `
SELECT display_name, phone, telegram, updated_at FROM client_profiles WHERE user_id = $1`,
		userID).Scan(&cp.DisplayName, &cp.Phone, &cp.Telegram, &updatedAt)
	if err == nil {
		cp.UpdatedAt = &updatedAt
		return cp, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return cp, nil // ещё не заполнял — это норм, отдаём пустую без updated_at
	}
	return ClientProfile{}, fmt.Errorf("get client profile: %w", err)
}

// PatchClientProfile — upsert одной транзакцией:
//   - INSERT-on-conflict гарантирует существование строки;
//   - один UPDATE с COALESCE применяет только переданные поля;
//   - optimistic-lock по updated_at (если клиент прислал ожидание): иначе
//     два таба, одновременно сохраняющих контакты, перетирают друг друга
//     last-write-wins без 409 — несимметрично с specialist-профилем.
//
// При первом сохранении (строка создана через INSERT ON CONFLICT) клиент
// присылает UpdatedAt=nil → проверка отключена.
func (r *Repo) PatchClientProfile(ctx context.Context, userID uuid.UUID, in ClientProfilePatch) (ClientProfile, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ClientProfile{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Гарантируем существование строки. Свежевставленная имеет updated_at=now(),
	// что не совпадёт с присланным UpdatedAt от старой версии — но клиент при
	// первом PATCH прислает nil, так что проверка не сработает.
	if _, err := tx.Exec(ctx, `
INSERT INTO client_profiles (user_id) VALUES ($1)
ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return ClientProfile{}, fmt.Errorf("upsert: %w", err)
	}

	var dn, ph, tg *string
	if in.DisplayName != nil {
		v := strings.TrimSpace(*in.DisplayName)
		dn = &v
	}
	if in.Phone != nil {
		v := strings.TrimSpace(*in.Phone)
		ph = &v
	}
	if in.Telegram != nil {
		v := strings.TrimSpace(*in.Telegram)
		tg = &v
	}

	tag, err := tx.Exec(ctx, `
UPDATE client_profiles SET
  display_name = COALESCE($2, display_name),
  phone        = COALESCE($3, phone),
  telegram     = COALESCE($4, telegram),
  updated_at   = now()
WHERE user_id = $1
  AND ($5::timestamptz IS NULL OR updated_at = $5)`,
		userID, dn, ph, tg, in.UpdatedAt)
	if err != nil {
		return ClientProfile{}, fmt.Errorf("update client profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Нашёлся только нюанс: до UPDATE мы вставили (если её не было) строку
		// через ON CONFLICT DO NOTHING, поэтому строка точно существует. 0
		// строк → версия не совпала.
		if in.UpdatedAt != nil {
			return ClientProfile{}, ErrConflict
		}
		// UpdatedAt=nil + 0 строк теоретически невозможно — оставляем
		// обобщённую ошибку через тот же путь.
		return ClientProfile{}, ErrConflict
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
