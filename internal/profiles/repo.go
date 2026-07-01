package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("profile not found")
	// ErrConflict — клиент прислал PatchInput.UpdatedAt, не совпадающий
	// с текущим в БД (кто-то параллельно успел сохранить).
	ErrConflict = errors.New("profile updated_at mismatch")
	// ErrUsernameTaken — username уже занят другим спецом (unique violation
	// на specialist_profiles_username_idx).
	ErrUsernameTaken = errors.New("username already taken")
	// ErrInvalidUsername — username не прошёл CHECK constraint (формат /^[a-z0-9_-]{3,30}$/).
	// Сервис обычно валидирует ДО UPDATE через ValidateUsername, но это страховка
	// если в обход (например, тесты или прямой SQL).
	ErrInvalidUsername = errors.New("username has invalid format")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

func (r *Repo) Pool() *pgxpool.Pool { return r.db }

func (r *Repo) Get(ctx context.Context, userID uuid.UUID) (Profile, error) {
	const q = `
SELECT p.user_id, COALESCE(p.username, ''), p.display_name, p.bio,
       COALESCE(p.avatar_url, ''), COALESCE(p.city, ''),
       p.rate_min, p.rate_max, p.currency,
       p.is_published, p.rating_avg, p.reviews_count,
       COALESCE(p.contact_email, ''), COALESCE(p.contact_phone, ''),
       p.updated_at,
       p.production_id, p.is_freelance,
       p.moderation_status, COALESCE(p.moderation_reason, ''), p.moderation_reviewed_at,
       p.social_links
FROM specialist_profiles p
WHERE p.user_id = $1`
	var p Profile
	var socialJSON []byte
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&p.UserID, &p.Username, &p.DisplayName, &p.Bio,
		&p.AvatarURL, &p.City,
		&p.RateMin, &p.RateMax, &p.Currency,
		&p.IsPublished, &p.RatingAvg, &p.ReviewsCount,
		&p.ContactEmail, &p.ContactPhone,
		&p.UpdatedAt,
		&p.ProductionID, &p.IsFreelance,
		&p.ModerationStatus, &p.ModerationReason, &p.ModerationReviewedAt,
		&socialJSON,
	)
	if err == nil && len(socialJSON) > 0 {
		// JSON Unmarshal безопасен — записываем туда только из самого нашего
		// PatchInTx, формат под контролем. Если приходит мусор — берём пустую
		// карту (не валим всю выборку профиля).
		_ = json.Unmarshal(socialJSON, &p.SocialLinks)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("query profile: %w", err)
	}

	cats, primary, err := r.listCategories(ctx, userID)
	if err != nil {
		return Profile{}, err
	}
	p.Categories = cats
	p.PrimaryCategory = primary

	skills, err := r.listSkillIDs(ctx, userID)
	if err != nil {
		return Profile{}, err
	}
	p.SkillIDs = skills

	return p, nil
}

// ResolveUserIDByUsername — поиск user_id по username для handle-роутинга
// /specialists/{handle}. Возвращает ErrNotFound если username не существует.
// username сравнивается case-insensitive (хранится в lowercase).
func (r *Repo) ResolveUserIDByUsername(ctx context.Context, username string) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT user_id FROM specialist_profiles WHERE username = lower($1)`,
		username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve username: %w", err)
	}
	return id, nil
}

func (r *Repo) listCategories(ctx context.Context, userID uuid.UUID) ([]string, string, error) {
	rows, err := r.db.Query(ctx,
		`SELECT category_code, is_primary FROM specialist_categories WHERE user_id = $1`,
		userID)
	if err != nil {
		return nil, "", fmt.Errorf("query categories: %w", err)
	}
	defer rows.Close()
	codes := make([]string, 0, 4)
	primary := ""
	for rows.Next() {
		var code string
		var isPrimary bool
		if err := rows.Scan(&code, &isPrimary); err != nil {
			return nil, "", err
		}
		codes = append(codes, code)
		if isPrimary {
			primary = code
		}
	}
	return codes, primary, rows.Err()
}

func (r *Repo) listSkillIDs(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.db.Query(ctx,
		`SELECT skill_id FROM specialist_skills WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("query skills: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id.String())
	}
	return out, rows.Err()
}

func (r *Repo) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repo) PatchInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, in PatchInput) error {
	// username: nil = не трогать, "" = NULL (сброс), любое другое = поставить.
	// Используем тройку (setUsername, usernameVal, usernameIsEmpty) для UPDATE
	// CASE — иначе COALESCE не отличит "не трогать" от "сбросить".
	var setUsername bool
	var usernameVal *string
	if in.Username != nil {
		setUsername = true
		v := strings.TrimSpace(strings.ToLower(*in.Username))
		if v != "" {
			usernameVal = &v
		}
	}
	const q = `
UPDATE specialist_profiles SET
  display_name  = COALESCE($2, display_name),
  bio           = COALESCE($3, bio),
  avatar_url    = COALESCE($4, avatar_url),
  city          = COALESCE($5, city),
  rate_min      = CASE WHEN $6::boolean THEN $7 ELSE rate_min END,
  rate_max      = CASE WHEN $8::boolean THEN $9 ELSE rate_max END,
  currency      = COALESCE($10, currency),
  contact_email = COALESCE($11, contact_email),
  contact_phone = COALESCE($12, contact_phone),
  production_id = CASE WHEN $14::boolean THEN $15 ELSE production_id END,
  is_freelance  = CASE WHEN $16::boolean THEN $17 ELSE is_freelance END,
  username      = CASE WHEN $18::boolean THEN $19 ELSE username END,
  social_links  = CASE WHEN $20::boolean THEN $21::jsonb ELSE social_links END,
  updated_at    = now()
WHERE user_id = $1
  AND ($13::timestamptz IS NULL OR updated_at = $13)`
	// social_links: nil = не трогать; не-nil = заменить полностью.
	var setSocial bool
	var socialJSON []byte
	if in.SocialLinks != nil {
		setSocial = true
		var mErr error
		socialJSON, mErr = json.Marshal(in.SocialLinks)
		if mErr != nil {
			return fmt.Errorf("marshal social_links: %w", mErr)
		}
	}
	tag, err := tx.Exec(ctx, q,
		userID,
		in.DisplayName,
		in.Bio,
		in.AvatarURL,
		in.City,
		in.RateMin != nil, in.RateMin,
		in.RateMax != nil, in.RateMax,
		in.Currency,
		in.ContactEmail,
		in.ContactPhone,
		in.UpdatedAt,
		in.SetProduction, in.ProductionID,
		in.SetIsFreelance, in.IsFreelance,
		setUsername, usernameVal,
		setSocial, socialJSON,
	)
	if err != nil {
		// 23505 unique_violation на username — отдаём как ErrConflict,
		// сервис мапит в 409 conflict с понятным сообщением.
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "specialist_profiles_username_idx" {
			return ErrUsernameTaken
		}
		// 23514 check_violation на username CHECK constraint — невалидный формат.
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return ErrInvalidUsername
		}
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 0 строк может быть по двум причинам: нет такого профиля или updated_at не совпал.
		// Если клиент прислал UpdatedAt, считаем что профиль существует (для
		// авторизованного юзера так и есть) — это конфликт.
		if in.UpdatedAt != nil {
			return ErrConflict
		}
		return ErrNotFound
	}
	return nil
}

