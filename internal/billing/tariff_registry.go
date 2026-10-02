package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Реестр тарифов: строка — ПРОЕКТ.
//
// Прайс площадки один на всех, и до сих пор раздел «Прайс» показывал
// именно его: одну действующую версию и историю выпусков. Договариваются
// же с каждым заказчиком отдельно, и вопрос, который задают этому
// разделу, звучит не «какой у нас прайс», а «по каким условиям идёт вот
// этот проект и чем он отличается от соседнего».
//
// Ответить на него можно было только обойдя проекты по одному. Здесь —
// все сразу, одной таблицей: у кого ступени, у кого старая модель, у
// кого тарифа нет вовсе (а это значит, что проект считается по нулям).

// TariffRow — строка реестра. Всё, что нужно, чтобы увидеть проект и
// понять, надо ли в него заходить.
type TariffRow struct {
	ProjectID   uuid.UUID `json:"project_id"`
	Title       string    `json:"title"`
	ClientName  string    `json:"client_name,omitempty"`
	ManagerName string    `json:"manager_name,omitempty"`
	Status      string    `json:"status"`
	// HasTerms — снимок условий заведён. false означает «проект
	// считается по нулям», и это худшее из состояний: экран денег
	// показывает ровные нули, и выглядит это как «ещё не начислили».
	HasTerms bool `json:"has_terms"`
	// Stepped — тариф считается лесенкой. false при HasTerms=true
	// означает старую модель: оклад плюс ставка за тысячу.
	Stepped bool `json:"stepped"`
	// StepsCount/MinFee/MaxFee — во что обходится период заказчику на
	// нижней и верхней ступени. По ним видно вилку, не открывая проект.
	StepsCount int   `json:"steps_count"`
	MinFee     int64 `json:"min_fee"`
	MaxFee     int64 `json:"max_fee"`
	// FeePerVideo/CreatorFeePerVideo — фикс за ролик: основная цена
	// работы. Ступени и ставка за тысячу — надбавка за просмотры.
	FeePerVideo        *int64 `json:"fee_per_video,omitempty"`
	CreatorFeePerVideo *int64 `json:"creator_fee_per_video,omitempty"`
	// SalaryPerMonth/RatePer1000Views — числа старой модели. Показываем
	// их там, где ступеней нет: иначе строка выглядела бы пустой у
	// проекта, у которого тариф на самом деле задан.
	SalaryPerMonth   int64      `json:"salary_per_month"`
	RatePer1000Views int64      `json:"rate_per_1000_views"`
	GuaranteeViews   *int64     `json:"guarantee_views,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
}

// TariffRegistry — тарифы всех проектов с креаторами.
//
// Только creators_turnkey: у разового заказа и у проекта по воронке
// тарифа в этом смысле нет, и пустые строки в реестре означали бы, что
// про них забыли.
//
// Отменённые не показываем, завершённые — показываем: по ним ещё
// досчитываются периоды, и вопрос «почему счёт такой» задают именно про
// них.
func (r *Repo) TariffRegistry(ctx context.Context) ([]TariffRow, error) {
	rows, err := r.db.Query(ctx, `
SELECT pr.id, COALESCE(pr.title, ''), pr.status::text,
       COALESCE(NULLIF(cp.display_name, ''), NULLIF(cu.display_name, ''),
                split_part(cu.email, '@', 1), pr.client_name, '') AS client_name,
       COALESCE(NULLIF(sp.display_name, ''), NULLIF(mu.display_name, ''),
                split_part(mu.email, '@', 1), '') AS manager_name,
       (b.project_id IS NOT NULL) AS has_terms,
       COALESCE(b.salary_per_month, 0), COALESCE(b.rate_per_1000_views, 0),
       b.fee_per_video, b.creator_fee_per_video,
       b.guarantee_views, b.updated_at,
       COALESCE(s.cnt, 0), COALESCE(s.min_fee, 0), COALESCE(s.max_fee, 0)
FROM projects pr
LEFT JOIN project_billing b ON b.project_id = pr.id
LEFT JOIN users cu ON cu.id = pr.client_user_id
LEFT JOIN client_profiles cp ON cp.user_id = pr.client_user_id
LEFT JOIN users mu ON mu.id = pr.assigned_to_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = pr.assigned_to_user_id
LEFT JOIN LATERAL (
    SELECT count(*)::int AS cnt, MIN(client_fee) AS min_fee, MAX(client_fee) AS max_fee
    -- Только лесенка просмотров: в списке проектов колонка «модель»
    -- говорит, по чему считается ПЕРИОД. Ступени подписчиков — доплата
    -- сверху, и, попав сюда, они назвали бы модель чужим числом.
    FROM terms_steps WHERE project_id = pr.id AND kind = 'views'
) s ON TRUE
WHERE pr.kind = 'creators_turnkey'
  AND pr.status <> 'cancelled'
  AND pr.is_test = FALSE
ORDER BY (b.project_id IS NULL) DESC, pr.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("tariff registry: %w", err)
	}
	defer rows.Close()

	out := make([]TariffRow, 0, 16)
	for rows.Next() {
		var t TariffRow
		if err := rows.Scan(&t.ProjectID, &t.Title, &t.Status, &t.ClientName, &t.ManagerName,
			&t.HasTerms, &t.SalaryPerMonth, &t.RatePer1000Views,
			&t.FeePerVideo, &t.CreatorFeePerVideo,
			&t.GuaranteeViews, &t.UpdatedAt,
			&t.StepsCount, &t.MinFee, &t.MaxFee); err != nil {
			return nil, fmt.Errorf("scan tariff row: %w", err)
		}
		t.Stepped = t.StepsCount > 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// TariffRegistry — реестр тарифов проектов.
func (s *Service) TariffRegistry(ctx context.Context) ([]TariffRow, error) {
	return s.repo.TariffRegistry(ctx)
}
