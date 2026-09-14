package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Прайс площадки: сколько платит клиент за креатора и сколько из этого
// получает сам креатор.
//
// Правится только админом и только созданием НОВОЙ версии. Существующую
// не переписываем: клиент согласился с конкретной версией, а проекты
// сняли с неё числа снимком — задним числом менять то, под чем стоит
// согласие, нельзя.

// TermsVersion — версия прайса в списке админа.
type TermsVersion struct {
	Terms
	// Version — номер версии; растёт на единицу.
	Version int `json:"version"`
	// Body — текст условий, с которым соглашается клиент.
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	// IsCurrent — эта версия сейчас действует. Действует всегда одна:
	// самая новая.
	IsCurrent bool `json:"is_current"`
	// ConsentedClients — сколько клиентов уже согласились именно с ней.
	// Показывает, что версию нельзя считать черновиком.
	ConsentedClients int `json:"consented_clients"`
	// UsedByProjects — сколько проектов сняли с неё числа.
	UsedByProjects int `json:"used_by_projects"`
}

// ListTermsVersions — весь прайс, свежие первыми.
func (r *Repo) ListTermsVersions(ctx context.Context) ([]TermsVersion, error) {
	rows, err := r.db.Query(ctx, `
SELECT v.id, v.version, v.body, v.published_at,
       v.salary_per_month, v.videos_first_month, v.videos_next_months,
       v.rate_per_1000_views, v.bonus_views_threshold, v.rate_per_1000_views_over,
       v.click_bonus_rate, v.click_bonus_threshold, v.click_bonus_rate_over,
       v.creator_salary_per_month, v.creator_rate_per_1000_views,
       v.creator_rate_per_1000_views_over,
       v.version = (SELECT MAX(version) FROM terms_versions),
       (SELECT COUNT(*) FROM client_terms_consents c WHERE c.terms_version_id = v.id),
       (SELECT COUNT(*) FROM project_billing b WHERE b.terms_version_id = v.id)
FROM terms_versions v
ORDER BY v.version DESC`)
	if err != nil {
		return nil, fmt.Errorf("list terms versions: %w", err)
	}
	defer rows.Close()
	out := make([]TermsVersion, 0)
	for rows.Next() {
		var v TermsVersion
		if err := rows.Scan(&v.TermsVersionID, &v.Version, &v.Body, &v.PublishedAt,
			&v.SalaryPerMonth, &v.VideosFirstMonth, &v.VideosNextMonths,
			&v.RatePer1000Views, &v.BonusViewsThreshold, &v.RatePer1000ViewsOver,
			&v.ClickBonusRate, &v.ClickBonusThreshold, &v.ClickBonusRateOver,
			&v.CreatorSalaryPerMonth, &v.CreatorRatePer1000Views,
			&v.CreatorRatePer1000ViewsOver,
			&v.IsCurrent, &v.ConsentedClients, &v.UsedByProjects); err != nil {
			return nil, fmt.Errorf("scan terms version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PublishTermsVersion — выпустить новую версию прайса.
func (r *Repo) PublishTermsVersion(ctx context.Context, v TermsVersion) (TermsVersion, error) {
	err := r.db.QueryRow(ctx, `
INSERT INTO terms_versions
  (version, body, salary_per_month, videos_first_month, videos_next_months,
   rate_per_1000_views, bonus_views_threshold, rate_per_1000_views_over,
   click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
   creator_salary_per_month, creator_rate_per_1000_views, creator_rate_per_1000_views_over)
VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions),
        $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING id, version, published_at`,
		v.Body, v.SalaryPerMonth, v.VideosFirstMonth, v.VideosNextMonths,
		v.RatePer1000Views, v.BonusViewsThreshold, v.RatePer1000ViewsOver,
		v.ClickBonusRate, v.ClickBonusThreshold, v.ClickBonusRateOver,
		v.CreatorSalaryPerMonth, v.CreatorRatePer1000Views, v.CreatorRatePer1000ViewsOver).
		Scan(&v.TermsVersionID, &v.Version, &v.PublishedAt)
	if err != nil {
		return TermsVersion{}, fmt.Errorf("publish terms version: %w", err)
	}
	v.IsCurrent = true
	return v, nil
}

// ---- сервис ----

func (s *Service) ListTermsVersions(ctx context.Context) ([]TermsVersion, error) {
	return s.repo.ListTermsVersions(ctx)
}

// PublishTermsVersion — проверить и выпустить.
func (s *Service) PublishTermsVersion(ctx context.Context, v TermsVersion) (TermsVersion, error) {
	v.Body = strings.TrimSpace(v.Body)
	if v.Body == "" {
		return TermsVersion{}, fmt.Errorf("%w: текст условий обязателен", ErrInvalidInput)
	}
	if len([]rune(v.Body)) > 100_000 {
		return TermsVersion{}, fmt.Errorf("%w: текст условий слишком длинный", ErrInvalidInput)
	}
	// Ставки проверяем тем же кодом, что и условия проекта: правила
	// одинаковые, и расходиться им незачем.
	if _, err := s.SaveTermsCheck(v.Terms); err != nil {
		return TermsVersion{}, err
	}
	// Креатору нельзя обещать больше, чем берём с клиента: это не тариф,
	// а убыток на каждом ролике, и почти всегда — опечатка в поле.
	c := v.Terms.CreatorSide()
	if c.SalaryPerMonth > v.SalaryPerMonth ||
		c.RatePer1000Views > v.RatePer1000Views ||
		c.RatePer1000ViewsOver > v.RatePer1000ViewsOver {
		return TermsVersion{}, fmt.Errorf(
			"%w: доля креатора больше, чем платит клиент — проверьте ставки", ErrInvalidInput)
	}
	return s.repo.PublishTermsVersion(ctx, v)
}

// SaveTermsCheck — только проверка ставок, без записи. Вынесена, чтобы
// прайс и условия проекта валидировались одинаково.
func (s *Service) SaveTermsCheck(t Terms) (Terms, error) {
	t.ProjectID = uuid.Nil
	return checkRates(t)
}