// LockProfileForUpdateInTx бампит updated_at родительского профиля и берёт
// row-level лок до конца транзакции. Если expected != nil — проверяет,
// что прежнее updated_at совпадает (optimistic concurrency check для
// PUT /me/profile/{categories,skills} и т.п., где DELETE+INSERT не
// защищён от lost-update).
func (r *Repo) LockProfileForUpdateInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, expected *time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE specialist_profiles SET updated_at = now()
		 WHERE user_id = $1 AND ($2::timestamptz IS NULL OR updated_at = $2)`,
		userID, expected)
	if err != nil {
		return fmt.Errorf("lock profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if expected != nil {
			return ErrConflict
		}
		return ErrNotFound
	}
	return nil
}

func (r *Repo) ReplaceCategoriesInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, codes []string, primary string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM specialist_categories WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete categories: %w", err)
	}
	// Один INSERT через unnest вместо N round-trip'ов. На 4-8 категориях
	// разница невелика, но row-lock и WAL-запись делаются один раз.
	if len(codes) > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO specialist_categories (user_id, category_code, is_primary)
SELECT $1, c, c = $3 FROM unnest($2::text[]) AS c`,
			userID, codes, primary); err != nil {
			return fmt.Errorf("insert categories: %w", err)
		}
	}
	// Каскад: если из профиля убрали категорию X, у его видео в
	// portfolio_items.category_codes её тоже не должно остаться — иначе
	// /feed?category=X покажет ролик «editor» от спеца, который editor'ом
	// уже не является. INTERSECT по unnest сохраняет порядок и убирает
	// «осиротевшие» коды.
	if _, err := tx.Exec(ctx, `
UPDATE portfolio_items pi
SET category_codes = COALESCE(ARRAY(
    SELECT c FROM unnest(pi.category_codes) AS c
    WHERE c IN (SELECT category_code FROM specialist_categories WHERE user_id = $1)
), ARRAY[]::TEXT[])
WHERE pi.user_id = $1`, userID); err != nil {
		return fmt.Errorf("cascade portfolio category_codes: %w", err)
	}
	return nil
}

func (r *Repo) ReplaceSkillsInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, skillIDs []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM specialist_skills WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete skills: %w", err)
	}
	if len(skillIDs) > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO specialist_skills (user_id, skill_id)
SELECT $1, s FROM unnest($2::uuid[]) AS s`,
			userID, skillIDs); err != nil {
			return fmt.Errorf("insert skills: %w", err)
		}
	}
	return nil
}

// RevalidatePublishInTx — повторяет hard-проверки SetPublished внутри tx
// под FOR UPDATE на specialist_profiles, чтобы между долгим LLM-checker'ом
// и финальным UPDATE юзер не смог почистить bio/display_name/production
// (R10). Если условия нарушены — ErrPublishIncomplete.
func (r *Repo) RevalidatePublishInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var (
		productionID *uuid.UUID
		isFreelance  bool
		bio          string
		displayName  string
	)
	err := tx.QueryRow(ctx, `
SELECT production_id, is_freelance,
       COALESCE(TRIM(bio), ''),
       COALESCE(TRIM(display_name), '')
FROM specialist_profiles
WHERE user_id = $1
FOR UPDATE`, userID).Scan(&productionID, &isFreelance, &bio, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revalidate publish: %w", err)
	}
	if productionID == nil && !isFreelance {
		return fmt.Errorf("%w: выберите студию или отметьте «фрилансер»", ErrPublishIncomplete)
	}
	if bio == "" {
		return fmt.Errorf("%w: заполните «О себе» в профиле", ErrPublishIncomplete)
	}
	if displayName == "" {
		return fmt.Errorf("%w: заполните имя в профиле", ErrPublishIncomplete)
	}
	return nil
}

// SetPublishedInTx — переключает is_published.
//
// При published=true модерация устроена так:
//   - был rejected      → переводим в pending_review (новая попытка пройти модерацию)
//   - был approved      → оставляем approved (это «включить обратно видимость» без правок профиля)
//   - был pending_review → остаётся pending_review
//
// При published=false — moderation_status не трогается. Юзер просто выводит
// себя из каталога; одобрение «висит» до следующей публикации.
//
// Возвращает needsAdminNotify=true если результат UPDATE'a — спец встал
// в очередь модерации и админу нужно прислать уведомление. Условие:
// published=true И итоговый status='pending_review' И (до этого
// is_published было false ИЛИ status был не pending_review). Это покрывает
// первую публикацию (default-pending) и retry после rejection. Уже-pending
// спец, который ещё раз дёрнул publish, не флапает уведомлениями.
func (r *Repo) SetPublishedInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, published bool) (needsAdminNotify bool, err error) {
	const q = `
WITH before AS (
  SELECT is_published AS prev_pub, moderation_status AS prev_status
  FROM specialist_profiles WHERE user_id = $1 FOR UPDATE
), upd AS (
  UPDATE specialist_profiles SET
    is_published      = $2,
    moderation_status = CASE
      WHEN $2 AND moderation_status = 'rejected' THEN 'pending_review'
      ELSE moderation_status
    END,
    updated_at = now()
  WHERE user_id = $1
  RETURNING is_published AS now_pub, moderation_status AS now_status
)
SELECT
  (SELECT prev_pub    FROM before),
  (SELECT prev_status FROM before),
  (SELECT now_pub     FROM upd),
  (SELECT now_status  FROM upd)`
	var prevPub *bool
	var prevStatus *string
	var nowPub *bool
	var nowStatus *string
	if err := tx.QueryRow(ctx, q, userID, published).Scan(&prevPub, &prevStatus, &nowPub, &nowStatus); err != nil {
		return false, fmt.Errorf("update published: %w", err)
	}
	if nowStatus == nil || nowPub == nil {
		return false, ErrNotFound
	}
	if !*nowPub || *nowStatus != "pending_review" {
		return false, nil
	}
	prevPubVal := false
	if prevPub != nil {
		prevPubVal = *prevPub
	}
	prevStatusVal := ""
	if prevStatus != nil {
		prevStatusVal = *prevStatus
	}
	return !prevPubVal || prevStatusVal != "pending_review", nil
}

// BumpModerationToPendingIfApprovedInTx — если профиль approved, переводит
// его в pending_review. Используется во всех ручках, которые меняют контент,
// видимый в каталоге (профиль, категории, навыки, портфолио). После такой
// мутации спец временно исчезает из поиска до повторного одобрения админом.
// Идемпотентно; если status != approved — никаких действий.
//
// Возвращает changed=true если переход реально произошёл (был approved).
// Service использует это, чтобы эмитить уведомление админу.
func (r *Repo) BumpModerationToPendingIfApprovedInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (changed bool, err error) {
	tag, err := tx.Exec(ctx, `
UPDATE specialist_profiles
SET moderation_status = 'pending_review',
    moderation_reason = NULL,
    moderation_reviewed_at = NULL,
    moderation_reviewed_by = NULL,
    updated_at = now()
