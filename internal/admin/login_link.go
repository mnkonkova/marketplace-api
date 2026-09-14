package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// Одноразовая ссылка для входа сотруднику.
//
// Отдельный путь, а не послабление в GenerateInvite. Там фильтр
// `is_admin = FALSE AND is_manager = FALSE` стоит не от лишней
// осторожности: ручка /manager/users/{id}/generate_invite доступна
// менеджеру, и без фильтра менеджер выписывал бы токен на UUID админа,
// обменивал его и получал админский доступ (data-sec D1). Снять фильтр —
// значит вернуть эскалацию привилегий.
//
// Поэтому здесь своя функция со своим правилом: цель может быть кем
// угодно, включая админа, но выписать ссылку может только админ, и
// смонтирована ручка только в админской секции роутера.

// staffLoginLinkTTL — 72 часа, а не общий inviteTTL в семь дней.
// Ссылка пускает в чужой аккаунт с правами сотрудника: чем короче окно,
// тем меньше цена забытой в переписке ссылки. Трёх суток хватает, чтобы
// человек дошёл до почты в выходные.
const staffLoginLinkTTL = 72 * time.Hour

// ErrForbiddenActor — ссылку просит не админ. Роль читается из БД в той
// же транзакции: маршрут проверяет её на входе, но между выдачей JWT и
// этим запросом роль могли снять, а токен живёт ещё минуты.
var ErrForbiddenActor = errors.New("only an admin can issue a staff login link")

// ErrInactiveTarget — ссылку просят для отключённого аккаунта. Войти по
// ней всё равно нельзя (middleware режет is_active=FALSE), и выдавать
// её значит обещать то, чего не будет.
var ErrInactiveTarget = errors.New("target user is inactive")

// GenerateStaffLoginLink — одноразовая ссылка входа для сотрудника.
//
// Всё одной транзакцией: проверка прав актора, гашение прошлых ссылок,
// новая запись и строка журнала. Отказ не оставляет ни ссылки, ни следа
// в журнале — «выписал ссылку» в истории должно означать, что ссылка
// действительно выписана.
func (r *Repo) GenerateStaffLoginLink(ctx context.Context, targetID, actorID uuid.UUID) (rawToken string, expiresAt time.Time, err error) {
	// 32 байта → 64 hex-символа, как у обычного инвайта: этого с запасом
	// против перебора.
	var rnd [32]byte
	if _, e := rand.Read(rnd[:]); e != nil {
		return "", time.Time{}, fmt.Errorf("rand: %w", e)
	}
	rawToken = hex.EncodeToString(rnd[:])
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])
	expiresAt = time.Now().Add(staffLoginLinkTTL)

	txErr := r.withTx(ctx, func(tx pgx.Tx) error {
		// Кто просит. Перечитываем из БД: роль в запросе подтверждена
		// токеном, а токен переживает снятие роли.
		var actorIsAdmin bool
		if e := tx.QueryRow(ctx,
			`SELECT is_admin FROM users WHERE id = $1 AND is_active = TRUE`,
			actorID).Scan(&actorIsAdmin); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return ErrForbiddenActor
			}
			return fmt.Errorf("check actor: %w", e)
		}
		if !actorIsAdmin {
			return ErrForbiddenActor
		}

		// Кому. В отличие от GenerateInvite, роль цели не ограничена:
		// ссылка нужна именно сотруднику. Ограничение здесь другое — её
		// выписывает только админ, проверено выше.
		var targetActive bool
		if e := tx.QueryRow(ctx,
			`SELECT is_active FROM users WHERE id = $1`, targetID).Scan(&targetActive); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("check target: %w", e)
		}
		if !targetActive {
			return ErrInactiveTarget
		}

		// Прошлые невостребованные ссылки гасим: одноразовая значит, что
		// действует ровно последняя выданная.
		if _, e := tx.Exec(ctx,
			`UPDATE client_invites SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
			targetID); e != nil {
			return fmt.Errorf("invalidate prev: %w", e)
		}
		if _, e := tx.Exec(ctx,
			`INSERT INTO client_invites (user_id, token_hash, expires_at, created_by)
			 VALUES ($1, $2, $3, $4)`,
			targetID, tokenHash, expiresAt, actorID); e != nil {
			return fmt.Errorf("insert login link: %w", e)
		}
		// Токен в журнал не кладём — по журналу нельзя войти чужим
		// аккаунтом. Хватает «кто, кому, до какого времени».
		return audit.Write(ctx, tx, actorID, audit.ActionUserLoginLink,
			audit.ObjectUser, targetID.String(), map[string]any{
				"expires_at": expiresAt.UTC().Format(time.RFC3339),
			})
	})
	if txErr != nil {
		return "", time.Time{}, txErr
	}
	return rawToken, expiresAt, nil
}

// GenerateStaffLoginLink — ссылка входа для сотрудника (только админ).
func (s *Service) GenerateStaffLoginLink(ctx context.Context, targetID, actorID uuid.UUID) (InviteGenerateResult, error) {
	raw, expiresAt, err := s.repo.GenerateStaffLoginLink(ctx, targetID, actorID)
	if err != nil {
		return InviteGenerateResult{}, err
	}
	return InviteGenerateResult{
		Token:     raw,
		URL:       s.appBaseURL + "/auth/invite?token=" + raw,
		ExpiresAt: expiresAt,
	}, nil
}

// AdminStaffLoginLink godoc
// @Summary  Одноразовая ссылка для входа сотруднику (админ)
// @Description Действует 72 часа и гасит предыдущую невостребованную
// @Description ссылку этого человека. В отличие от
// @Description /admin/users/{id}/generate_invite, цель может быть
// @Description менеджером или админом — поэтому ручка смонтирована только
// @Description в админской секции и отдельно перепроверяет, что просит
// @Description действительно админ (роль могли снять после выдачи токена).
// @Description Права при входе берутся из базы в момент обмена ссылки, а
// @Description не из неё самой: ссылка не носит в себе никакой роли.
// @Tags     admin-users
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "user id сотрудника"
// @Success  200 {object} InviteGenerateResult
// @Failure  403 {object} errorResponse "forbidden_actor — роль админа снята"
// @Failure  404 {object} errorResponse "not_found"
// @Failure  409 {object} errorResponse "inactive_user — аккаунт отключён"
// @Router   /admin/users/{id}/login_link [post]
func (h *Handler) AdminStaffLoginLink(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, "id")
	if !ok {
		return
	}
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
		return
	}
	res, err := h.svc.GenerateStaffLoginLink(r.Context(), id, actor)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	// Сам токен в лог не пишем: журнал и stdout читают больше людей, чем
	// должны иметь возможность войти под сотрудником.
	auditLog(r, "user.login_link", id, "expires_at", res.ExpiresAt.Format(time.RFC3339))
	httpx.WriteJSON(w, http.StatusOK, res)
}
