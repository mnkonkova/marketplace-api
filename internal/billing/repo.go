package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrAlreadyConfirmed — платёж подтверждён, второй раз не подтверждается.
	ErrAlreadyConfirmed = errors.New("payment already confirmed")
	// ErrAccrualLocked — начисление утверждено или выплачено: пересчитывать
	// его нельзя. Иначе цифра, по которой уже перевели деньги, поменяется
	// задним числом.
	ErrAccrualLocked = errors.New("accrual is approved or paid")
	// ErrWrongAccrualStatus — кнопка нажата не в том порядке: выплатить
	// можно только утверждённое.
	ErrWrongAccrualStatus = errors.New("wrong accrual status for this action")
)

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// nameExpr — та же лесенка имени, что во всех остальных доменах.
const nameExpr = `COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       )`

// Terms — условия проекта. Нет строки — нули: проект, которому условия ещё
// не задали, показывает нули, а не чужие ставки.
func (r *Repo) Terms(ctx context.Context, projectID uuid.UUID) (Terms, error) {
	t := Terms{ProjectID: projectID}
	err := r.db.QueryRow(ctx, `
SELECT terms_version_id, salary_per_month, videos_first_month, videos_next_months,
       rate_per_1000_views, bonus_views_threshold, rate_per_1000_views_over,
       click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
       creator_salary_per_month, creator_rate_per_1000_views,
       creator_rate_per_1000_views_over, updated_at
FROM project_billing WHERE project_id = $1`, projectID).
		Scan(&t.TermsVersionID, &t.SalaryPerMonth, &t.VideosFirstMonth, &t.VideosNextMonths,
			&t.RatePer1000Views, &t.BonusViewsThreshold, &t.RatePer1000ViewsOver,
			&t.ClickBonusRate, &t.ClickBonusThreshold, &t.ClickBonusRateOver,
			&t.CreatorSalaryPerMonth, &t.CreatorRatePer1000Views,
			&t.CreatorRatePer1000ViewsOver, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, nil
	}
	if err != nil {
		return Terms{}, fmt.Errorf("load project billing: %w", err)
	}
	return t, nil
}

// SaveTerms — задать условия проекта.
func (r *Repo) SaveTerms(ctx context.Context, t Terms, actor uuid.UUID) (Terms, error) {
	if err := r.db.QueryRow(ctx, `
INSERT INTO project_billing
  (project_id, terms_version_id, salary_per_month,
   videos_first_month, videos_next_months, rate_per_1000_views,
   bonus_views_threshold, rate_per_1000_views_over,
   click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
   creator_salary_per_month, creator_rate_per_1000_views,
   creator_rate_per_1000_views_over,
   updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now())
ON CONFLICT (project_id) DO UPDATE SET
  terms_version_id = EXCLUDED.terms_version_id,
  salary_per_month = EXCLUDED.salary_per_month,
  videos_first_month = EXCLUDED.videos_first_month,
  videos_next_months = EXCLUDED.videos_next_months,
  rate_per_1000_views = EXCLUDED.rate_per_1000_views,
  bonus_views_threshold = EXCLUDED.bonus_views_threshold,
  rate_per_1000_views_over = EXCLUDED.rate_per_1000_views_over,
  click_bonus_rate = EXCLUDED.click_bonus_rate,
  click_bonus_threshold = EXCLUDED.click_bonus_threshold,
  click_bonus_rate_over = EXCLUDED.click_bonus_rate_over,
  creator_salary_per_month = EXCLUDED.creator_salary_per_month,
  creator_rate_per_1000_views = EXCLUDED.creator_rate_per_1000_views,
  creator_rate_per_1000_views_over = EXCLUDED.creator_rate_per_1000_views_over,
  updated_by = EXCLUDED.updated_by,
  updated_at = now()
RETURNING updated_at`,
		t.ProjectID, t.TermsVersionID, t.SalaryPerMonth,
		t.VideosFirstMonth, t.VideosNextMonths, t.RatePer1000Views,
		t.BonusViewsThreshold, t.RatePer1000ViewsOver,
		t.ClickBonusRate, t.ClickBonusThreshold, t.ClickBonusRateOver,
		t.CreatorSalaryPerMonth, t.CreatorRatePer1000Views, t.CreatorRatePer1000ViewsOver,
		actor).Scan(&t.UpdatedAt); err != nil {
		return Terms{}, fmt.Errorf("save project billing: %w", err)
	}
	return t, nil
}