WHERE user_id = $1 AND moderation_status = 'approved'`,
		userID)
	if err != nil {
		return false, fmt.Errorf("bump moderation: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetModerationDecisionInTx — verdict админа: approved / rejected.
// reason обязателен для rejected (валидация на стороне сервиса).
// reviewedBy — id админа, чтобы был аудит-трейл.
//
// expectedUpdatedAt — optimistic-lock: версия профиля, которую видел админ
// в момент открытия карточки. Если спец параллельно успел отредактировать
// (updated_at сменился), возвращается ErrConflict — фронт должен перезагрузить
// карточку и попросить админа пересмотреть. Без этого admin одобряет V1, а
// в каталог попадает V2 (race из docs/SPECIALIST_MODERATION.md §5.C).
func (r *Repo) SetModerationDecisionInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, status string, reason string, reviewedBy uuid.UUID, expectedUpdatedAt *time.Time) error {
	var reasonArg *string
	if reason != "" {
		reasonArg = &reason
	}
	tag, err := tx.Exec(ctx, `
UPDATE specialist_profiles
SET moderation_status      = $2,
    moderation_reason      = $3,
    moderation_reviewed_at = now(),
    moderation_reviewed_by = $4,
    updated_at             = now()
WHERE user_id = $1
  AND ($5::timestamptz IS NULL OR updated_at = $5)`,
		userID, status, reasonArg, reviewedBy, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("set moderation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if expectedUpdatedAt != nil {
			return ErrConflict
		}
		return ErrNotFound
	}
	return nil
}

func (r *Repo) CategoryTitle(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", nil
	}
	var title string
	err := r.db.QueryRow(ctx, `SELECT title FROM specialty_categories WHERE code = $1`, code).Scan(&title)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query category title: %w", err)
	}
	return title, nil
}

func (r *Repo) ValidCategoryCodes(ctx context.Context, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT code FROM specialty_categories WHERE code = ANY($1)`, codes)
	if err != nil {
		return nil, fmt.Errorf("validate codes: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, len(codes))
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repo) GetPublic(ctx context.Context, userID uuid.UUID) (PublicProfile, error) {
	return r.getPublic(ctx, userID, true)
}

// GetPublicForOwner — preview для самого спеца (owner-mode): ВКЛЮЧАЕТ
// неопубликованные и не-approved профили, чтобы юзер мог увидеть как его
// видят клиенты до публикации/модерации. Handler выставляет IsPreview=true.
func (r *Repo) GetPublicForOwner(ctx context.Context, userID uuid.UUID) (PublicProfile, error) {
	return r.getPublic(ctx, userID, false)
}

func (r *Repo) getPublic(ctx context.Context, userID uuid.UUID, strict bool) (PublicProfile, error) {
	var publishCond string
	if strict {
		publishCond = "AND p.is_published = TRUE AND p.moderation_status = 'approved'"
	}
	// Один SQL вместо 5 последовательных round-trip'ов (было ~500 мс TTFB,
	// стало ~150 мс). Категории/скилы/портфолио/отзывы собираются через
	// json_agg — Go парсит из JSON и заполняет соответствующие поля.
	// Images для kind='image' подтягиваются вложенным json_agg (photo-sets).
	// Reviews лимитятся 20 в подзапросе (порядок как в listReviews).
	q := `
SELECT
  p.user_id,
  COALESCE(p.username, ''),
  p.display_name,
  p.bio,
  COALESCE(p.avatar_url, ''),
  COALESCE(p.city, ''),
  p.rate_min, p.rate_max, p.currency,
  p.rating_avg, p.reviews_count,
  COALESCE(pr.name, ''),
  p.is_freelance,
  p.is_published,
  p.moderation_status,
  p.social_links,
  COALESCE((
    SELECT json_agg(json_build_object(
      'code', sc.category_code,
      'title', c.title,
      'is_primary', sc.is_primary
    ) ORDER BY sc.is_primary DESC, c.sort_order, c.title)
    FROM specialist_categories sc
    JOIN specialty_categories c ON c.code = sc.category_code
    WHERE sc.user_id = p.user_id
  ), '[]'::json) AS categories_json,
  COALESCE((
    SELECT json_agg(json_build_object(
      'id', s.id,
      'slug', s.slug,
      'title', s.title,
      'kind', s.kind
    ) ORDER BY s.kind, s.title)
    FROM specialist_skills ss
    JOIN skills s ON s.id = ss.skill_id
    WHERE ss.user_id = p.user_id
  ), '[]'::json) AS skills_json,
  COALESCE((
    SELECT json_agg(json_build_object(
      'id', pi.id,
      'kind', pi.kind,
      'title', pi.title,
      'description', pi.description,
      'video_url', COALESCE(pi.video_url, ''),
      'thumbnail_url', COALESCE(pi.thumbnail_url, ''),
      'external_url', COALESCE(pi.external_url, ''),
      'category_codes', pi.category_codes,
      'sort_order', pi.sort_order,
      'created_at', pi.created_at,
      'updated_at', pi.updated_at,
      'preview_url', COALESCE(pi.preview_url, ''),
      'animated_thumb_url', COALESCE(pi.animated_thumb_url, ''),
      'preview_status', pi.preview_status,
      'images', COALESCE((
        SELECT json_agg(json_build_object(
          'id', img.id,
          'image_url', img.image_url,
          'sort_order', img.sort_order,
          'width', img.width,
          'height', img.height,
          'created_at', img.created_at
        ) ORDER BY img.sort_order, img.created_at)
        FROM portfolio_images img
        WHERE img.portfolio_item_id = pi.id AND pi.kind = 'image'
      ), '[]'::json)
    ) ORDER BY pi.sort_order, pi.created_at DESC)
    FROM portfolio_items pi
    WHERE pi.user_id = p.user_id
  ), '[]'::json) AS portfolio_json,
  COALESCE((
    SELECT json_agg(json_build_object(
      'id', r.id,
      'author_user_id', r.author_user_id,
      'author_name', COALESCE(NULLIF(r.author_name, ''), 'Клиент'),
      'rating', r.rating,
      'text', r.text,
      'created_at', r.created_at
    ) ORDER BY r.created_at DESC)
    FROM (
      SELECT id, author_user_id, author_name, rating, text, created_at
      FROM reviews
      WHERE target_user_id = p.user_id
      ORDER BY created_at DESC
      LIMIT 20
    ) r
  ), '[]'::json) AS reviews_json
FROM specialist_profiles p
LEFT JOIN productions pr ON pr.id = p.production_id AND pr.is_active = TRUE
WHERE p.user_id = $1
` + publishCond

	var p PublicProfile
	var socialJSON, catsJSON, skillsJSON, portfolioJSON, reviewsJSON []byte
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&p.UserID, &p.Username, &p.DisplayName, &p.Bio, &p.AvatarURL, &p.City,
		&p.RateMin, &p.RateMax, &p.Currency, &p.RatingAvg, &p.ReviewsCount,
		&p.ProductionName, &p.IsFreelance,
		&p.IsPublished, &p.ModerationStatus,
		&socialJSON,
		&catsJSON, &skillsJSON, &portfolioJSON, &reviewsJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicProfile{}, ErrNotFound
	}
	if err != nil {
		return PublicProfile{}, fmt.Errorf("query public profile: %w", err)
	}
	if len(socialJSON) > 0 {
		_ = json.Unmarshal(socialJSON, &p.SocialLinks)
	}
	// Инициализируем как пустые слайсы (json_agg с COALESCE даёт '[]',
	// unmarshal вернёт []T{}, но на всякий случай гарантируем не-nil).
	p.Categories = []CategoryRef{}
	p.Skills = []SkillRef{}
	p.Portfolio = []PortfolioItem{}
	p.Reviews = []Review{}
	if err := json.Unmarshal(catsJSON, &p.Categories); err != nil {
		return PublicProfile{}, fmt.Errorf("unmarshal categories: %w", err)
	}
	if err := json.Unmarshal(skillsJSON, &p.Skills); err != nil {
		return PublicProfile{}, fmt.Errorf("unmarshal skills: %w", err)
	}
	if err := json.Unmarshal(portfolioJSON, &p.Portfolio); err != nil {
		return PublicProfile{}, fmt.Errorf("unmarshal portfolio: %w", err)
	}
	if err := json.Unmarshal(reviewsJSON, &p.Reviews); err != nil {
		return PublicProfile{}, fmt.Errorf("unmarshal reviews: %w", err)
	}

	return p, nil
}

