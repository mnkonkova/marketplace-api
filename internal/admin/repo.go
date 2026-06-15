package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrNotFound      = errors.New("user not found")
	ErrNotManager    = errors.New("user is not a manager")
	ErrAlreadyExists = errors.New("user already exists")
	ErrInviteInvalid = errors.New("invite token invalid or expired")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// ListManagers — все пользователи с is_manager=true, отсортированные:
// сначала ожидающие аппрува, потом одобренные. Включает счётчик assigned.
func (r *Repo) ListManagers(ctx context.Context, approved *bool) ([]ManagerInfo, error) {
	q := `
SELECT u.id, COALESCE(u.email::text, ''), u.is_active, u.is_approved,
       u.email_verified_at IS NOT NULL,
       COALESCE(sp.display_name, ''),
       (SELECT COUNT(*) FROM projects p
        WHERE p.assigned_to_user_id = u.id
          AND p.status IN ('draft','active','on_hold','dispute')),
       u.created_at
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE u.is_manager = TRUE`
	args := []any{}
	if approved != nil {
		q += " AND u.is_approved = $1"
		args = append(args, *approved)
	}
	q += " ORDER BY u.is_approved ASC, u.created_at ASC"
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list managers: %w", err)
	}
	defer rows.Close()
	out := make([]ManagerInfo, 0)
	for rows.Next() {
		var m ManagerInfo
		if err := rows.Scan(&m.UserID, &m.Email, &m.IsActive, &m.IsApproved,
			&m.EmailVerified, &m.DisplayName, &m.AssignedProjects, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan manager: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchUsers — лукап для admin/manager UI: создать проект для существующего
// клиента, назначить спеца, найти юзера для promote-to-manager. Ищем по
// ILIKE '%q%' одновременно в email, phone и display_name (из обоих профильных
// таблиц через COALESCE). q короче 2 символов — пустой результат.
// Возвращает максимум 20 результатов.
//
// kind:
//   - "client" / "specialist" — узкий фильтр по users.kind.
//   - "all" / ""             — без фильтра, для admin promote-to-manager.
func (r *Repo) SearchUsers(ctx context.Context, q, kind string) ([]UserSearchResult, error) {
	q = strings.TrimSpace(q)
	if len(q) < 2 {
		return []UserSearchResult{}, nil
	}
	if kind != "" && kind != "all" && kind != "client" && kind != "specialist" {
		return nil, fmt.Errorf("invalid kind %q", kind)
	}
	// data-sec D11: escape LIKE-метасимволов в пользовательском вводе.
	// Без этого q="_" даёт match-all (любой символ в ILIKE), а q="%%%%" —
	// дорогой seq-scan. Эскейпим `\`, `%`, `_` обратным слэшем и говорим
	// PG ESCAPE '\' ниже в самом запросе.
	likeEsc := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	qEsc := likeEsc.Replace(q)
	pattern := "%" + qEsc + "%"
	prefix := qEsc + "%"

	// LEFT JOIN на оба профиля. COALESCE имени: сначала client_profiles,
	// затем specialist_profiles. У спеца профиль гарантирован, у клиента —
	// опционален. Если оба пусты — display_name=''.
	queryStr := `
SELECT u.id, COALESCE(u.email::text,''), COALESCE(u.phone,''),
       u.kind,
       COALESCE(NULLIF(cp.display_name,''), NULLIF(sp.display_name,''), '')
FROM users u
LEFT JOIN client_profiles     cp ON cp.user_id = u.id
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
WHERE u.is_active = TRUE`
	args := []any{pattern, prefix}
	pIdx := 3
	if kind != "" && kind != "all" {
		queryStr += fmt.Sprintf(" AND u.kind = $%d", pIdx)
		args = append(args, kind)
		pIdx++
	}
	// ESCAPE '\' — парный к likeEsc выше: PG будет трактовать `\%` и `\_`
	// в pattern/prefix как литералы, а не как метасимволы LIKE.
	queryStr += `
  AND (u.email::text ILIKE $1 ESCAPE '\' OR u.phone ILIKE $1 ESCAPE '\'
       OR cp.display_name ILIKE $1 ESCAPE '\' OR sp.display_name ILIKE $1 ESCAPE '\')
ORDER BY
  CASE WHEN u.email::text ILIKE $2 ESCAPE '\' OR cp.display_name ILIKE $2 ESCAPE '\' OR sp.display_name ILIKE $2 ESCAPE '\'
       THEN 0 ELSE 1 END,
  u.created_at DESC
LIMIT 20`
	rows, err := r.db.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()
	out := make([]UserSearchResult, 0, 20)
	for rows.Next() {
		var u UserSearchResult
		if err := rows.Scan(&u.UserID, &u.Email, &u.Phone, &u.Kind, &u.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListAllUsers — полный листинг с пагинацией для /admin/users.
// Возвращает (items, total). total — общее число строк под фильтрами
// (без limit/offset) для отрисовки пагинатора на фронте.
//
// Фильтры:
//   - q: ILIKE по email/phone/display_name, < 2 символов = игнор.
//   - kind: client | specialist (пусто = без фильтра).
//   - role: manager | admin | regular (regular = !is_manager AND !is_admin).
//
// Сортировка: created_at DESC (новые сверху).
func (r *Repo) ListAllUsers(ctx context.Context, p ListAllUsersParams) ([]UserListItem, int, error) {
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 20
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if p.Kind != "" && p.Kind != "client" && p.Kind != "specialist" {
		return nil, 0, fmt.Errorf("invalid kind %q", p.Kind)
	}
	if p.Role != "" && p.Role != "manager" && p.Role != "admin" && p.Role != "regular" {
		return nil, 0, fmt.Errorf("invalid role %q", p.Role)
	}

	// data-sec D11: escape LIKE-метасимволов, аналогично SearchUsers выше.
	likeEsc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	q := strings.TrimSpace(p.Q)

	// Условия и аргументы собираем динамически — limit/offset идут в конце.
	args := []any{}
	conds := []string{}

	if len(q) >= 2 {
		pattern := "%" + likeEsc.Replace(q) + "%"
		args = append(args, pattern)
		idx := len(args)
		conds = append(conds, fmt.Sprintf(`(
			u.email::text ILIKE $%d ESCAPE '\'
			OR u.phone ILIKE $%d ESCAPE '\'
			OR cp.display_name ILIKE $%d ESCAPE '\'
			OR sp.display_name ILIKE $%d ESCAPE '\'
		)`, idx, idx, idx, idx))
	}
	if p.Kind != "" {
		args = append(args, p.Kind)
		conds = append(conds, fmt.Sprintf("u.kind = $%d", len(args)))
	}
	switch p.Role {
	case "manager":
		conds = append(conds, "u.is_manager = TRUE AND u.is_admin = FALSE")
	case "admin":
		conds = append(conds, "u.is_admin = TRUE")
	case "regular":
		conds = append(conds, "u.is_manager = FALSE AND u.is_admin = FALSE")
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	base := fmt.Sprintf(`
FROM users u
LEFT JOIN client_profiles     cp ON cp.user_id = u.id
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
%s`, where)

	// total под фильтрами — один отдельный COUNT, чтобы фронт мог нарисовать
	// пагинатор. Дешёвый запрос, т.к. фильтры ILIKE используют тот же план.
	var total int
	if err := r.db.QueryRow(ctx, "SELECT COUNT(*) "+base, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	args = append(args, p.Limit, p.Offset)
	listQ := fmt.Sprintf(`
SELECT u.id, COALESCE(u.email::text,''), COALESCE(u.phone,''),
       COALESCE(NULLIF(cp.display_name,''), NULLIF(sp.display_name,''), ''),
       u.kind, u.is_admin, u.is_manager, u.is_approved, u.is_active,
       u.email_verified_at IS NOT NULL,
       u.created_at
%s
ORDER BY u.created_at DESC
LIMIT $%d OFFSET $%d`, base, len(args)-1, len(args))

	rows, err := r.db.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	items := make([]UserListItem, 0, p.Limit)
	for rows.Next() {
		var u UserListItem
		if err := rows.Scan(
			&u.UserID, &u.Email, &u.Phone, &u.DisplayName,
			&u.Kind, &u.IsAdmin, &u.IsManager, &u.IsApproved, &u.IsActive,
			&u.EmailVerified, &u.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		items = append(items, u)
	}
	return items, total, rows.Err()
}

// PromoteToManager — выставляет is_manager=TRUE и is_approved=TRUE.
// Используется в /admin/managers/promote: админ нашёл юзера по email/имени,
// делает его менеджером без отдельного approve-шага. Идемпотентно.
func (r *Repo) PromoteToManager(ctx context.Context, userID uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET is_manager = TRUE, is_approved = TRUE, updated_at = now()
		 WHERE id = $1 AND is_active = TRUE`,
		userID)
	if err != nil {
		return fmt.Errorf("promote to manager: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetApproved — менеджеру (только!) меняем is_approved. Если юзер не manager —
// ErrNotManager (защита от случайного одобрения кого попало). Идемпотентен.
func (r *Repo) SetApproved(ctx context.Context, userID uuid.UUID, approved bool) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET is_approved = $2, updated_at = now()
		 WHERE id = $1 AND is_manager = TRUE`,
		userID, approved)
	if err != nil {
		return fmt.Errorf("set approved: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// либо юзер не существует, либо не manager — различим probe-запросом.
		var exists bool
		if perr := r.db.QueryRow(ctx, `SELECT TRUE FROM users WHERE id = $1`, userID).Scan(&exists); perr != nil {
			if errors.Is(perr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("probe user: %w", perr)
		}
		return ErrNotManager
	}
	return nil
}

// SetActive — деактивировать/реактивировать юзера. is_active=false
// блокирует логин и пропускает юзера из публичной выдачи (search, feed).
// Не удаляет данные — мягкое отключение. Идемпотентно.
//
// data-sec: запрещаем деактивировать админов через этот endpoint (защита
// от случайного «выстрела в ногу»). Если очень надо — через psql.
func (r *Repo) SetActive(ctx context.Context, userID uuid.UUID, active bool) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET is_active = $2, updated_at = now()
		 WHERE id = $1 AND is_admin = FALSE`,
		userID, active)
	if err != nil {
		return fmt.Errorf("set active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// либо юзера нет, либо он админ — диагностируем через probe.
		var isAdmin bool
		if perr := r.db.QueryRow(ctx,
			`SELECT is_admin FROM users WHERE id = $1`, userID).Scan(&isAdmin); perr != nil {
			if errors.Is(perr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("probe user: %w", perr)
		}
		if isAdmin {
			return fmt.Errorf("%w: нельзя деактивировать админа через UI", ErrInvalidInputRepo)
		}
		return ErrNotFound
	}
	return nil
}

// ErrInvalidInputRepo — repo-уровневая «битый ввод» ошибка. Маппится в
// admin.ErrInvalidInput в service.go, чтобы handler отдал 400.
var ErrInvalidInputRepo = errors.New("invalid input")

// VerifyEmail — ручная пометка email подтверждённым (для админского заноса
// клиента). Если email_verified_at уже не NULL — no-op (идемпотентно).
// Возвращает ErrNotFound если юзер не существует.
func (r *Repo) VerifyEmail(ctx context.Context, userID uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()),
		                  updated_at = now()
		 WHERE id = $1`,
		userID)
	if err != nil {
		return fmt.Errorf("verify email: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DemoteFromManager — полностью снимает manager-роль: is_manager=FALSE,
// is_approved=FALSE. После этого юзер возвращается к своей базовой роли
// по kind (client → RoleClient, specialist → RoleSpecialist).
//
// Раньше «снять менеджера» делалось через SetApproved(false), которое
// убирало только is_approved. Но Role() при is_manager=TRUE возвращает
// RoleManager независимо от is_approved, и middleware режет с 403
// forbidden_unapproved — юзер ни менеджер (заблокирован), ни клиент.
// Now drop the flag вообще.
func (r *Repo) DemoteFromManager(ctx context.Context, userID uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET is_manager = FALSE, is_approved = FALSE, updated_at = now()
		 WHERE id = $1`,
		userID)
	if err != nil {
		return fmt.Errorf("demote manager: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateClient — заводит юзера (kind=client, без manager/admin-флагов;
// password = случайные 32 байта в base64; юзер всё равно зайдёт по magic-link).
// is_approved=TRUE, email_verified_at=now (раз создаёт админ, верификации не нужно).
func (r *Repo) CreateClient(ctx context.Context, in CreateClientInput) (uuid.UUID, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return uuid.Nil, errors.New("email required")
	}
	// Генерим случайный пароль (юзер не знает — заходит magic-link'ом или
	// сбрасывает). Кладём bcrypt-хеш в users.password_hash, чтобы если
	// password-flow когда-то заработает на этих юзерах, никто не залезет.
	var rnd [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return uuid.Nil, fmt.Errorf("rand: %w", err)
	}
	rawPwd := base64.StdEncoding.EncodeToString(rnd[:])
	hash, err := bcrypt.GenerateFromPassword([]byte(rawPwd), bcrypt.DefaultCost)
	if err != nil {
		return uuid.Nil, fmt.Errorf("bcrypt: %w", err)
	}
	passwordHash := string(hash)

	var userID uuid.UUID
	err = r.db.QueryRow(ctx, `
INSERT INTO users (email, password_hash, kind, is_approved, email_verified_at)
VALUES ($1, $2, 'client', TRUE, now())
RETURNING id`, email, passwordHash).Scan(&userID)
	if isUniqueViolation(err) {
		return uuid.Nil, ErrAlreadyExists
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create client: %w", err)
	}
	// display_name (если задано) положим в specialist_profiles. Это spec-таблица,
	// но в неё пишет и client — она хранит общий display_name для UI шапки.
	_ = in.DisplayName // оставляем в users.email пока — display_name в Ф6 не критично
	return userID, nil
}

// GenerateInvite — создать magic-link токен для юзера. Гасит предыдущие
// неиспользованные. Возвращает raw-токен (для отправки в письме/в ответе
// админу) и expires_at.
func (r *Repo) GenerateInvite(ctx context.Context, userID, createdBy uuid.UUID, ttl time.Duration) (rawToken string, expiresAt time.Time, err error) {
	// 32 байта → 64 hex символа. Этого с запасом для anti-bruteforce.
	var rnd [32]byte
	if _, e := rand.Read(rnd[:]); e != nil {
		return "", time.Time{}, fmt.Errorf("rand: %w", e)
	}
	rawToken = hex.EncodeToString(rnd[:])
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])
	expiresAt = time.Now().Add(ttl)

	tx, e := r.db.BeginTx(ctx, pgx.TxOptions{})
	if e != nil {
		return "", time.Time{}, fmt.Errorf("begin tx: %w", e)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// data-sec D1: инвайт можно генерить только для обычных юзеров (клиент/спец),
	// НЕ для менеджеров и админов. Раньше тут было `WHERE id = $1` без
	// фильтра роли — менеджер мог через `/manager/users/{id}/generate_invite`
	// сгенерить токен на UUID админа, потом редимить и получить JWT админа
	// (privilege escalation, см. internal/auth/handlers redeem_invite).
	var exists bool
	if e := tx.QueryRow(ctx, `
SELECT TRUE FROM users
WHERE id = $1
  AND is_admin   = FALSE
  AND is_manager = FALSE`, userID).Scan(&exists); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return "", time.Time{}, ErrNotFound
		}
		return "", time.Time{}, fmt.Errorf("check user: %w", e)
	}

	// гасим прошлые неиспользованные
	if _, e := tx.Exec(ctx,
		`UPDATE client_invites SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`,
		userID); e != nil {
		return "", time.Time{}, fmt.Errorf("invalidate prev: %w", e)
	}

	if _, e := tx.Exec(ctx,
		`INSERT INTO client_invites (user_id, token_hash, expires_at, created_by)
		 VALUES ($1, $2, $3, $4)`,
		userID, tokenHash, expiresAt, createdBy); e != nil {
		return "", time.Time{}, fmt.Errorf("insert invite: %w", e)
	}

	if e := tx.Commit(ctx); e != nil {
		return "", time.Time{}, fmt.Errorf("commit: %w", e)
	}
	return rawToken, expiresAt, nil
}

// ConsumeInvite — обмен raw-токена на user_id (для выдачи JWT). Атомарно:
// найти + пометить used + проставить email_verified. Несуществующий /
// использованный / просроченный → ErrInviteInvalid.
func (r *Repo) ConsumeInvite(ctx context.Context, rawToken string) (uuid.UUID, error) {
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID uuid.UUID
	err = tx.QueryRow(ctx, `
UPDATE client_invites
SET used_at = now()
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
RETURNING user_id`, tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInviteInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consume invite: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()),
		                  updated_at = now()
		 WHERE id = $1`, userID); err != nil {
		return uuid.Nil, fmt.Errorf("verify user: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