// LatestTerms — действующая версия правил, чтобы снять с неё числа.
func (r *Repo) LatestTerms(ctx context.Context) (Terms, error) {
	var t Terms
	err := r.db.QueryRow(ctx, `
SELECT id, salary_per_month, videos_first_month, videos_next_months,
       rate_per_1000_views, bonus_views_threshold,
       rate_per_1000_views_over, click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
       creator_salary_per_month, creator_rate_per_1000_views, creator_rate_per_1000_views_over
FROM terms_versions ORDER BY version DESC LIMIT 1`).
		Scan(&t.TermsVersionID, &t.SalaryPerMonth, &t.VideosFirstMonth, &t.VideosNextMonths,
			&t.RatePer1000Views, &t.BonusViewsThreshold, &t.RatePer1000ViewsOver,
			&t.ClickBonusRate, &t.ClickBonusThreshold, &t.ClickBonusRateOver,
			&t.CreatorSalaryPerMonth, &t.CreatorRatePer1000Views, &t.CreatorRatePer1000ViewsOver)
	if errors.Is(err, pgx.ErrNoRows) {
		return Terms{}, ErrNotFound
	}
	if err != nil {
		return Terms{}, fmt.Errorf("load latest terms: %w", err)
	}
	return t, nil
}

// ---- платежи ----

func (r *Repo) Payments(ctx context.Context, projectID uuid.UUID) ([]Payment, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, project_id, kind::text, amount, status::text, note,
       confirmed_by, confirmed_at, created_at
FROM project_payments WHERE project_id = $1 ORDER BY kind`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list payments: %w", err)
	}
	defer rows.Close()
	out := make([]Payment, 0, 2)
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Kind, &p.Amount, &p.Status,
			&p.Note, &p.ConfirmedBy, &p.ConfirmedAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan payment: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPayment — завести или поправить ожидаемый платёж. Подтверждённый
// не трогаем: сумма, по которой уже отчитались, задним числом не меняется.
func (r *Repo) UpsertPayment(ctx context.Context, projectID uuid.UUID, kind PaymentKind, amount int64, note string, actor uuid.UUID) (Payment, error) {
	var p Payment
	err := r.db.QueryRow(ctx, `
INSERT INTO project_payments (project_id, kind, amount, note, created_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, kind) DO UPDATE
  SET amount = EXCLUDED.amount, note = EXCLUDED.note
  WHERE project_payments.status <> 'confirmed'
RETURNING id, project_id, kind::text, amount, status::text, note,
          confirmed_by, confirmed_at, created_at`,
		projectID, kind, amount, note, actor).
		Scan(&p.ID, &p.ProjectID, &p.Kind, &p.Amount, &p.Status, &p.Note,
			&p.ConfirmedBy, &p.ConfirmedAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT ... WHERE не сработал — значит платёж подтверждён.
		return Payment{}, ErrAlreadyConfirmed
	}
	if err != nil {
		return Payment{}, fmt.Errorf("upsert payment: %w", err)
	}
	return p, nil
}

// ConfirmPayment — кнопка менеджера «деньги пришли».
func (r *Repo) ConfirmPayment(ctx context.Context, projectID uuid.UUID, kind PaymentKind, actor uuid.UUID) (Payment, error) {
	var p Payment
	err := r.db.QueryRow(ctx, `
UPDATE project_payments
SET status = 'confirmed', confirmed_by = $3, confirmed_at = now()
WHERE project_id = $1 AND kind = $2 AND status = 'awaiting'
RETURNING id, project_id, kind::text, amount, status::text, note,
          confirmed_by, confirmed_at, created_at`, projectID, kind, actor).
		Scan(&p.ID, &p.ProjectID, &p.Kind, &p.Amount, &p.Status, &p.Note,
			&p.ConfirmedBy, &p.ConfirmedAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Либо платежа нет вовсе, либо он уже подтверждён. Разделяем:
		// «нечего подтверждать» и «уже подтверждено» — разные ответы.
		var exists bool
		if err := r.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM project_payments WHERE project_id = $1 AND kind = $2)`,
			projectID, kind).Scan(&exists); err != nil {
			return Payment{}, fmt.Errorf("check payment: %w", err)
		}
		if exists {
			return Payment{}, ErrAlreadyConfirmed
		}
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("confirm payment: %w", err)
	}
	return p, nil
}

// ---- начисления ----