func (r *Repo) listCategoriesWithTitles(ctx context.Context, userID uuid.UUID) ([]CategoryRef, error) {
	rows, err := r.db.Query(ctx, `
SELECT sc.category_code, c.title, sc.is_primary
FROM specialist_categories sc
JOIN specialty_categories c ON c.code = sc.category_code
WHERE sc.user_id = $1
ORDER BY sc.is_primary DESC, c.sort_order, c.title`, userID)
	if err != nil {
		return nil, fmt.Errorf("query categories: %w", err)
	}
	defer rows.Close()
	out := make([]CategoryRef, 0, 4)
	for rows.Next() {
		var c CategoryRef
		if err := rows.Scan(&c.Code, &c.Title, &c.IsPrimary); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repo) listSkillsWithTitles(ctx context.Context, userID uuid.UUID) ([]SkillRef, error) {
	rows, err := r.db.Query(ctx, `
SELECT s.id, s.slug, s.title, s.kind
FROM specialist_skills ss
JOIN skills s ON s.id = ss.skill_id
WHERE ss.user_id = $1
ORDER BY s.kind, s.title`, userID)
	if err != nil {
		return nil, fmt.Errorf("query skills: %w", err)
	}
	defer rows.Close()
	out := make([]SkillRef, 0, 8)
	for rows.Next() {
		var s SkillRef
		if err := rows.Scan(&s.ID, &s.Slug, &s.Title, &s.Kind); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repo) listPortfolio(ctx context.Context, userID uuid.UUID) ([]PortfolioItem, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, kind, title, description,
       COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
       category_codes, sort_order, created_at, updated_at,
       COALESCE(preview_url, ''), COALESCE(animated_thumb_url, ''), preview_status
FROM portfolio_items
WHERE user_id = $1
ORDER BY sort_order, created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("query portfolio: %w", err)
	}
	defer rows.Close()
	out := make([]PortfolioItem, 0, 8)
	var imageItemIDs []uuid.UUID
	for rows.Next() {
		var p PortfolioItem
		if err := rows.Scan(
			&p.ID, &p.Kind, &p.Title, &p.Description,
			&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
			&p.CategoryCodes, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt,
			&p.PreviewURL, &p.AnimatedThumbURL, &p.PreviewStatus,
		); err != nil {
			return nil, err
		}
		if p.Kind == "image" {
			imageItemIDs = append(imageItemIDs, p.ID)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(imageItemIDs) > 0 {
		byItem, err := r.loadImagesByItems(ctx, imageItemIDs)
		if err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].Kind == "image" {
				out[i].Images = byItem[out[i].ID]
			}
		}
	}
	return out, nil
}

// loadImagesByItems — батч-загрузка кадров для нескольких photo-set'ов одним
// запросом (избегает N+1 при ListPortfolio). Возвращает map item_id → []image.
func (r *Repo) loadImagesByItems(ctx context.Context, itemIDs []uuid.UUID) (map[uuid.UUID][]PortfolioImage, error) {
	return r.loadImagesByItemsQuerier(ctx, r.db, itemIDs)
}

// loadImagesByItemsInTx — та же выборка, но использует переданный tx. Нужен
// в Append/Reorder, чтобы видеть свои же INSERT'ы/UPDATE'ы до коммита —
// иначе caller получит устаревший список. (READ COMMITTED snapshot pool'a
// не видит uncommitted writes в чужом connection'е.)
func (r *Repo) loadImagesByItemsInTx(ctx context.Context, tx pgx.Tx, itemIDs []uuid.UUID) (map[uuid.UUID][]PortfolioImage, error) {
	return r.loadImagesByItemsQuerier(ctx, tx, itemIDs)
}

// pgxQuerier — минимальный интерфейс, общий для *pgxpool.Pool и pgx.Tx.
// Позволяет переиспользовать один SELECT с разных источников соединений.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (r *Repo) loadImagesByItemsQuerier(ctx context.Context, q pgxQuerier, itemIDs []uuid.UUID) (map[uuid.UUID][]PortfolioImage, error) {
	rows, err := q.Query(ctx, `
SELECT id, portfolio_item_id, image_url, sort_order, width, height, created_at
FROM portfolio_images
WHERE portfolio_item_id = ANY($1)
ORDER BY portfolio_item_id, sort_order, created_at`, itemIDs)
	if err != nil {
		return nil, fmt.Errorf("query portfolio images: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID][]PortfolioImage, len(itemIDs))
	for rows.Next() {
		var img PortfolioImage
		var parent uuid.UUID
		if err := rows.Scan(&img.ID, &parent, &img.ImageURL, &img.SortOrder, &img.Width, &img.Height, &img.CreatedAt); err != nil {
			return nil, err
		}
		out[parent] = append(out[parent], img)
	}
	return out, rows.Err()
}

// ListPortfolio — экспортируемая обёртка для /me/portfolio (handler).
// listPortfolio (lower-case) дёргается из GetPublic, держим обе.
func (r *Repo) ListPortfolio(ctx context.Context, userID uuid.UUID) ([]PortfolioItem, error) {
	return r.listPortfolio(ctx, userID)
}

// CountVideosInTx — число видео-айтемов спеца (kind='video' и непустой
// video_url) внутри переданной транзакции. Используется в проверке
// hard-лимита: считаем после row-lock'a на specialist_profiles, чтобы
// двое параллельных INSERT'ов не обошли проверку («19+19 → 21»).
func (r *Repo) CountVideosInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM portfolio_items
		 WHERE user_id = $1 AND kind = 'video'
		   AND video_url IS NOT NULL AND video_url <> ''`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count videos: %w", err)
	}
	return n, nil
}

// CountPhotoSetsInTx — число photo-set'ов спеца (kind='image') внутри tx.
// Параллельно с CountVideosInTx: лимиты независимы, спец может иметь
// 20 видео + 20 photo-set'ов.
func (r *Repo) CountPhotoSetsInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM portfolio_items
		 WHERE user_id = $1 AND kind = 'image'`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count photo-sets: %w", err)
	}
	return n, nil
}

