package profiles

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("profile not found")

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

func (r *Repo) Pool() *pgxpool.Pool { return r.db }

func (r *Repo) Get(ctx context.Context, userID uuid.UUID) (Profile, error) {
	const q = `
SELECT p.user_id, p.display_name, p.bio,
       COALESCE(p.avatar_url, ''), COALESCE(p.city, ''),
       p.rate_min, p.rate_max, p.currency,
       p.is_published, p.rating_avg, p.reviews_count
FROM specialist_profiles p
WHERE p.user_id = $1`
	var p Profile
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&p.UserID, &p.DisplayName, &p.Bio,
		&p.AvatarURL, &p.City,
		&p.RateMin, &p.RateMax, &p.Currency,
		&p.IsPublished, &p.RatingAvg, &p.ReviewsCount,
	)
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
	const q = `
UPDATE specialist_profiles SET
  display_name = COALESCE($2, display_name),
  bio          = COALESCE($3, bio),
  avatar_url   = COALESCE($4, avatar_url),
  city         = COALESCE($5, city),
  rate_min     = CASE WHEN $6::boolean THEN $7 ELSE rate_min END,
  rate_max     = CASE WHEN $8::boolean THEN $9 ELSE rate_max END,
  currency     = COALESCE($10, currency),
  updated_at   = now()
WHERE user_id = $1`
	tag, err := tx.Exec(ctx, q,
		userID,
		in.DisplayName,
		in.Bio,
		in.AvatarURL,
		in.City,
		in.RateMin != nil, in.RateMin,
		in.RateMax != nil, in.RateMax,
		in.Currency,
	)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) ReplaceCategoriesInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, codes []string, primary string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM specialist_categories WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete categories: %w", err)
	}
	for _, code := range codes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO specialist_categories (user_id, category_code, is_primary) VALUES ($1, $2, $3)`,
			userID, code, code == primary); err != nil {
			return fmt.Errorf("insert category %s: %w", code, err)
		}
	}
	return nil
}

func (r *Repo) ReplaceSkillsInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, skillIDs []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM specialist_skills WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete skills: %w", err)
	}
	for _, sid := range skillIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO specialist_skills (user_id, skill_id) VALUES ($1, $2)`,
			userID, sid); err != nil {
			return fmt.Errorf("insert skill: %w", err)
		}
	}
	return nil
}

func (r *Repo) SetPublishedInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, published bool) error {
	tag, err := tx.Exec(ctx,
		`UPDATE specialist_profiles SET is_published = $2, updated_at = now() WHERE user_id = $1`,
		userID, published)
	if err != nil {
		return fmt.Errorf("update published: %w", err)
	}
	if tag.RowsAffected() == 0 {
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
	const q = `
SELECT p.user_id, p.display_name, p.bio,
       COALESCE(p.avatar_url, ''), COALESCE(p.city, ''),
       p.rate_min, p.rate_max, p.currency,
       p.rating_avg, p.reviews_count
FROM specialist_profiles p
WHERE p.user_id = $1 AND p.is_published = TRUE`
	var p PublicProfile
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&p.UserID, &p.DisplayName, &p.Bio, &p.AvatarURL, &p.City,
		&p.RateMin, &p.RateMax, &p.Currency, &p.RatingAvg, &p.ReviewsCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicProfile{}, ErrNotFound
	}
	if err != nil {
		return PublicProfile{}, fmt.Errorf("query public profile: %w", err)
	}

	cats, err := r.listCategoriesWithTitles(ctx, userID)
	if err != nil {
		return PublicProfile{}, err
	}
	p.Categories = cats

	skills, err := r.listSkillsWithTitles(ctx, userID)
	if err != nil {
		return PublicProfile{}, err
	}
	p.Skills = skills

	portfolio, err := r.listPortfolio(ctx, userID)
	if err != nil {
		return PublicProfile{}, err
	}
	p.Portfolio = portfolio

	reviews, err := r.listReviews(ctx, userID)
	if err != nil {
		return PublicProfile{}, err
	}
	p.Reviews = reviews

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
SELECT id, title, description,
       COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
       category_codes, sort_order, created_at
FROM portfolio_items
WHERE user_id = $1
ORDER BY sort_order, created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("query portfolio: %w", err)
	}
	defer rows.Close()
	out := make([]PortfolioItem, 0, 8)
	for rows.Next() {
		var p PortfolioItem
		if err := rows.Scan(
			&p.ID, &p.Title, &p.Description,
			&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
			&p.CategoryCodes, &p.SortOrder, &p.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPortfolio — экспортируемая обёртка для /me/portfolio (handler).
// listPortfolio (lower-case) дёргается из GetPublic, держим обе.
func (r *Repo) ListPortfolio(ctx context.Context, userID uuid.UUID) ([]PortfolioItem, error) {
	return r.listPortfolio(ctx, userID)
}

// CreatePortfolioVideo — добавляет видео-айтем. sort_order выставляем на
// MAX+1, чтобы новое видео было в конце списка (юзер потом перемешает).
func (r *Repo) CreatePortfolioVideo(ctx context.Context, userID uuid.UUID, in PortfolioCreateInput) (PortfolioItem, error) {
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
RETURNING id, title, description,
          COALESCE(video_url, ''), COALESCE(thumbnail_url, ''), COALESCE(external_url, ''),
          category_codes, sort_order, created_at`
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
	err := r.db.QueryRow(ctx, q,
		userID, in.Title, in.Description,
		in.VideoURL, in.ThumbnailURL,
		cats,
		dur, aspect,
	).Scan(
		&p.ID, &p.Title, &p.Description,
		&p.VideoURL, &p.ThumbnailURL, &p.ExternalURL,
		&p.CategoryCodes, &p.SortOrder, &p.CreatedAt,
	)
	if err != nil {
		return PortfolioItem{}, fmt.Errorf("insert portfolio: %w", err)
	}
	return p, nil
}

// DeletePortfolioItem — удаляет видео если оно принадлежит userID.
// Возвращает ErrNotFound если запись не найдена/чужая (без утечки factов
// о существовании чужих ID).
func (r *Repo) DeletePortfolioItem(ctx context.Context, userID, itemID uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
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
	rows, err := r.db.Query(ctx, `
SELECT id, COALESCE(NULLIF(author_name, ''), 'Клиент'), rating, text, created_at
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
		if err := rows.Scan(&rev.ID, &rev.AuthorName, &rev.Rating, &rev.Text, &rev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	return out, rows.Err()
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
