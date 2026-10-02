package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
       creator_rate_per_1000_views_over,
       step_views, first_period_fee, base_fee, step_fee,
       step_tier2_from, step_fee_over, step_cap_views, guarantee_views,
       creator_first_period_fee, creator_base_fee, creator_step_fee, creator_step_fee_over,
       subscriber_rate, creator_subscriber_rate,
       fee_per_video, creator_fee_per_video,
       project_cost,
       updated_at
FROM project_billing WHERE project_id = $1`, projectID).
		Scan(&t.TermsVersionID, &t.SalaryPerMonth, &t.VideosFirstMonth, &t.VideosNextMonths,
			&t.RatePer1000Views, &t.BonusViewsThreshold, &t.RatePer1000ViewsOver,
			&t.ClickBonusRate, &t.ClickBonusThreshold, &t.ClickBonusRateOver,
			&t.CreatorSalaryPerMonth, &t.CreatorRatePer1000Views,
			&t.CreatorRatePer1000ViewsOver,
			&t.StepViews, &t.FirstPeriodFee, &t.BaseFee, &t.StepFee,
			&t.StepTier2From, &t.StepFeeOver, &t.StepCapViews, &t.GuaranteeViews,
			&t.CreatorFirstPeriodFee, &t.CreatorBaseFee, &t.CreatorStepFee, &t.CreatorStepFeeOver,
			&t.SubscriberRate, &t.CreatorSubscriberRate,
			&t.FeePerVideo, &t.CreatorFeePerVideo,
			&t.ProjectCost,
			&t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, nil
	}
	if err != nil {
		return Terms{}, fmt.Errorf("load project billing: %w", err)
	}
	// Ступени — своим запросом: их произвольное число, и в строку
	// project_billing они не влезают по устройству.
	if t.Steps, err = loadSteps(ctx, r.db, stepsOwnerProject, projectID); err != nil {
		return Terms{}, err
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
   step_views, first_period_fee, base_fee, step_fee,
   step_tier2_from, step_fee_over, step_cap_views, guarantee_views,
   creator_first_period_fee, creator_base_fee, creator_step_fee, creator_step_fee_over,
   subscriber_rate, creator_subscriber_rate,
   fee_per_video, creator_fee_per_video, project_cost,
   updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
        $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27,
        $28, $29, $30, $31, $32,
        $15, now())
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
  step_views = EXCLUDED.step_views,
  first_period_fee = EXCLUDED.first_period_fee,
  base_fee = EXCLUDED.base_fee,
  step_fee = EXCLUDED.step_fee,
  step_tier2_from = EXCLUDED.step_tier2_from,
  step_fee_over = EXCLUDED.step_fee_over,
  step_cap_views = EXCLUDED.step_cap_views,
  guarantee_views = EXCLUDED.guarantee_views,
  creator_first_period_fee = EXCLUDED.creator_first_period_fee,
  creator_base_fee = EXCLUDED.creator_base_fee,
  creator_step_fee = EXCLUDED.creator_step_fee,
  creator_step_fee_over = EXCLUDED.creator_step_fee_over,
  fee_per_video = EXCLUDED.fee_per_video,
  creator_fee_per_video = EXCLUDED.creator_fee_per_video,
  subscriber_rate = EXCLUDED.subscriber_rate,
  creator_subscriber_rate = EXCLUDED.creator_subscriber_rate,
  project_cost = EXCLUDED.project_cost,
  updated_by = EXCLUDED.updated_by,
  updated_at = now()
RETURNING updated_at`,
		t.ProjectID, t.TermsVersionID, t.SalaryPerMonth,
		t.VideosFirstMonth, t.VideosNextMonths, t.RatePer1000Views,
		t.BonusViewsThreshold, t.RatePer1000ViewsOver,
		t.ClickBonusRate, t.ClickBonusThreshold, t.ClickBonusRateOver,
		t.CreatorSalaryPerMonth, t.CreatorRatePer1000Views, t.CreatorRatePer1000ViewsOver,
		actor,
		t.StepViews, t.FirstPeriodFee, t.BaseFee, t.StepFee,
		t.StepTier2From, t.StepFeeOver, t.StepCapViews, t.GuaranteeViews,
		t.CreatorFirstPeriodFee, t.CreatorBaseFee, t.CreatorStepFee, t.CreatorStepFeeOver,
		t.SubscriberRate, t.CreatorSubscriberRate,
		t.FeePerVideo, t.CreatorFeePerVideo, t.ProjectCost,
	).Scan(&t.UpdatedAt); err != nil {
		return Terms{}, fmt.Errorf("save project billing: %w", err)
	}
	// Снимок ступеней переписывается целиком: ступень удаляют не реже,
	// чем добавляют, и «обновить пришедшее» оставило бы удалённую жить в
	// расчёте проекта.
	if err := replaceSteps(ctx, r.db, stepsOwnerProject, t.ProjectID, t.Steps); err != nil {
		return Terms{}, err
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
       creator_salary_per_month, creator_rate_per_1000_views, creator_rate_per_1000_views_over,
       step_views, first_period_fee, base_fee, step_fee,
       step_tier2_from, step_fee_over, step_cap_views, guarantee_views,
       creator_first_period_fee, creator_base_fee, creator_step_fee, creator_step_fee_over,
       subscriber_rate, creator_subscriber_rate,
       fee_per_video, creator_fee_per_video
FROM terms_versions ORDER BY version DESC LIMIT 1`).
		Scan(&t.TermsVersionID, &t.SalaryPerMonth, &t.VideosFirstMonth, &t.VideosNextMonths,
			&t.RatePer1000Views, &t.BonusViewsThreshold, &t.RatePer1000ViewsOver,
			&t.ClickBonusRate, &t.ClickBonusThreshold, &t.ClickBonusRateOver,
			&t.CreatorSalaryPerMonth, &t.CreatorRatePer1000Views, &t.CreatorRatePer1000ViewsOver,
			&t.StepViews, &t.FirstPeriodFee, &t.BaseFee, &t.StepFee,
			&t.StepTier2From, &t.StepFeeOver, &t.StepCapViews, &t.GuaranteeViews,
			&t.CreatorFirstPeriodFee, &t.CreatorBaseFee, &t.CreatorStepFee, &t.CreatorStepFeeOver,
			&t.SubscriberRate, &t.CreatorSubscriberRate,
			&t.FeePerVideo, &t.CreatorFeePerVideo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Terms{}, ErrNotFound
	}
	if err != nil {
		return Terms{}, fmt.Errorf("load latest terms: %w", err)
	}
	// Ступени едут в снимок проекта вместе с остальными числами: иначе
	// проект снял бы с прайса всё, кроме того, по чему и считается.
	if t.TermsVersionID != nil {
		if t.Steps, err = loadSteps(ctx, r.db, stepsOwnerVersion, *t.TermsVersionID); err != nil {
			return Terms{}, err
		}
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

// CreatorCard — чем человек подписан в списке: имя, портрет и адрес его
// страницы. Втроём, а не по отдельности: подпись и ссылка на неё — одно
// решение экрана, и разнести их по двум запросам значит однажды показать
// имя без ссылки или ссылку без имени.
type CreatorCard struct {
	Name      string
	AvatarURL string
	Username  string
	// Public — открывается ли страница специалиста снаружи. Адрес у
	// человека есть всегда, а страницы по нему может не быть: публичная
	// карточка живёт только при is_published AND
	// moderation_status='approved'. Ссылка на непубликованный профиль
	// ведёт в 404, и решать это должен тот, кто про профиль знает, —
	// сервер, а не экран.
	Public bool
}

// profilePublicExpr — то же условие, что фильтрует публичную выдачу
// специалистов (см. profiles.Repo.GetPublic). Одним выражением: две
// копии однажды разойдутся, и ссылка начнёт врать ровно на тех
// профилях, из-за которых её и заводили.
const profilePublicExpr = `COALESCE(sp.is_published AND sp.moderation_status = 'approved', FALSE)`

// CreatorCards — подписи по списку id, той же лесенкой имени, что и везде.
//
// Нужны предварительному расчёту. Сохранённые начисления берут их тем же
// выражением прямо в запросе, а строки, посчитанные на лету, приходят из
// арифметики — в ней людей нет, только числа. На экране это выглядело как
// потерянные данные: строка есть, деньги есть, человека нет, «Без имени».
func (r *Repo) CreatorCards(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CreatorCard, error) {
	out := make(map[uuid.UUID]CreatorCard, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.db.Query(ctx, `
SELECT u.id, `+nameExpr+`, COALESCE(sp.avatar_url, ''), COALESCE(sp.username, ''),
       `+profilePublicExpr+`
FROM users u
LEFT JOIN specialist_profiles sp ON sp.user_id = u.id
LEFT JOIN client_profiles cp     ON cp.user_id = u.id
WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("creator names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var c CreatorCard
		if err := rows.Scan(&id, &c.Name, &c.AvatarURL, &c.Username, &c.Public); err != nil {
			return nil, fmt.Errorf("scan creator name: %w", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

func (r *Repo) Accruals(ctx context.Context, projectID uuid.UUID, periodStart *time.Time, creatorID *uuid.UUID) ([]Accrual, error) {
	rows, err := r.db.Query(ctx, `
SELECT a.id, a.project_id, a.creator_user_id, `+nameExpr+`,
       COALESCE(sp.avatar_url, ''), COALESCE(sp.username, ''),
       `+profilePublicExpr+`, a.period_start,
       a.salary, a.videos_planned, a.videos_delivered, a.deduction,
       a.views_total, a.views_base, a.views_over, a.views_bonus,
       a.clicks, a.click_bonus, a.subscribers, a.subscriber_bonus, a.total,
       a.payout_salary, a.payout_deduction, a.payout_views_bonus,
       a.payout_click_bonus, a.payout_subscriber_bonus, a.payout_total, a.status::text,
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
			&a.CreatorAvatarURL, &a.CreatorUsername, &a.CreatorProfilePublic,
			&a.PeriodStart, &a.Salary, &a.VideosPlanned, &a.VideosDelivered,
			&a.Deduction, &a.ViewsTotal, &a.ViewsBase, &a.ViewsOver, &a.ViewsBonus,
			&a.Clicks, &a.ClickBonus, &a.Subscribers, &a.SubscriberBonus, &a.Total,
			&a.PayoutSalary, &a.PayoutDeduction, &a.PayoutViewsBonus,
			&a.PayoutClickBonus, &a.PayoutSubscriberBonus, &a.PayoutTotal,
			&a.Status, &a.Priority,
			&a.ApprovedAt, &a.PaidAt, &a.CalculatedAt); err != nil {
			return nil, fmt.Errorf("scan accrual: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// creatorPeriod — сырые числа по одному креатору за месяц.
type creatorPeriod struct {
	CreatorID uuid.UUID
	// Planned/Delivered — сколько роликов вышло в периоде и сколько из
	// них сдано полностью. Это показатель работы: в него входят и те,
	// что креатор добавил себе сам.
	Planned   int
	Delivered int
	// PlannedAssigned/DeliveredAssigned — то же самое, но только по
	// выкладкам, которые ПОСТАВИЛ МЕНЕДЖЕР. Недосдача считается по ним и
	// только по ним.
	//
	// Иначе кнопка «добавить себе ролик» работала бы против того, кто её
	// нажал: добавил пять, сдал два — и потерял часть оклада за три,
	// которых ему никто не поручал. Человек своими руками сделал бы себе
	// хуже. Просмотры самодобавленных при этом идут в счёт наравне:
	// просмотры есть просмотры.
	PlannedAssigned   int
	DeliveredAssigned int
	ViewsTotal        int64
	// ViewsBase/ViewsOver — просмотры до порога и сверх него, посчитанные
	// ПО КАЖДОМУ РОЛИКУ отдельно и потом сложенные. Считать по сумме за
	// месяц нельзя: порог стоит на ролике, и два ролика по 600 000 — это
	// ноль сверхпорогового объёма, а не 200 000.
	ViewsBase int64
	ViewsOver int64
	// Clicks — переходы по метке ЗА ЭТОТ ПЕРИОД: счётчик метки
	// накопительный, и из него вычтено всё, что уже зачли прошлые
	// периоды. Иначе одни и те же переходы оплачивались бы заново
	// каждые тридцать дней.
	Clicks int
	// Subscribers — сколько подписчиков прибавилось за период. Вводит
	// менеджер руками: сборщика подписчиков у нас нет, и выдумывать его
	// под объявленную ставку нельзя — ровно как с переходами по UTM.
	Subscribers int64
}

// periodFacts — что креаторы проекта наработали за месяц.
//
// Просмотры берутся по последнему снимку каждой ссылки и складываются в
// ролик целиком: порог «миллион» стоит на ролике, а не на площадке.
func (r *Repo) periodFacts(ctx context.Context, projectID uuid.UUID, p ProjectPeriod, threshold int64) ([]creatorPeriod, error) {
	rows, err := r.db.Query(ctx, `
WITH pub AS (
    SELECT f.id, f.creator_user_id, f.status, f.self_added,
           COALESCE(SUM(cur.views), 0) AS views
    FROM (`+publishedInPeriodSQL+`) f
    JOIN publication_links l ON l.publication_id = f.id
    LEFT JOIN LATERAL (
        SELECT views FROM video_stat_daily d
        WHERE d.link_id = l.id ORDER BY d.stat_date DESC LIMIT 1
    ) cur ON TRUE
    GROUP BY f.id, f.creator_user_id, f.status, f.self_added
)
SELECT pc.creator_user_id,
       COUNT(pub.id),
       COUNT(pub.id) FILTER (WHERE pub.status IN ('done', 'closed_manually')),
       -- Знаменатель недосдачи — только выкладки менеджера. См.
       -- creatorPeriod.PlannedAssigned: самодобавленные сюда не идут.
       COUNT(pub.id) FILTER (WHERE NOT pub.self_added),
       COUNT(pub.id) FILTER (WHERE NOT pub.self_added
                               AND pub.status IN ('done', 'closed_manually')),
       COALESCE(SUM(pub.views), 0),
       -- Ступени считаются по каждому ролику и складываются.
       --
       -- COALESCE ВНУТРИ обязателен: у креатора без выкладок LEFT JOIN
       -- даёт строку с NULL, а LEAST(NULL, порог) в PostgreSQL — это не
       -- NULL, а порог: NULL'ы он молча пропускает. Человек без единого
       -- ролика получал бонус за миллион просмотров.
       COALESCE(SUM(LEAST(COALESCE(pub.views, 0), $5)), 0),
       COALESCE(SUM(GREATEST(COALESCE(pub.views, 0) - $5, 0)), 0),
       GREATEST(COALESCE(MAX(utm.clicks), 0) - COALESCE(MAX(prevclicks.counted), 0), 0),
       COALESCE(MAX(subs.subscribers), 0)
FROM project_creators pc
LEFT JOIN pub ON pub.creator_user_id = pc.creator_user_id
LEFT JOIN creator_utm_links utm
       ON utm.project_id = pc.project_id AND utm.creator_user_id = pc.creator_user_id
LEFT JOIN LATERAL (
    -- Переходы по метке — счётчик НАКОПИТЕЛЬНЫЙ: в creator_utm_links
    -- лежит «всего кликов по ссылке», а не «кликов за период». Считать
    -- его целиком каждый период значило бы выставлять одни и те же
    -- переходы заново каждые тридцать дней. Отнимаем то, что уже
    -- посчитано прошлыми периодами — строки начислений и есть история
    -- зачтённого.
    SELECT COALESCE(SUM(ca.clicks), 0) AS counted
    FROM creator_accruals ca
    WHERE ca.project_id = pc.project_id
      AND ca.creator_user_id = pc.creator_user_id
      AND ca.period_start < $6
) prevclicks ON TRUE
LEFT JOIN creator_period_subscribers subs
       ON subs.project_id = pc.project_id AND subs.creator_user_id = pc.creator_user_id
      AND subs.period_start = $6
WHERE pc.project_id = $1 AND pc.removed_at IS NULL
-- $3 — левая граница отбора выкладок: у первого периода её нет (см.
-- periodFrom). $6 — начало периода как таковое: подписчиков менеджер
-- вписывает ИМЕННО периоду, и им открытая граница не нужна.
GROUP BY pc.creator_user_id`, projectID, projectID, periodFrom(p), p.EndsOn, threshold, p.StartsOn)
	if err != nil {
		return nil, fmt.Errorf("period facts: %w", err)
	}
	defer rows.Close()
	out := make([]creatorPeriod, 0)
	for rows.Next() {
		var c creatorPeriod
		if err := rows.Scan(&c.CreatorID, &c.Planned, &c.Delivered,
			&c.PlannedAssigned, &c.DeliveredAssigned,
			&c.ViewsTotal, &c.ViewsBase, &c.ViewsOver, &c.Clicks, &c.Subscribers); err != nil {
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
    SELECT sp.publication_id AS id, sp.creator_user_id, sp.status, sp.self_added,
           COALESCE(SUM(sv.views), 0) AS views
    FROM project_period_publications sp
    LEFT JOIN project_period_views sv
           ON sv.period_id = sp.period_id
          AND sv.publication_id = sp.publication_id
    WHERE sp.period_id = $2
    GROUP BY sp.publication_id, sp.creator_user_id, sp.status, sp.self_added
)
SELECT pc.creator_user_id,
       COUNT(pub.id),
       COUNT(pub.id) FILTER (WHERE pub.status IN ('done', 'closed_manually')),
       COUNT(pub.id) FILTER (WHERE NOT pub.self_added),
       COUNT(pub.id) FILTER (WHERE NOT pub.self_added
                               AND pub.status IN ('done', 'closed_manually')),
       COALESCE(SUM(pub.views), 0),
       COALESCE(SUM(LEAST(COALESCE(pub.views, 0), $3)), 0),
       COALESCE(SUM(GREATEST(COALESCE(pub.views, 0) - $3, 0)), 0),
       GREATEST(COALESCE(MAX(utm.clicks), 0) - COALESCE(MAX(prevclicks.counted), 0), 0),
       COALESCE(MAX(subs.subscribers), 0)
FROM project_creators pc
LEFT JOIN pub ON pub.creator_user_id = pc.creator_user_id
LEFT JOIN creator_utm_links utm
       ON utm.project_id = pc.project_id AND utm.creator_user_id = pc.creator_user_id
LEFT JOIN LATERAL (
    -- Переходы по метке — счётчик НАКОПИТЕЛЬНЫЙ: в creator_utm_links
    -- лежит «всего кликов по ссылке», а не «кликов за период». Считать
    -- его целиком каждый период значило бы выставлять одни и те же
    -- переходы заново каждые тридцать дней. Отнимаем то, что уже
    -- посчитано прошлыми периодами — строки начислений и есть история
    -- зачтённого.
    SELECT COALESCE(SUM(ca.clicks), 0) AS counted
    FROM creator_accruals ca
    WHERE ca.project_id = pc.project_id
      AND ca.creator_user_id = pc.creator_user_id
      AND ca.period_start < $4
) prevclicks ON TRUE
LEFT JOIN creator_period_subscribers subs
       ON subs.project_id = pc.project_id AND subs.creator_user_id = pc.creator_user_id
      AND subs.period_start = $4
WHERE pc.project_id = $1 AND pc.removed_at IS NULL
GROUP BY pc.creator_user_id`, projectID, p.ID, threshold, p.StartsOn)
	if err != nil {
		return nil, fmt.Errorf("locked period facts: %w", err)
	}
	defer rows.Close()
	out := make([]creatorPeriod, 0)
	for rows.Next() {
		var c creatorPeriod
		if err := rows.Scan(&c.CreatorID, &c.Planned, &c.Delivered,
			&c.PlannedAssigned, &c.DeliveredAssigned,
			&c.ViewsTotal, &c.ViewsBase, &c.ViewsOver, &c.Clicks, &c.Subscribers); err != nil {
			return nil, fmt.Errorf("scan locked period facts: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveAccrual — записать пересчитанное. Утверждённое и выплаченное не
// трогаем: цифра, по которой уже перевели деньги, задним числом не меняется.
// SaveAccrual — записать строку начисления.
//
// fromSnapshot говорит, откуда взяты числа: из среза подытоженного
// периода или из живых просмотров. Это не справка для логов, а условие
// записи.
//
// Подытог и сбор статистики — два разных тикера одного воркера, и оба
// стартуют сразу при запуске процесса. Пересчёт после сбора читает
// период открытым, считает по живым просмотрам и пишет; между чтением и
// записью подытог успевает закоммититься — и живые числа затирают уже
// замороженные. Начисления только что подытоженного периода ещё
// `draft`, так что проверка статуса самой строки от этого не спасает.
//
// Поэтому живые числа не записываются, если период к этому моменту стал
// подытоженным. Числа из среза записываются всегда: их и пишет подытог.
func (r *Repo) SaveAccrual(ctx context.Context, a Accrual, fromSnapshot bool) error {
	tag, err := r.db.Exec(ctx, `
INSERT INTO creator_accruals
  (project_id, creator_user_id, period_start, salary, videos_planned,
   videos_delivered, deduction, views_total, views_base, views_over, views_bonus,
   clicks, click_bonus, subscribers, subscriber_bonus, total,
   payout_salary, payout_deduction, payout_views_bonus, payout_click_bonus,
   payout_subscriber_bonus, payout_total,
   calculated_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22, now()
-- Та же защита, что и в DO UPDATE ниже: живыми числами в подытоженный
-- период не пишем. Без неё строка креатора, добавленного в состав уже
-- ПОСЛЕ подытога, ложилась бы в замороженный период по сегодняшним
-- просмотрам — вставке-то переписывать нечего, и условие UPDATE её не
-- касается.
WHERE $23 OR NOT EXISTS (
    SELECT 1 FROM project_periods pp
    WHERE pp.project_id = $1 AND pp.starts_on = $3 AND pp.status = 'locked')
ON CONFLICT (project_id, creator_user_id, period_start) DO UPDATE SET
  salary = EXCLUDED.salary, videos_planned = EXCLUDED.videos_planned,
  videos_delivered = EXCLUDED.videos_delivered, deduction = EXCLUDED.deduction,
  views_total = EXCLUDED.views_total, views_base = EXCLUDED.views_base,
  views_over = EXCLUDED.views_over,
  views_bonus = EXCLUDED.views_bonus, clicks = EXCLUDED.clicks,
  click_bonus = EXCLUDED.click_bonus,
  subscribers = EXCLUDED.subscribers, subscriber_bonus = EXCLUDED.subscriber_bonus,
  total = EXCLUDED.total,
  payout_salary = EXCLUDED.payout_salary,
  payout_deduction = EXCLUDED.payout_deduction,
  payout_views_bonus = EXCLUDED.payout_views_bonus,
  payout_click_bonus = EXCLUDED.payout_click_bonus,
  payout_subscriber_bonus = EXCLUDED.payout_subscriber_bonus,
  payout_total = EXCLUDED.payout_total,
  calculated_at = now()
WHERE creator_accruals.status = 'draft'
  AND ($23 OR NOT EXISTS (
      SELECT 1 FROM project_periods pp
      WHERE pp.project_id = creator_accruals.project_id
        AND pp.starts_on = creator_accruals.period_start
        AND pp.status = 'locked'))`,
		a.ProjectID, a.CreatorUserID, a.PeriodStart, a.Salary, a.VideosPlanned,
		a.VideosDelivered, a.Deduction, a.ViewsTotal, a.ViewsBase, a.ViewsOver,
		a.ViewsBonus, a.Clicks, a.ClickBonus, a.Subscribers, a.SubscriberBonus, a.Total,
		a.PayoutSalary, a.PayoutDeduction, a.PayoutViewsBonus,
		a.PayoutClickBonus, a.PayoutSubscriberBonus, a.PayoutTotal, fromSnapshot)
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

// ---- ступени тарифа ----
//
// Ступени лежат строками, а не колонками: их число произвольно, и каждая
// новая точка прайса колонками означала бы миграцию. Владелец у строки
// один из двух — версия прайса или снимок проекта, — и поэтому запросы
// параметризованы именем колонки владельца. Имя приходит из констант
// ниже, а не снаружи: подстановка в SQL здесь безопасна ровно потому,
// что подставлять чужое некуда.

const (
	stepsOwnerVersion = "terms_version_id"
	stepsOwnerProject = "project_id"
)

// querier — то общее, что нужно ступеням от пула и от транзакции:
// выпуск версии и снимок условий обязаны класть ступени в ту же
// транзакцию, что и саму версию.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// StepKindViews/StepKindSubscribers — чья это лесенка.
//
// Таблица одна на оба вида: правило «ступень — это порог и две цены»
// общее, и второй такой же таблицей оно разъехалось бы на первой же
// правке. Но запросы обязаны вид УКАЗЫВАТЬ: ступень подписчиков,
// прочитанная как ступень просмотров, молча станет ценой периода.
const (
	StepKindViews       = "views"
	StepKindSubscribers = "subscribers"
)

func loadSteps(ctx context.Context, q querier, owner string, id uuid.UUID) ([]TermsStep, error) {
	return loadStepsOf(ctx, q, owner, id, StepKindViews)
}

func loadStepsOf(
	ctx context.Context, q querier, owner string, id uuid.UUID, kind string,
) ([]TermsStep, error) {
	rows, err := q.Query(ctx,
		`SELECT from_views, client_fee, creator_fee FROM terms_steps
WHERE `+owner+` = $1 AND kind = $2 ORDER BY from_views`, id, kind)
	if err != nil {
		return nil, fmt.Errorf("load terms steps: %w", err)
	}
	defer rows.Close()
	out := make([]TermsStep, 0, 4)
	for rows.Next() {
		var s TermsStep
		if err := rows.Scan(&s.FromViews, &s.ClientFee, &s.CreatorFee); err != nil {
			return nil, fmt.Errorf("scan terms step: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// replaceSteps — переписать лесенку владельца целиком.
//
// Целиком, а не по одной строке: ступень удаляют не реже, чем добавляют,
// и «обнови то, что пришло» оставило бы удалённую ступень жить в
// расчёте. Для версии прайса это вставка в свежесозданную строку, для
// снимка проекта — замена прежнего снимка.
func replaceSteps(ctx context.Context, q querier, owner string, id uuid.UUID, steps []TermsStep) error {
	return replaceStepsOf(ctx, q, owner, id, StepKindViews, steps)
}

// replaceStepsOf — переписать лесенку ОДНОГО вида, не трогая соседний.
//
// Чужой вид не трогаем ни на удалении, ни на вставке: сохранение
// лесенки просмотров иначе стирало бы лесенку подписчиков целиком.
func replaceStepsOf(
	ctx context.Context, q querier, owner string, id uuid.UUID, kind string, steps []TermsStep,
) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM terms_steps WHERE `+owner+` = $1 AND kind = $2`, id, kind); err != nil {
		return fmt.Errorf("clear terms steps: %w", err)
	}
	for _, s := range steps {
		if _, err := q.Exec(ctx,
			`INSERT INTO terms_steps (`+owner+`, kind, from_views, client_fee, creator_fee)
VALUES ($1, $2, $3, $4, $5)`, id, kind, s.FromViews, s.ClientFee, s.CreatorFee); err != nil {
			return fmt.Errorf("insert terms step: %w", err)
		}
	}
	return nil
}

// ---- подписчики ----
//
// Подписчиков никто не собирает: сборщика по ним нет, и выдумывать его
// под объявленную ставку нельзя. Число вписывает менеджер — ровно так же,
// как заведены переходы по UTM. Держим по периоду: KPI считается за
// период, и одно поле «сколько всего» пришлось бы каждый месяц
// перезаписывать, теряя то, за что уже заплатили.

// PeriodSubscribers — сколько подписчиков записано креатору за период.
func (r *Repo) PeriodSubscribers(
	ctx context.Context, projectID uuid.UUID, periodStart time.Time,
) ([]CreatorSubscribers, error) {
	rows, err := r.db.Query(ctx, `
SELECT s.creator_user_id, `+nameExpr+`, s.subscribers, s.updated_at
FROM creator_period_subscribers s
LEFT JOIN users u                ON u.id = s.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = s.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = s.creator_user_id
WHERE s.project_id = $1 AND s.period_start = $2
ORDER BY 2`, projectID, periodStart)
	if err != nil {
		return nil, fmt.Errorf("list period subscribers: %w", err)
	}
	defer rows.Close()
	out := make([]CreatorSubscribers, 0)
	for rows.Next() {
		var c CreatorSubscribers
		if err := rows.Scan(&c.CreatorUserID, &c.CreatorName, &c.Subscribers, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan period subscribers: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveSubscribers — записать число подписчиков за период.
func (r *Repo) SaveSubscribers(
	ctx context.Context, projectID, creatorID uuid.UUID, periodStart time.Time, n int64, actor uuid.UUID,
) (CreatorSubscribers, error) {
	c := CreatorSubscribers{CreatorUserID: creatorID, PeriodStart: periodStart}
	if err := r.db.QueryRow(ctx, `
INSERT INTO creator_period_subscribers
  (project_id, creator_user_id, period_start, subscribers, updated_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, creator_user_id, period_start) DO UPDATE
  SET subscribers = EXCLUDED.subscribers, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING subscribers, updated_at`, projectID, creatorID, periodStart, n, actor).
		Scan(&c.Subscribers, &c.UpdatedAt); err != nil {
		return CreatorSubscribers{}, fmt.Errorf("save period subscribers: %w", err)
	}
	return c, nil
}