// CreatePortfolioPhotoSetInTx — создаёт photo-set: один portfolio_item
// kind='image' + N кадров в portfolio_images. Возвращает item с заполненным
// Images. Вызывающий код в той же tx эмитит outbox-событие.
func (r *Repo) CreatePortfolioPhotoSetInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, in PortfolioPhotoSetCreateInput) (PortfolioItem, error) {
	const qItem = `
INSERT INTO portfolio_items (
    user_id, title, description,
    category_codes,
    kind,
    sort_order
)
VALUES (
    $1, $2, $3,
    $4,
    'image',
    COALESCE((SELECT MAX(sort_order)+1 FROM portfolio_items WHERE user_id = $1), 0)
)
RETURNING id, kind, title, description,
          COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
          category_codes, sort_order, created_at, updated_at,
          COALESCE(preview_url, ''), COALESCE(animated_thumb_url, ''), preview_status`
	var p PortfolioItem
	cats := in.CategoryCodes
	if cats == nil {
		cats = []string{}
	}
	err := tx.QueryRow(ctx, qItem, userID, in.Title, in.Description, cats).Scan(
		&p.ID, &p.Kind, &p.Title, &p.Description,
		&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
		&p.CategoryCodes, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt,
		&p.PreviewURL, &p.AnimatedThumbURL, &p.PreviewStatus,
	)
	if err != nil {
		return PortfolioItem{}, fmt.Errorf("insert photo-set item: %w", err)
	}
	const qImage = `
INSERT INTO portfolio_images (portfolio_item_id, image_url, sort_order, width, height)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, image_url, sort_order, width, height, created_at`
	images := make([]PortfolioImage, 0, len(in.Images))
	for i, img := range in.Images {
		var stored PortfolioImage
		if err := tx.QueryRow(ctx, qImage, p.ID, img.ImageURL, i, img.Width, img.Height).Scan(
			&stored.ID, &stored.ImageURL, &stored.SortOrder, &stored.Width, &stored.Height, &stored.CreatedAt,
		); err != nil {
			return PortfolioItem{}, fmt.Errorf("insert photo-image: %w", err)
		}
		// Денормализуем превью на родителя: первое фото = thumbnail_url.
		// Удобно для cabinet-плиток и для feed-индексера (можно использовать
		// как thumb если нужен fast hint без загрузки массива).
		if i == 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE portfolio_items SET thumbnail_url = $2 WHERE id = $1`,
				p.ID, img.ImageURL); err != nil {
				return PortfolioItem{}, fmt.Errorf("set thumbnail: %w", err)
			}
			p.ThumbnailURL = img.ImageURL
		}
		images = append(images, stored)
	}
	p.Images = images
	return p, nil
}

// CreatePortfolioVideoInTx — добавляет видео-айтем внутри переданной
// транзакции. Вызывающий код в одной tx эмитит outbox-событие, чтобы
// ES-индекс спеца (last_video_at) обновился атомарно с записью в PG.
func (r *Repo) CreatePortfolioVideoInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, in PortfolioCreateInput) (PortfolioItem, error) {
	const q = `
INSERT INTO portfolio_items (
    user_id, title, description,
    video_url, thumbnail_url,
    category_codes,
    kind, duration_sec, aspect,
    sort_order
)
VALUES (
    $1, $2, $3,
    $4, NULLIF($5, ''),
    $6,
    'video', $7, $8,
    COALESCE((SELECT MAX(sort_order)+1 FROM portfolio_items WHERE user_id = $1), 0)
)
RETURNING id, kind, title, description,
          COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
          category_codes, sort_order, created_at, updated_at,
          COALESCE(preview_url, ''), COALESCE(animated_thumb_url, ''), preview_status`
	var p PortfolioItem
	cats := in.CategoryCodes
	if cats == nil {
		cats = []string{}
	}
	var dur *int
	if in.DurationSec > 0 {
		d := in.DurationSec
		dur = &d
	}
	aspect := in.Aspect
	if aspect == "" {
		aspect = "9:16"
	}
	err := tx.QueryRow(ctx, q,
		userID, in.Title, in.Description,
		in.VideoURL, in.ThumbnailURL,
		cats,
		dur, aspect,
	).Scan(
		&p.ID, &p.Kind, &p.Title, &p.Description,
		&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
		&p.CategoryCodes, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt,
		&p.PreviewURL, &p.AnimatedThumbURL, &p.PreviewStatus,
	)
	if err != nil {
		return PortfolioItem{}, fmt.Errorf("insert portfolio: %w", err)
	}
	return p, nil
}

// UpdatePortfolioCategoriesInTx — переписывает category_codes у видео-айтема
// внутри переданной транзакции. См. CreatePortfolioVideoInTx про outbox.
// Если expectedUpdatedAt != nil, добавляется optimistic-lock проверка
// updated_at = expected. ErrConflict если версия устарела, ErrNotFound
// если записи нет или она чужая.
func (r *Repo) UpdatePortfolioCategoriesInTx(ctx context.Context, tx pgx.Tx, userID, itemID uuid.UUID, codes []string, expectedUpdatedAt *time.Time) (PortfolioItem, error) {
	if codes == nil {
		codes = []string{}
	}
	const q = `
UPDATE portfolio_items
   SET category_codes = $3,
       updated_at     = now()
 WHERE id = $1 AND user_id = $2
   AND ($4::timestamptz IS NULL OR updated_at = $4)
RETURNING id, kind, title, description,
          COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
          category_codes, sort_order, created_at, updated_at`
	var p PortfolioItem
	err := tx.QueryRow(ctx, q, itemID, userID, codes, expectedUpdatedAt).Scan(
		&p.ID, &p.Kind, &p.Title, &p.Description,
		&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
		&p.CategoryCodes, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// 0 строк: либо записи нет/чужая, либо updated_at не совпал.
		// Если клиент прислал ожидание — считаем что это conflict (наиболее
		// частый кейс при гонке); фронт всё равно перезагрузит и увидит, что
		// записи нет, если она и правда удалена.
		if expectedUpdatedAt != nil {
			return PortfolioItem{}, ErrConflict
		}
		return PortfolioItem{}, ErrNotFound
	}
	if err != nil {
		return PortfolioItem{}, fmt.Errorf("update portfolio categories: %w", err)
	}
	return p, nil
}

// LoadItemUpdatedAtInTx — читает свежий updated_at parent'а в той же tx.
// Нужен чтобы Append/Reorder/Delete-image могли вернуть фронту обновлённый
// item.updated_at — без этого фронтовый snapshot станет stale и следующий
// PATCH /me/portfolio/{id} (edit meta) получит 409.
func (r *Repo) LoadItemUpdatedAtInTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID) (time.Time, error) {
	var t time.Time
	err := tx.QueryRow(ctx,
		`SELECT updated_at FROM portfolio_items WHERE id = $1`, itemID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load item updated_at: %w", err)
	}
	return t, nil
}

// AppendPortfolioImagesInTx — добавляет N фото в конец photo-set'а.
// Гарантирует ownership через JOIN с user_id. Возвращает обновлённый список
// всех кадров (для отрисовки cover/counter на фронте сразу).
// Если parent kind != 'image' или не принадлежит юзеру — ErrNotFound.
// Если len(after) > maxPerSet — ErrLimitExceeded.
var ErrLimitExceeded = errors.New("photo-set limit exceeded")

func (r *Repo) AppendPortfolioImagesInTx(ctx context.Context, tx pgx.Tx, userID, itemID uuid.UUID, urls []PortfolioPhotoRef, maxPerSet int) ([]PortfolioImage, error) {
	// 1. lock parent + проверка ownership/kind
	var ownerID uuid.UUID
	var kind string
	if err := tx.QueryRow(ctx,
		`SELECT user_id, kind FROM portfolio_items WHERE id = $1 FOR UPDATE`, itemID,
	).Scan(&ownerID, &kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lock parent: %w", err)
	}
	if ownerID != userID || kind != "image" {
		return nil, ErrNotFound
	}
	// 2. текущий count и max sort_order
	var current int
	var maxSort *int
	if err := tx.QueryRow(ctx,
		`SELECT count(*), MAX(sort_order) FROM portfolio_images WHERE portfolio_item_id = $1`,
		itemID,
	).Scan(&current, &maxSort); err != nil {
		return nil, fmt.Errorf("count images: %w", err)
	}
	if current+len(urls) > maxPerSet {
		return nil, ErrLimitExceeded
	}
	startOrder := 0
	if maxSort != nil {
		startOrder = *maxSort + 1
	}
	// 3. INSERT каждого нового
	const qInsert = `
INSERT INTO portfolio_images (portfolio_item_id, image_url, sort_order, width, height)
VALUES ($1, $2, $3, $4, $5)`
	for i, img := range urls {
		if _, err := tx.Exec(ctx, qInsert, itemID, img.ImageURL, startOrder+i, img.Width, img.Height); err != nil {
			return nil, fmt.Errorf("insert image: %w", err)
		}
	}
	// 4. бампим parent.updated_at — фронт держит optimistic-lock версию на
	//    portfolio_items.updated_at, после append она должна измениться,
	//    иначе следующий PATCH meta получит 409.
	if _, err := tx.Exec(ctx,
		`UPDATE portfolio_items SET updated_at = now() WHERE id = $1`, itemID); err != nil {
		return nil, fmt.Errorf("bump parent updated_at: %w", err)
	}
	// 5. перечитать весь набор для возврата фронту — через tx, иначе INSERT'ы
	//    из этой же tx не видны (отдельный pool-connection идёт под своим snapshot).
	byItem, err := r.loadImagesByItemsInTx(ctx, tx, []uuid.UUID{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// ReorderPortfolioImagesInTx — переписывает sort_order у всех кадров photo-set'а
// по списку imageIDs. Список ДОЛЖЕН содержать все текущие image_id'шки сета;
// если не совпадает — ErrInvalidInput (страховка от потери кадра).
// Также обновляет cover (thumbnail_url у parent'а) на новое первое фото.
func (r *Repo) ReorderPortfolioImagesInTx(ctx context.Context, tx pgx.Tx, userID, itemID uuid.UUID, imageIDs []uuid.UUID) ([]PortfolioImage, error) {
	// 1. lock parent + ownership/kind check
	var ownerID uuid.UUID
	var kind string
	if err := tx.QueryRow(ctx,
		`SELECT user_id, kind FROM portfolio_items WHERE id = $1 FOR UPDATE`, itemID,
	).Scan(&ownerID, &kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lock parent: %w", err)
	}
	if ownerID != userID || kind != "image" {
		return nil, ErrNotFound
	}
	// 2. собрать current id'шки этого сета
	rows, err := tx.Query(ctx,
		`SELECT id FROM portfolio_images WHERE portfolio_item_id = $1`, itemID)
	if err != nil {
		return nil, fmt.Errorf("load current ids: %w", err)
	}
	currentSet := make(map[uuid.UUID]struct{})
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		currentSet[id] = struct{}{}
	}
	rows.Close()
	// 3. валидация: длины совпадают + все id'шки запроса есть в current set'е
	if len(imageIDs) != len(currentSet) {
		return nil, fmt.Errorf("%w: image_ids count mismatch (got %d, want %d)",
			ErrInvalidInput, len(imageIDs), len(currentSet))
	}
	for _, id := range imageIDs {
		if _, ok := currentSet[id]; !ok {
			return nil, fmt.Errorf("%w: image_id %s не принадлежит этому сету", ErrInvalidInput, id)
		}
	}
	// 4. UPDATE sort_order для каждого
	for i, id := range imageIDs {
		if _, err := tx.Exec(ctx,
			`UPDATE portfolio_images SET sort_order = $2 WHERE id = $1`, id, i); err != nil {
			return nil, fmt.Errorf("update sort_order: %w", err)
		}
	}
	// 5. обновить cover у parent (новое первое фото)
	var coverURL string
	if err := tx.QueryRow(ctx,
		`SELECT image_url FROM portfolio_images WHERE id = $1`, imageIDs[0],
	).Scan(&coverURL); err != nil {
		return nil, fmt.Errorf("get new cover: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE portfolio_items SET thumbnail_url = $2, updated_at = now() WHERE id = $1`,
		itemID, coverURL); err != nil {
		return nil, fmt.Errorf("update cover: %w", err)
	}
	// 6. вернуть обновлённый порядок — через tx, иначе UPDATE'ы sort_order
	//    из этой же tx не видны pool-connection'у.
	byItem, err := r.loadImagesByItemsInTx(ctx, tx, []uuid.UUID{itemID})
	if err != nil {
		return nil, err
	}
	return byItem[itemID], nil
}

