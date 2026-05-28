package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

type Category struct {
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Icon        string `json:"icon,omitempty"`
	SortOrder   int    `json:"sort_order"`
}

func (r *Repo) ListCategories(ctx context.Context) ([]Category, error) {
	const q = `SELECT code, title, description, type, COALESCE(icon, ''), sort_order
               FROM specialty_categories ORDER BY sort_order, title`
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query categories: %w", err)
	}
	defer rows.Close()

	out := make([]Category, 0, 16)
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.Code, &c.Title, &c.Description, &c.Type, &c.Icon, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type Skill struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// SkillFilter — параметры фильтрации /skills. Все поля опциональны.
// Category — код категории из specialty_categories: фильтрует через JOIN
// со skill_categories. Платформы (kind='platform') в skill_categories не
// заводятся, поэтому при Category != "" они в выдачу не попадают —
// фронт показывает платформы отдельным блоком, см. cabinet.page.html.
type SkillFilter struct {
	Kind     string
	Category string
}

func (r *Repo) ListSkills(ctx context.Context, f SkillFilter) ([]Skill, error) {
	args := []any{}
	q := `SELECT DISTINCT s.id, s.slug, s.title, s.kind FROM skills s`
	if f.Category != "" {
		args = append(args, f.Category)
		q += fmt.Sprintf(` JOIN skill_categories sc ON sc.skill_id = s.id AND sc.category_code = $%d`, len(args))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		q += fmt.Sprintf(` WHERE s.kind = $%d`, len(args))
	}
	q += ` ORDER BY kind, title`

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query skills: %w", err)
	}
	defer rows.Close()

	out := make([]Skill, 0, 32)
	for rows.Next() {
		var s Skill
		if err := rows.Scan(&s.ID, &s.Slug, &s.Title, &s.Kind); err != nil {
			return nil, fmt.Errorf("scan skill: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