func (r *Repo) Accruals(ctx context.Context, projectID uuid.UUID, periodStart *time.Time, creatorID *uuid.UUID) ([]Accrual, error) {
	rows, err := r.db.Query(ctx, `
SELECT a.id, a.project_id, a.creator_user_id, `+nameExpr+`, a.period_start,
       a.salary, a.videos_planned, a.videos_delivered, a.deduction,
       a.views_total, a.views_base, a.views_over, a.views_bonus,
       a.clicks, a.click_bonus, a.total,
       a.payout_salary, a.payout_deduction, a.payout_views_bonus,
       a.payout_click_bonus, a.payout_total, a.status::text,
       -- Приоритет из подборки: «команда собрана по вашему приоритету».
       -- Есть только у проектов, выросших из заказа.
       COALESCE(oc.priority, 0),
       a.approved_at, a.paid_at, a.calculated_at
FROM creator_accruals a
LEFT JOIN users u                ON u.id = a.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = a.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = a.creator_user_id
LEFT JOIN creator_orders o       ON o.project_id = a.project_id
LEFT JOIN order_candidates oc    ON oc.order_id = o.id
                                AND oc.creator_user_id = a.creator_user_id
WHERE a.project_id = $1
  AND ($2::date IS NULL OR a.period_start = $2)
  AND ($3::uuid IS NULL OR a.creator_user_id = $3)
ORDER BY a.period_start DESC, COALESCE(oc.priority, 999), 4`, projectID, periodStart, creatorID)
	if err != nil {
		return nil, fmt.Errorf("list accruals: %w", err)
	}
	defer rows.Close()
	out := make([]Accrual, 0)
	for rows.Next() {
		var a Accrual
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.CreatorUserID, &a.CreatorName,
			&a.PeriodStart, &a.Salary, &a.VideosPlanned, &a.VideosDelivered,
			&a.Deduction, &a.ViewsTotal, &a.ViewsBase, &a.ViewsOver, &a.ViewsBonus,
			&a.Clicks, &a.ClickBonus, &a.Total,
			&a.PayoutSalary, &a.PayoutDeduction, &a.PayoutViewsBonus,
			&a.PayoutClickBonus, &a.PayoutTotal, &a.Status, &a.Priority,
			&a.ApprovedAt, &a.PaidAt, &a.CalculatedAt); err != nil {
			return nil, fmt.Errorf("scan accrual: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// creatorPeriod — сырые числа по одному креатору за месяц.
type creatorPeriod struct {
	CreatorID  uuid.UUID
	Planned    int
	Delivered  int
	ViewsTotal int64
	// ViewsBase/ViewsOver — просмотры до порога и сверх него, посчитанные
	// ПО КАЖДОМУ РОЛИКУ отдельно и потом сложенные. Считать по сумме за
	// месяц нельзя: порог стоит на ролике, и два ролика по 600 000 — это
	// ноль сверхпорогового объёма, а не 200 000.
	ViewsBase int64
	ViewsOver int64
	Clicks    int
}

// periodFacts — что креаторы проекта наработали за месяц.
//
// Просмотры берутся по последнему снимку каждой ссылки и складываются в
// ролик целиком: порог «миллион» стоит на ролике, а не на площадке.
func (r *Repo) periodFacts(ctx context.Context, projectID uuid.UUID, p ProjectPeriod, threshold int64) ([]creatorPeriod, error) {
	rows, err := r.db.Query(ctx, `
WITH pub AS (
    SELECT f.id, f.creator_user_id, f.status,
           COALESCE(SUM(cur.views), 0) AS views
    FROM (`+publishedInPeriodSQL+`) f
    JOIN publication_links l ON l.publication_id = f.id
    LEFT JOIN LATERAL (
        SELECT views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    GROUP BY f.id, f.creator_user_id, f.status
)
SELECT pc.creator_user_id,
       COUNT(pub.id),
       COUNT(pub.id) FILTER (WHERE pub.status IN ('done', 'closed_manually')),
       COALESCE(SUM(pub.views), 0),
       -- Ступени считаются по каждому ролику и складываются.
       --
       -- COALESCE ВНУТРИ обязателен: у креатора без выкладок LEFT JOIN
       -- даёт строку с NULL, а LEAST(NULL, порог) в PostgreSQL — это не
       -- NULL, а порог: NULL'ы он молча пропускает. Человек без единого
       -- ролика получал бонус за миллион просмотров.
       COALESCE(SUM(LEAST(COALESCE(pub.views, 0), $5)), 0),
       COALESCE(SUM(GREATEST(COALESCE(pub.views, 0) - $5, 0)), 0),
       COALESCE(MAX(utm.clicks), 0)
FROM project_creators pc
LEFT JOIN pub ON pub.creator_user_id = pc.creator_user_id
LEFT JOIN creator_utm_links utm
       ON utm.project_id = pc.project_id AND utm.creator_user_id = pc.creator_user_id
WHERE pc.project_id = $1 AND pc.removed_at IS NULL
GROUP BY pc.creator_user_id`, projectID, projectID, p.StartsOn, p.EndsOn, threshold)
	if err != nil {
		return nil, fmt.Errorf("period facts: %w", err)
	}
	defer rows.Close()
	out := make([]creatorPeriod, 0)
	for rows.Next() {
		var c creatorPeriod
		if err := rows.Scan(&c.CreatorID, &c.Planned, &c.Delivered,
			&c.ViewsTotal, &c.ViewsBase, &c.ViewsOver, &c.Clicks); err != nil {
			return nil, fmt.Errorf("scan period facts: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// periodFactsLocked — то же, что periodFacts, но по срезу
// зафиксированного месяца.
//
// Запрос повторяет форму живого не от лени: правила счёта у месяца одни,
// меняется только источник чисел. Разойдись они — зафиксированный месяц
// начал бы считаться иначе, чем считался в день фиксации, и весь смысл
// фиксации пропал бы.
//
// Состав месяца тоже из среза: выкладку могли удалить или отменить после
// фиксации, а в зафиксированном месяце она обязана остаться.
func (r *Repo) periodFactsLocked(ctx context.Context, projectID uuid.UUID, p ProjectPeriod, threshold int64) ([]creatorPeriod, error) {
	rows, err := r.db.Query(ctx, `
WITH pub AS (
    -- SUM пропускает NULL, и «данных на отсечку не было» превращается в
    -- ноль. Для денег это верно — платить не за что, — но само отличие
    -- не теряется: в срезе у такой строки stat_date пуст, а период, где
    -- числа были уничтожены, помечен приблизительным.
    SELECT sp.publication_id AS id, sp.creator_user_id, sp.status,
           COALESCE(SUM(sv.views), 0) AS views
    FROM project_period_publications sp
    LEFT JOIN project_period_views sv
           ON sv.period_id = sp.period_id
          AND sv.publication_id = sp.publication_id
    WHERE sp.period_id = $2
    GROUP BY sp.publication_id, sp.creator_user_id, sp.status
)
SELECT pc.creator_user_id,
       COUNT(pub.id),
       COUNT(pub.id) FILTER (WHERE pub.status IN ('done', 'closed_manually')),
       COALESCE(SUM(pub.views), 0),
       COALESCE(SUM(LEAST(COALESCE(pub.views, 0), $3)), 0),
       COALESCE(SUM(GREATEST(COALESCE(pub.views, 0) - $3, 0)), 0),
       COALESCE(MAX(utm.clicks), 0)
FROM project_creators pc
LEFT JOIN pub ON pub.creator_user_id = pc.creator_user_id
LEFT JOIN creator_utm_links utm
       ON utm.project_id = pc.project_id AND utm.creator_user_id = pc.creator_user_id
WHERE pc.project_id = $1 AND pc.removed_at IS NULL
GROUP BY pc.creator_user_id`, projectID, p.ID, threshold)
	if err != nil {
		return nil, fmt.Errorf("locked period facts: %w", err)
	}
	defer rows.Close()
	out := make([]creatorPeriod, 0)
	for rows.Next() {
		var c creatorPeriod
		if err := rows.Scan(&c.CreatorID, &c.Planned, &c.Delivered,
			&c.ViewsTotal, &c.ViewsBase, &c.ViewsOver, &c.Clicks); err != nil {
			return nil, fmt.Errorf("scan locked period facts: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveAccrual — записать пересчитанное. Утверждённое и выплаченное не
// трогаем: цифра, по которой уже перевели деньги, задним числом не меняется.
func (r *Repo) SaveAccrual(ctx context.Context, a Accrual) error {
	tag, err := r.db.Exec(ctx, `
INSERT INTO creator_accruals
  (project_id, creator_user_id, period_start, salary, videos_planned,
   videos_delivered, deduction, views_total, views_base, views_over, views_bonus,
   clicks, click_bonus, total,
   payout_salary, payout_deduction, payout_views_bonus, payout_click_bonus, payout_total,
   calculated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19, now())
ON CONFLICT (project_id, creator_user_id, period_start) DO UPDATE SET
  salary = EXCLUDED.salary, videos_planned = EXCLUDED.videos_planned,
  videos_delivered = EXCLUDED.videos_delivered, deduction = EXCLUDED.deduction,
  views_total = EXCLUDED.views_total, views_base = EXCLUDED.views_base,
  views_over = EXCLUDED.views_over,
  views_bonus = EXCLUDED.views_bonus, clicks = EXCLUDED.clicks,
  click_bonus = EXCLUDED.click_bonus, total = EXCLUDED.total,
  payout_salary = EXCLUDED.payout_salary,
  payout_deduction = EXCLUDED.payout_deduction,
  payout_views_bonus = EXCLUDED.payout_views_bonus,
  payout_click_bonus = EXCLUDED.payout_click_bonus,
  payout_total = EXCLUDED.payout_total,
  calculated_at = now()
WHERE creator_accruals.status = 'draft'`,
		a.ProjectID, a.CreatorUserID, a.PeriodStart, a.Salary, a.VideosPlanned,
		a.VideosDelivered, a.Deduction, a.ViewsTotal, a.ViewsBase, a.ViewsOver,
		a.ViewsBonus, a.Clicks, a.ClickBonus, a.Total,
		a.PayoutSalary, a.PayoutDeduction, a.PayoutViewsBonus,
		a.PayoutClickBonus, a.PayoutTotal)
	if err != nil {
		return fmt.Errorf("save accrual: %w", err)
	}
	_ = tag
	return nil
}

// DecideAccrual — кнопки «утвердить период» и «выплачено».
//
// Порядок обязателен: выплатить можно только утверждённое. Иначе строка
// уходит в «выплачено» из черновика, минуя того, кто должен был на неё
// посмотреть.
func (r *Repo) DecideAccrual(ctx context.Context, accrualID, actor uuid.UUID, to AccrualStatus) (Accrual, error) {
	var from AccrualStatus
	switch to {
	case AccrualApproved:
		from = AccrualDraft
	case AccrualPaid:
		from = AccrualApproved
	default:
		return Accrual{}, ErrWrongAccrualStatus
	}

	col := "approved"
	if to == AccrualPaid {
		col = "paid"
	}
	var projectID uuid.UUID
	var creatorID uuid.UUID
	var periodStart time.Time
	err := r.db.QueryRow(ctx, `
UPDATE creator_accruals
SET status = $3, `+col+`_by = $2, `+col+`_at = now()
WHERE id = $1 AND status = $4
RETURNING project_id, creator_user_id, period_start`,
		accrualID, actor, to, from).Scan(&projectID, &creatorID, &periodStart)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := r.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM creator_accruals WHERE id = $1)`, accrualID).
			Scan(&exists); err != nil {
			return Accrual{}, fmt.Errorf("check accrual: %w", err)
		}
		if exists {
			return Accrual{}, ErrWrongAccrualStatus
		}
		return Accrual{}, ErrNotFound
	}
	if err != nil {
		return Accrual{}, fmt.Errorf("decide accrual: %w", err)
	}
	list, err := r.Accruals(ctx, projectID, &periodStart, &creatorID)
	if err != nil || len(list) == 0 {
		return Accrual{}, err
	}
	return list[0], nil
}

// ---- UTM ----

func (r *Repo) UTM(ctx context.Context, projectID uuid.UUID, creatorID *uuid.UUID) ([]UTMLink, error) {
	rows, err := r.db.Query(ctx, `
SELECT utm.creator_user_id, `+nameExpr+`, utm.url, utm.clicks, utm.updated_at
FROM creator_utm_links utm
LEFT JOIN users u                ON u.id = utm.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = utm.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = utm.creator_user_id
WHERE utm.project_id = $1 AND ($2::uuid IS NULL OR utm.creator_user_id = $2)
ORDER BY 2`, projectID, creatorID)
	if err != nil {
		return nil, fmt.Errorf("list utm: %w", err)
	}
	defer rows.Close()
	out := make([]UTMLink, 0)
	for rows.Next() {
		var l UTMLink
		if err := rows.Scan(&l.CreatorUserID, &l.CreatorName, &l.URL, &l.Clicks, &l.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan utm: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveUTM — метку ставит менеджер. Креатор её только видит.
func (r *Repo) SaveUTM(ctx context.Context, projectID, creatorID uuid.UUID, url string, actor uuid.UUID) (UTMLink, error) {
	var l UTMLink
	l.CreatorUserID = creatorID
	if err := r.db.QueryRow(ctx, `
INSERT INTO creator_utm_links (project_id, creator_user_id, url, created_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, creator_user_id) DO UPDATE
  SET url = EXCLUDED.url, updated_at = now()
RETURNING url, clicks, updated_at`, projectID, creatorID, url, actor).
		Scan(&l.URL, &l.Clicks, &l.UpdatedAt); err != nil {
		return UTMLink{}, fmt.Errorf("save utm: %w", err)
	}
	return l, nil
}

// firstOfMonth — первое число, как в creator_availability и в CHECK таблицы.
func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