// UpdatePortfolioMetaInTx — частичный апдейт title/description одного айтема
// (любой kind). Ownership проверяется через user_id=$2.
// expectedUpdatedAt — optimistic-lock на portfolio_items.updated_at. nil = без
// проверки. Возвращает ErrNotFound если айтема нет / чужой, ErrConflict если
// версия не совпала.
func (r *Repo) UpdatePortfolioMetaInTx(ctx context.Context, tx pgx.Tx, userID, itemID uuid.UUID, in PortfolioPatchInput) (PortfolioItem, error) {
	const q = `
UPDATE portfolio_items
   SET title       = COALESCE($3, title),
       description = COALESCE($4, description),
       updated_at  = now()
 WHERE id = $1 AND user_id = $2
   AND ($5::timestamptz IS NULL OR updated_at = $5)
RETURNING id, kind, title, description,
          COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
          category_codes, sort_order, created_at, updated_at,
          COALESCE(preview_url, ''), COALESCE(animated_thumb_url, ''), preview_status`
	var p PortfolioItem
	err := tx.QueryRow(ctx, q, itemID, userID, in.Title, in.Description, in.UpdatedAt).Scan(
		&p.ID, &p.Kind, &p.Title, &p.Description,
		&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
		&p.CategoryCodes, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt,
		&p.PreviewURL, &p.AnimatedThumbURL, &p.PreviewStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if in.UpdatedAt != nil {
			return PortfolioItem{}, ErrConflict
		}
		return PortfolioItem{}, ErrNotFound
	}
	if err != nil {
		return PortfolioItem{}, fmt.Errorf("update portfolio meta: %w", err)
	}
	return p, nil
}

// DeletePortfolioImageInTx — удаляет одно фото из photo-set'а. Возвращает
// (parentItemID, leftAfterDelete, coverImageURL, error). coverImageURL —
// URL первого фото после удаления (по sort_order). Если фото не найдено
// или не принадлежит юзеру — ErrNotFound.
//
// JOIN с portfolio_items нужен чтобы заодно проверить ownership: фронт
// шлёт img_id, но мы не имеем item_id в URL, поэтому валидируем через
// родителя в одном запросе.
func (r *Repo) DeletePortfolioImageInTx(ctx context.Context, tx pgx.Tx, userID, imageID uuid.UUID) (parentItemID uuid.UUID, leftAfterDelete int, coverURL string, err error) {
	// Lock parent item первым: сериализует delete с reorder/append/другим delete
	// на этом же сете. Без lock'a параллельные delete'ы последних двух фото
	// оба видели бы count=1 в своём READ COMMITTED snapshot'е и оба делали бы
	// UPDATE cover вместо DELETE parent — сет оставался бы пустым.
	const qLock = `
SELECT pi.id FROM portfolio_items pi
JOIN portfolio_images i ON i.portfolio_item_id = pi.id
WHERE i.id = $1 AND pi.user_id = $2 AND pi.kind = 'image'
FOR UPDATE OF pi`
	if err := tx.QueryRow(ctx, qLock, imageID, userID).Scan(&parentItemID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, 0, "", ErrNotFound
		}
		return uuid.Nil, 0, "", fmt.Errorf("lock parent for delete: %w", err)
	}
	const qDelete = `
DELETE FROM portfolio_images i
USING portfolio_items pi
WHERE i.id = $1
  AND i.portfolio_item_id = pi.id
  AND pi.user_id = $2
  AND pi.kind = 'image'
RETURNING pi.id`
	if err := tx.QueryRow(ctx, qDelete, imageID, userID).Scan(&parentItemID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, 0, "", ErrNotFound
		}
		return uuid.Nil, 0, "", fmt.Errorf("delete portfolio image: %w", err)
	}
	const qCount = `
SELECT count(*), COALESCE((
    SELECT image_url FROM portfolio_images
     WHERE portfolio_item_id = $1
     ORDER BY sort_order, created_at
     LIMIT 1
), '')
FROM portfolio_images WHERE portfolio_item_id = $1`
	if err := tx.QueryRow(ctx, qCount, parentItemID).Scan(&leftAfterDelete, &coverURL); err != nil {
		return uuid.Nil, 0, "", fmt.Errorf("count remaining images: %w", err)
	}
	return parentItemID, leftAfterDelete, coverURL, nil
}

// UpdatePortfolioCoverThumbnailInTx — синхронизирует portfolio_items.thumbnail_url
// с первым кадром photo-set'а после удаления (или другой перестановки).
// Без отдельной транзакции — вызывается изнутри tx.
func (r *Repo) UpdatePortfolioCoverThumbnailInTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, coverURL string) error {
	_, err := tx.Exec(ctx,
		`UPDATE portfolio_items SET thumbnail_url = NULLIF($2, ''), updated_at = now() WHERE id = $1`,
		itemID, coverURL)
	if err != nil {
		return fmt.Errorf("update cover: %w", err)
	}
	return nil
}

// DeletePortfolioItemByIDInTx — удаляет portfolio_item без проверки ownership
// (caller гарантирует это через предыдущие шаги, обычно после
// DeletePortfolioImageInTx когда фото в сете не осталось).
// CASCADE снесёт оставшиеся portfolio_images автоматически.
func (r *Repo) DeletePortfolioItemByIDInTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM portfolio_items WHERE id = $1`, itemID)
	if err != nil {
		return fmt.Errorf("delete portfolio item: %w", err)
	}
	return nil
}

// DeletePortfolioItemInTx — удаляет видео внутри tx. Outbox-эмиссия в той же
// tx, чтобы ES (last_video_at у спеца) обновился атомарно.
// Возвращает ErrNotFound если запись не найдена/чужая (без утечки factов
// о существовании чужих ID).
func (r *Repo) DeletePortfolioItemInTx(ctx context.Context, tx pgx.Tx, userID, itemID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM portfolio_items WHERE id = $1 AND user_id = $2`,
		itemID, userID)
	if err != nil {
		return fmt.Errorf("delete portfolio: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) listReviews(ctx context.Context, userID uuid.UUID) ([]Review, error) {
	// author_user_id отдаём как есть — фронт UI всё равно показывает
	// «Клиент» через AuthorName, но у нас (support/ops) есть UUID
	// для поднятия email и связи с недовольным.
	rows, err := r.db.Query(ctx, `
SELECT id, author_user_id, COALESCE(NULLIF(author_name, ''), 'Клиент'), rating, text, created_at
FROM reviews
WHERE target_user_id = $1
ORDER BY created_at DESC
LIMIT 20`, userID)
	if err != nil {
		return nil, fmt.Errorf("query reviews: %w", err)
	}
	defer rows.Close()
	out := make([]Review, 0, 8)
	for rows.Next() {
		var rev Review
		if err := rows.Scan(&rev.ID, &rev.AuthorUserID, &rev.AuthorName, &rev.Rating, &rev.Text, &rev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// LoadReferencedMediaURLs возвращает все непустые URL медиа из БД одним
// UNION-запросом. Используется orphan-sweep'ом: всё что не в этом списке
// (после извлечения ключа) — кандидат на удаление из bucket'а. Внешние
// URL (youtube и т.п.) тоже в выдаче — каллер их отфильтрует через
// s3.Client.KeyFromURL → "".
//
// ВАЖНО: при добавлении нового медиа-поля в portfolio_items / specialist_profiles
// нужно добавить его сюда, иначе sweep сожрёт продакшен-файлы. Сейчас покрыты:
//   - specialist_profiles.avatar_url
//   - portfolio_items.video_url       (оригинал)
//   - portfolio_items.thumbnail_url   (превью-картинка)
//   - portfolio_items.preview_url     (480p MP4 — docs/VIDEO_TRANSCODING.md)
//   - portfolio_items.animated_thumb_url (animated WebP «гифка» — §11 docs)
func (r *Repo) LoadReferencedMediaURLs(ctx context.Context) ([]string, error) {
	rows, err := r.db.Query(ctx, `
SELECT url FROM (
  SELECT avatar_url         AS url FROM specialist_profiles WHERE avatar_url         IS NOT NULL AND avatar_url         <> ''
  UNION ALL
  SELECT video_url          AS url FROM portfolio_items     WHERE video_url          IS NOT NULL AND video_url          <> ''
  UNION ALL
  SELECT thumbnail_url      AS url FROM portfolio_items     WHERE thumbnail_url      IS NOT NULL AND thumbnail_url      <> ''
  UNION ALL
  SELECT preview_url        AS url FROM portfolio_items     WHERE preview_url        IS NOT NULL AND preview_url        <> ''
  UNION ALL
  SELECT animated_thumb_url AS url FROM portfolio_items     WHERE animated_thumb_url IS NOT NULL AND animated_thumb_url <> ''
  UNION ALL
  SELECT image_url           AS url FROM portfolio_images    WHERE image_url          IS NOT NULL AND image_url          <> ''
) t`)
	if err != nil {
		return nil, fmt.Errorf("load referenced media: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 64)
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, err
		}
		out = append(out, url)
	}
	return out, rows.Err()
}

// ModerationQueueItem — карточка спеца в админ-очереди модерации.
// Достаточно того, что админ видит сходу, чтобы понять кто это.
type ModerationQueueItem struct {
	UserID          uuid.UUID  `json:"user_id"`
	Email           string     `json:"email,omitempty"`
	DisplayName     string     `json:"display_name"`
	AvatarURL       string     `json:"avatar_url,omitempty"`
	Bio             string     `json:"bio"`
	City            string     `json:"city,omitempty"`
	PrimaryCategory string     `json:"primary_category,omitempty"`
	IsFreelance     bool       `json:"is_freelance"`
	ProductionName  string     `json:"production_name,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
	Status          string     `json:"moderation_status"`
	Reason          string     `json:"moderation_reason,omitempty"`
}

// ListModerationQueue — очередь модерации. status="pending_review" (default)
// — все ожидающие админа. status="rejected" — отклонённые (для retrospect).
// status="approved" — одобренные (для аудита). status=""|"all" — все.
//
// Сортировка FIFO по updated_at (старые сверху) — «никого не забыли». Cursor-
// пагинация была бы overkill: pending'ов на проде десятки, не миллионы.
func (r *Repo) ListModerationQueue(ctx context.Context, status string, limit, offset int) ([]ModerationQueueItem, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	var (
		filter  string
		args    []any
		argIdx  = 1
	)
	switch status {
	case "", "all":
		filter = "WHERE p.is_published = TRUE"
	case "pending_review", "approved", "rejected":
		filter = "WHERE p.is_published = TRUE AND p.moderation_status = $1"
		args = append(args, status)
		argIdx = 2
	default:
		return nil, 0, fmt.Errorf("invalid moderation status: %q", status)
	}

	var total int
	countQ := `SELECT count(*) FROM specialist_profiles p ` + filter
	if err := r.db.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count moderation queue: %w", err)
	}

	listQ := `
SELECT p.user_id,
       COALESCE(u.email::text, ''),
       p.display_name,
       COALESCE(p.avatar_url, ''),
       COALESCE(p.bio, ''),
       COALESCE(p.city, ''),
       COALESCE((SELECT category_code FROM specialist_categories
                 WHERE user_id = p.user_id AND is_primary LIMIT 1), ''),
       p.is_freelance,
       COALESCE(pr.name, ''),
       p.updated_at,
       p.moderation_status,
       COALESCE(p.moderation_reason, '')
FROM specialist_profiles p
JOIN users u ON u.id = p.user_id
LEFT JOIN productions pr ON pr.id = p.production_id AND pr.is_active = TRUE
` + filter + fmt.Sprintf(`
ORDER BY p.updated_at ASC
LIMIT $%d OFFSET $%d`, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := r.db.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list moderation queue: %w", err)
	}
	defer rows.Close()
	out := make([]ModerationQueueItem, 0, limit)
	for rows.Next() {
		var it ModerationQueueItem
		if err := rows.Scan(
			&it.UserID, &it.Email,
			&it.DisplayName, &it.AvatarURL, &it.Bio, &it.City,
			&it.PrimaryCategory, &it.IsFreelance, &it.ProductionName,
			&it.UpdatedAt, &it.Status, &it.Reason,
		); err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// LoadModerationNotifyInfo — компактный набор полей для уведомления
// админа (email + display_name) в одной выборке внутри tx.
func (r *Repo) LoadModerationNotifyInfo(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (email, displayName string, err error) {
	err = tx.QueryRow(ctx, `
SELECT COALESCE(u.email::text, ''), COALESCE(p.display_name, '')
FROM specialist_profiles p
JOIN users u ON u.id = p.user_id
WHERE p.user_id = $1`, userID).Scan(&email, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("load notify info: %w", err)
	}
	return email, displayName, nil
}

// CountPendingModeration — счётчик для бейджа в админ-навигации. Возвращает
// число pending_review, у которых is_published=TRUE.
func (r *Repo) CountPendingModeration(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `
SELECT count(*) FROM specialist_profiles
WHERE is_published = TRUE AND moderation_status = 'pending_review'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// ModerationDetail — детальная карточка для admin-просмотра. Поверх
// публичных полей PublicProfile содержит updated_at (для optimistic-lock'a
// на Approve/Reject) и moderation_status + reason для UI.
type ModerationDetail struct {
	PublicProfile
	UpdatedAt        time.Time  `json:"updated_at"`
	ModerationStatus string     `json:"moderation_status"`
	ModerationReason string     `json:"moderation_reason,omitempty"`
	Email            string     `json:"email,omitempty"`
}

// GetForModeration — полная карточка спеца для admin'ского просмотра,
// без фильтра is_published / moderation_status. Возвращает то же самое,
// что и GetPublic, плюс updated_at и текущий moderation_status (фронт
// использует updated_at как optimistic-lock версию).
func (r *Repo) GetForModeration(ctx context.Context, userID uuid.UUID) (ModerationDetail, error) {
	const q = `
SELECT p.user_id, p.display_name, p.bio,
       COALESCE(p.avatar_url, ''), COALESCE(p.city, ''),
       p.rate_min, p.rate_max, p.currency,
       p.rating_avg, p.reviews_count,
       COALESCE(pr.name, ''), p.is_freelance,
       p.updated_at, p.moderation_status,
       COALESCE(p.moderation_reason, ''),
       COALESCE(u.email::text, '')
FROM specialist_profiles p
JOIN users u ON u.id = p.user_id
LEFT JOIN productions pr ON pr.id = p.production_id AND pr.is_active = TRUE
WHERE p.user_id = $1`
	var d ModerationDetail
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&d.UserID, &d.DisplayName, &d.Bio, &d.AvatarURL, &d.City,
		&d.RateMin, &d.RateMax, &d.Currency, &d.RatingAvg, &d.ReviewsCount,
		&d.ProductionName, &d.IsFreelance,
		&d.UpdatedAt, &d.ModerationStatus, &d.ModerationReason,
		&d.Email,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModerationDetail{}, ErrNotFound
	}
	if err != nil {
		return ModerationDetail{}, fmt.Errorf("query moderation profile: %w", err)
	}
	cats, err := r.listCategoriesWithTitles(ctx, userID)
	if err != nil {
		return ModerationDetail{}, err
	}
	d.Categories = cats
	skills, err := r.listSkillsWithTitles(ctx, userID)
	if err != nil {
		return ModerationDetail{}, err
	}
	d.Skills = skills
	portfolio, err := r.listPortfolio(ctx, userID)
	if err != nil {
		return ModerationDetail{}, err
	}
	d.Portfolio = portfolio
	return d, nil
}

func (r *Repo) ValidSkillIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, `SELECT id FROM skills WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("validate skills: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
