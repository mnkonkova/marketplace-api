package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
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
	// Margin — что из уплаченного клиентом остаётся площадке. Считается,
	// а не хранится: это разность двух сторон тарифа, и отдельная колонка
	// разошлась бы с ними на первой же правке ставок.
	Margin TermsMargin `json:"margin"`
}

// TermsMargin — разница между ценой клиента и выплатой креатору.
//
// В списке версий это главный вопрос к прайсу: две колонки ставок рядом
// админ вычитает в уме на каждой строке, и ошибается там, где выплата
// заполнена не во всех трёх полях.
type TermsMargin struct {
	// SalaryPerMonth — маржа на окладе за месяц, копейки.
	SalaryPerMonth int64 `json:"salary_per_month"`
	// RatePer1000Views/RatePer1000ViewsOver — маржа на ставке за тысячу
	// просмотров до порога и сверх него, копейки.
	RatePer1000Views     int64 `json:"rate_per_1000_views"`
	RatePer1000ViewsOver int64 `json:"rate_per_1000_views_over"`
	// HasMargin — креаторская сторона заполнена хоть где-то. Пустая
	// означает «платим креатору ровно то, что берём», то есть нули ниже —
	// настоящие нули, а не «не задано».
	HasMargin bool `json:"has_margin"`
}

// PlatformMargin — маржа площадки по этому тарифу.
func (t Terms) PlatformMargin() TermsMargin {
	c := t.CreatorSide()
	return TermsMargin{
		SalaryPerMonth:       t.SalaryPerMonth - c.SalaryPerMonth,
		RatePer1000Views:     t.RatePer1000Views - c.RatePer1000Views,
		RatePer1000ViewsOver: t.RatePer1000ViewsOver - c.RatePer1000ViewsOver,
		HasMargin:            t.HasMargin(),
	}
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
       v.step_views, v.first_period_fee, v.base_fee, v.step_fee,
       v.step_tier2_from, v.step_fee_over, v.step_cap_views, v.guarantee_views,
       v.creator_first_period_fee, v.creator_base_fee, v.creator_step_fee,
       v.creator_step_fee_over,
       v.subscriber_rate, v.creator_subscriber_rate,
       v.fee_per_video, v.creator_fee_per_video,
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
			&v.StepViews, &v.FirstPeriodFee, &v.BaseFee, &v.StepFee,
			&v.StepTier2From, &v.StepFeeOver, &v.StepCapViews, &v.GuaranteeViews,
			&v.CreatorFirstPeriodFee, &v.CreatorBaseFee, &v.CreatorStepFee,
			&v.CreatorStepFeeOver,
			&v.SubscriberRate, &v.CreatorSubscriberRate,
			&v.FeePerVideo, &v.CreatorFeePerVideo,
			&v.IsCurrent, &v.ConsentedClients, &v.UsedByProjects); err != nil {
			return nil, fmt.Errorf("scan terms version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Ступени — вторым проходом: курсор первого запроса ещё открыт, и
	// вложенный запрос по тому же соединению его бы и занял.
	for i := range out {
		if err := loadLadders(
			ctx, r.db, stepsOwnerVersion, *out[i].TermsVersionID, &out[i].Terms,
		); err != nil {
			return nil, err
		}
		out[i].Margin = out[i].Terms.PlatformMargin()
	}
	return out, nil
}

// TermsChange — одно изменившееся число прайса.
type TermsChange struct {
	// Field — имя поля тарифа (то же, что в JSON условий).
	Field string `json:"field"`
	// Label — человеческое название для интерфейса: админ читает
	// «Ставка за 1000 просмотров», а не rate_per_1000_views.
	Label string `json:"label"`
	// From/To — прежнее и новое значение. Пусто (nil) = «не задано»:
	// креаторские ставки бывают незаполненными, и 0 от «как у клиента»
	// одним числом не отличить.
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
}

// TermsPublishResult — выпущенная версия плюс последствия выпуска.
//
// Выпуск версии — необратимое действие: прежнюю мы не переписываем, а
// клиентское согласие с этого момента протухло. Поэтому ответ показывает
// не только новую версию, но и что именно изменилось и скольких клиентов
// придётся спросить заново.
type TermsPublishResult struct {
	TermsVersion
	// Changes — разница с прежней действующей версией. Пусто у самой
	// первой версии: сравнивать не с чем.
	Changes []TermsChange `json:"changes"`
	// ConsentsRequired — сколько клиентов согласились с прежней
	// действующей версией и теперь должны согласиться заново.
	ConsentsRequired int `json:"consents_required"`
	// ProjectsRefreshed — сколько ещё не начавшихся проектов переехало
	// на эту версию. Начавшиеся не трогаются: у них снимок.
	ProjectsRefreshed int `json:"projects_refreshed"`
}

// PublishTermsVersion — выпустить новую версию прайса.
//
// Прежняя действующая версия читается в той же транзакции: между чтением
// «с чем сравнивать» и вставкой мог бы встрять второй выпуск, и разница
// в ответе считалась бы от версии, которая уже не действует.
func (r *Repo) PublishTermsVersion(ctx context.Context, v TermsVersion, actorID uuid.UUID) (TermsPublishResult, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return TermsPublishResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prev, havePrev, err := currentTermsVersionTx(ctx, tx)
	if err != nil {
		return TermsPublishResult{}, err
	}

	err = tx.QueryRow(ctx, `
INSERT INTO terms_versions
  (version, body, salary_per_month, videos_first_month, videos_next_months,
   rate_per_1000_views, bonus_views_threshold, rate_per_1000_views_over,
   click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
   creator_salary_per_month, creator_rate_per_1000_views, creator_rate_per_1000_views_over,
   step_views, first_period_fee, base_fee, step_fee,
   step_tier2_from, step_fee_over, step_cap_views, guarantee_views,
   creator_first_period_fee, creator_base_fee, creator_step_fee, creator_step_fee_over,
   subscriber_rate, creator_subscriber_rate,
   fee_per_video, creator_fee_per_video)
VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM terms_versions),
        $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
        $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27,
        $28, $29)
RETURNING id, version, published_at`,
		v.Body, v.SalaryPerMonth, v.VideosFirstMonth, v.VideosNextMonths,
		v.RatePer1000Views, v.BonusViewsThreshold, v.RatePer1000ViewsOver,
		v.ClickBonusRate, v.ClickBonusThreshold, v.ClickBonusRateOver,
		v.CreatorSalaryPerMonth, v.CreatorRatePer1000Views, v.CreatorRatePer1000ViewsOver,
		v.StepViews, v.FirstPeriodFee, v.BaseFee, v.StepFee,
		v.StepTier2From, v.StepFeeOver, v.StepCapViews, v.GuaranteeViews,
		v.CreatorFirstPeriodFee, v.CreatorBaseFee, v.CreatorStepFee, v.CreatorStepFeeOver,
		v.SubscriberRate, v.CreatorSubscriberRate,
		v.FeePerVideo, v.CreatorFeePerVideo).
		Scan(&v.TermsVersionID, &v.Version, &v.PublishedAt)
	if err != nil {
		return TermsPublishResult{}, fmt.Errorf("publish terms version: %w", err)
	}
	// Ступени — в той же транзакции, что и сама версия: версия без своих
	// ступеней посчиталась бы по прежней модели, и это увидели бы не
	// сразу, а на первом счёте.
	if err := replaceLadders(ctx, tx, stepsOwnerVersion, *v.TermsVersionID, v.Terms); err != nil {
		return TermsPublishResult{}, err
	}
	v.IsCurrent = true
	v.Margin = v.Terms.PlatformMargin()

	out := TermsPublishResult{TermsVersion: v, Changes: []TermsChange{}}
	if havePrev {
		out.Changes = diffTerms(prev.Terms, v.Terms)
		// Согласие даётся на конкретную версию. Заново спрашивать нужно
		// тех, кто согласился с прежней действующей: они единственные,
		// у кого согласие только что перестало быть актуальным.
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(DISTINCT user_id) FROM client_terms_consents WHERE terms_version_id = $1`,
			prev.TermsVersionID).Scan(&out.ConsentsRequired); err != nil {
			return TermsPublishResult{}, fmt.Errorf("count consents: %w", err)
		}
	}

	// Проекты, которые ещё не начались, переезжают на новую версию.
	//
	// Условия проекта — снимок, и это правильно: подытоженный период
	// нельзя пересчитать задним числом. Но до первой выкладки считать
	// нечего, а прайс к этому моменту уже мог смениться — проект,
	// заведённый в понедельник и стартующий через месяц, не должен
	// уехать в работу по позавчерашним ставкам.
	//
	// Граница — первая выкладка, то есть первая сданная ссылка: от неё
	// же отсчитываются периоды. С этого момента снимок замораживается.
	refreshed, err := refreshNotStartedProjects(ctx, tx, *v.TermsVersionID, v.Terms)
	if err != nil {
		return TermsPublishResult{}, err
	}
	out.ProjectsRefreshed = refreshed

	if err := audit.Write(ctx, tx, actorID, audit.ActionTermsPublish,
		audit.ObjectTerms, v.TermsVersionID.String(), map[string]any{
			"version":            v.Version,
			"changes":            len(out.Changes),
			"consents_required":  out.ConsentsRequired,
			"projects_refreshed": out.ProjectsRefreshed,
		}); err != nil {
		return TermsPublishResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TermsPublishResult{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// refreshNotStartedProjects — перевести не начавшиеся проекты на версию.
//
// Не начавшийся — тот, у которого нет ни одной сданной ссылки: периоды
// отсчитываются от первой выкладки, и пока её нет, считать нечего.
// Отменённые проекты и те, у кого уже есть подытоженный период, не
// трогаем ни при каких условиях — второе невозможно без первой выкладки,
// но проверка стоит дёшево, а цена ошибки здесь — переписанный счёт.
//
// Возвращает, сколько проектов переехало: это число видит админ в ответе
// на выпуск версии и оно же уходит в журнал.
func refreshNotStartedProjects(
	ctx context.Context, tx pgx.Tx, versionID uuid.UUID, t Terms,
) (int, error) {
	rows, err := tx.Query(ctx, `
SELECT p.id
FROM projects p
WHERE p.status <> 'cancelled'
  AND NOT EXISTS (
      SELECT 1 FROM project_publications pub
      JOIN publication_links l ON l.publication_id = pub.id
      WHERE pub.project_id = p.id AND pub.status <> 'cancelled')
  AND NOT EXISTS (
      SELECT 1 FROM project_periods pp
      WHERE pp.project_id = p.id AND pp.status = 'locked')`)
	if err != nil {
		return 0, fmt.Errorf("list not started projects: %w", err)
	}
	ids := make([]uuid.UUID, 0, 16)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan project id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// Числа переносим одним UPDATE из самой версии: перечислять их
	// значениями значило бы держать третий список колонок рядом с двумя
	// уже существующими, и он разошёлся бы с ними на первой же правке
	// тарифа.
	if _, err := tx.Exec(ctx, `
UPDATE project_billing b
SET terms_version_id = v.id,
    salary_per_month = v.salary_per_month,
    videos_first_month = v.videos_first_month,
    videos_next_months = v.videos_next_months,
    rate_per_1000_views = v.rate_per_1000_views,
    bonus_views_threshold = v.bonus_views_threshold,
    rate_per_1000_views_over = v.rate_per_1000_views_over,
    click_bonus_rate = v.click_bonus_rate,
    click_bonus_threshold = v.click_bonus_threshold,
    click_bonus_rate_over = v.click_bonus_rate_over,
    creator_salary_per_month = v.creator_salary_per_month,
    creator_rate_per_1000_views = v.creator_rate_per_1000_views,
    creator_rate_per_1000_views_over = v.creator_rate_per_1000_views_over,
    step_views = v.step_views,
    first_period_fee = v.first_period_fee,
    base_fee = v.base_fee,
    step_fee = v.step_fee,
    step_tier2_from = v.step_tier2_from,
    step_fee_over = v.step_fee_over,
    step_cap_views = v.step_cap_views,
    guarantee_views = v.guarantee_views,
    creator_first_period_fee = v.creator_first_period_fee,
    creator_base_fee = v.creator_base_fee,
    creator_step_fee = v.creator_step_fee,
    creator_step_fee_over = v.creator_step_fee_over,
    subscriber_rate = v.subscriber_rate,
    creator_subscriber_rate = v.creator_subscriber_rate,
    fee_per_video = v.fee_per_video,
    creator_fee_per_video = v.creator_fee_per_video,
    updated_at = now()
FROM terms_versions v
WHERE v.id = $1 AND b.project_id = ANY($2)`, versionID, ids); err != nil {
		return 0, fmt.Errorf("refresh project billing: %w", err)
	}

	// Ступени лежат своей таблицей и одним UPDATE не переносятся — обе
	// лесенки, и просмотров, и подписчиков.
	for _, id := range ids {
		if err := replaceLadders(ctx, tx, stepsOwnerProject, id, t); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// currentTermsVersionTx — действующая версия (самая новая) внутри
// транзакции. have=false означает «прайса ещё не было».
func currentTermsVersionTx(ctx context.Context, tx pgx.Tx) (TermsVersion, bool, error) {
	var v TermsVersion
	err := tx.QueryRow(ctx, `
SELECT id, version, body, published_at,
       salary_per_month, videos_first_month, videos_next_months,
       rate_per_1000_views, bonus_views_threshold, rate_per_1000_views_over,
       click_bonus_rate, click_bonus_threshold, click_bonus_rate_over,
       creator_salary_per_month, creator_rate_per_1000_views, creator_rate_per_1000_views_over,
       step_views, first_period_fee, base_fee, step_fee,
       step_tier2_from, step_fee_over, step_cap_views, guarantee_views,
       creator_first_period_fee, creator_base_fee, creator_step_fee, creator_step_fee_over,
       subscriber_rate, creator_subscriber_rate,
       fee_per_video, creator_fee_per_video
FROM terms_versions
ORDER BY version DESC
LIMIT 1`).Scan(&v.TermsVersionID, &v.Version, &v.Body, &v.PublishedAt,
		&v.SalaryPerMonth, &v.VideosFirstMonth, &v.VideosNextMonths,
		&v.RatePer1000Views, &v.BonusViewsThreshold, &v.RatePer1000ViewsOver,
		&v.ClickBonusRate, &v.ClickBonusThreshold, &v.ClickBonusRateOver,
		&v.CreatorSalaryPerMonth, &v.CreatorRatePer1000Views, &v.CreatorRatePer1000ViewsOver,
		&v.StepViews, &v.FirstPeriodFee, &v.BaseFee, &v.StepFee,
		&v.StepTier2From, &v.StepFeeOver, &v.StepCapViews, &v.GuaranteeViews,
		&v.CreatorFirstPeriodFee, &v.CreatorBaseFee, &v.CreatorStepFee, &v.CreatorStepFeeOver,
		&v.SubscriberRate, &v.CreatorSubscriberRate,
		&v.FeePerVideo, &v.CreatorFeePerVideo)
	if errors.Is(err, pgx.ErrNoRows) {
		return TermsVersion{}, false, nil
	}
	if err != nil {
		return TermsVersion{}, false, fmt.Errorf("current terms version: %w", err)
	}
	if err := loadLadders(ctx, tx, stepsOwnerVersion, *v.TermsVersionID, &v.Terms); err != nil {
		return TermsVersion{}, false, err
	}
	return v, true, nil
}

// diffTerms — изменившиеся числа между двумя тарифами, в порядке полей
// формы: так админ читает разницу там же, где только что вводил.
func diffTerms(from, to Terms) []TermsChange {
	out := []TermsChange{}
	add := func(field, label string, a, b *int64) {
		if a == nil && b == nil {
			return
		}
		if a != nil && b != nil && *a == *b {
			return
		}
		out = append(out, TermsChange{Field: field, Label: label, From: a, To: b})
	}
	val := func(v int64) *int64 { return &v }
	add("salary_per_month", "Оклад за месяц", val(from.SalaryPerMonth), val(to.SalaryPerMonth))
	add("videos_first_month", "Роликов в первый месяц",
		val(int64(from.VideosFirstMonth)), val(int64(to.VideosFirstMonth)))
	add("videos_next_months", "Роликов со второго месяца",
		val(int64(from.VideosNextMonths)), val(int64(to.VideosNextMonths)))
	add("rate_per_1000_views", "Ставка за 1000 просмотров",
		val(from.RatePer1000Views), val(to.RatePer1000Views))
	add("bonus_views_threshold", "Порог просмотров на ролик",
		val(from.BonusViewsThreshold), val(to.BonusViewsThreshold))
	add("rate_per_1000_views_over", "Ставка за 1000 просмотров сверх порога",
		val(from.RatePer1000ViewsOver), val(to.RatePer1000ViewsOver))
	add("click_bonus_rate", "Ставка за переход", from.ClickBonusRate, to.ClickBonusRate)
	add("click_bonus_threshold", "Порог переходов за месяц",
		val(int64(from.ClickBonusThreshold)), val(int64(to.ClickBonusThreshold)))
	add("click_bonus_rate_over", "Ставка за переход сверх порога",
		from.ClickBonusRateOver, to.ClickBonusRateOver)
	add("creator_salary_per_month", "Оклад креатора за месяц",
		from.CreatorSalaryPerMonth, to.CreatorSalaryPerMonth)
	add("creator_rate_per_1000_views", "Креатору за 1000 просмотров",
		from.CreatorRatePer1000Views, to.CreatorRatePer1000Views)
	add("creator_rate_per_1000_views_over", "Креатору за 1000 просмотров сверх порога",
		from.CreatorRatePer1000ViewsOver, to.CreatorRatePer1000ViewsOver)

	// Ступенчатый тариф.
	add("step_views", "Размер ступени, просмотров", from.StepViews, to.StepViews)
	add("first_period_fee", "Фикс за первый период", from.FirstPeriodFee, to.FirstPeriodFee)
	add("base_fee", "Фикс со второго периода", from.BaseFee, to.BaseFee)
	add("step_fee", "Цена ступени", from.StepFee, to.StepFee)
	add("step_tier2_from", "Ступень дешевеет с", from.StepTier2From, to.StepTier2From)
	add("step_fee_over", "Цена ступени сверх порога", from.StepFeeOver, to.StepFeeOver)
	add("step_cap_views", "Потолок оплачиваемых просмотров", from.StepCapViews, to.StepCapViews)
	add("guarantee_views", "Гарантия, просмотров", from.GuaranteeViews, to.GuaranteeViews)
	add("creator_first_period_fee", "Креатору за первый период",
		from.CreatorFirstPeriodFee, to.CreatorFirstPeriodFee)
	add("creator_base_fee", "Креатору фикс со второго периода",
		from.CreatorBaseFee, to.CreatorBaseFee)
	add("creator_step_fee", "Креатору за ступень", from.CreatorStepFee, to.CreatorStepFee)
	add("creator_step_fee_over", "Креатору за ступень сверх порога",
		from.CreatorStepFeeOver, to.CreatorStepFeeOver)
	add("fee_per_video", "Фикс за ролик", from.FeePerVideo, to.FeePerVideo)
	add("creator_fee_per_video", "Креатору за ролик",
		from.CreatorFeePerVideo, to.CreatorFeePerVideo)
	add("subscriber_rate", "Ставка за подписчика", from.SubscriberRate, to.SubscriberRate)
	add("creator_subscriber_rate", "Креатору за подписчика",
		from.CreatorSubscriberRate, to.CreatorSubscriberRate)

	// Ступени сравниваем по порогам, а не по индексу в списке: ступень
	// вставляют в середину, и сравнение «первая с первой» показало бы
	// изменившимся весь хвост лесенки.
	out = append(out, diffSteps(from.Steps, to.Steps, "просмотров", "step")...)
	// Лесенка подписчиков — теми же правилами и своей подписью. Без неё
	// версия, в которой поменяли ТОЛЬКО ступени подписчиков, выходила бы
	// «без изменений»: и в журнале админа, и в вопросе «кому надо
	// согласиться заново» — а платить по ней клиенту уже по-другому.
	out = append(out, diffSteps(from.SubscriberSteps, to.SubscriberSteps, "подписчиков", "sub_step")...)
	return out
}

// diffSteps — что изменилось в лесенке порогов.
//
// Порог — это имя ступени, поэтому и подпись строится от него: «Ступень
// от 300 тыс.». Исчезнувшая ступень показывается переходом цены в
// пустоту, появившаяся — из пустоты: у обоих случаев на экране разный
// смысл, и «стало 0» вместо «ступени больше нет» соврало бы.
func diffSteps(from, to []TermsStep, unit, field string) []TermsChange {
	out := []TermsChange{}
	byFrom := func(steps []TermsStep) map[int64]TermsStep {
		m := make(map[int64]TermsStep, len(steps))
		for _, s := range steps {
			m[s.FromViews] = s
		}
		return m
	}
	a, b := byFrom(from), byFrom(to)
	seen := map[int64]bool{}
	thresholds := make([]int64, 0, len(a)+len(b))
	for _, list := range [][]TermsStep{from, to} {
		for _, s := range list {
			if !seen[s.FromViews] {
				seen[s.FromViews] = true
				thresholds = append(thresholds, s.FromViews)
			}
		}
	}
	sort.Slice(thresholds, func(i, j int) bool { return thresholds[i] < thresholds[j] })
	for _, th := range thresholds {
		label := fmt.Sprintf("Ступень от %d %s", th, unit)
		var was, now *int64
		if s, ok := a[th]; ok {
			v := s.ClientFee
			was = &v
		}
		if s, ok := b[th]; ok {
			v := s.ClientFee
			now = &v
		}
		if was == nil || now == nil || *was != *now {
			out = append(out, TermsChange{
				Field: fmt.Sprintf("%s_%d", field, th), Label: label, From: was, To: now,
			})
		}
		// Креаторская цена той же ступени — отдельной строкой: это другие
		// деньги, и меняют их порознь.
		var cwas, cnow *int64
		if s, ok := a[th]; ok {
			cwas = s.CreatorFee
		}
		if s, ok := b[th]; ok {
			cnow = s.CreatorFee
		}
		if (cwas == nil) != (cnow == nil) || (cwas != nil && cnow != nil && *cwas != *cnow) {
			out = append(out, TermsChange{
				Field: fmt.Sprintf("%s_%d_creator", field, th),
				Label: label + " — креатору", From: cwas, To: cnow,
			})
		}
	}
	return out
}

// ---- сервис ----

func (s *Service) ListTermsVersions(ctx context.Context) ([]TermsVersion, error) {
	return s.repo.ListTermsVersions(ctx)
}

// PublishTermsVersion — проверить и выпустить. actorID идёт в журнал
// админских действий в той же транзакции, что и сама версия.
func (s *Service) PublishTermsVersion(ctx context.Context, v TermsVersion, actorID uuid.UUID) (TermsPublishResult, error) {
	v.Body = strings.TrimSpace(v.Body)
	if v.Body == "" {
		return TermsPublishResult{}, fmt.Errorf("%w: текст условий обязателен", ErrInvalidInput)
	}
	if len([]rune(v.Body)) > 100_000 {
		return TermsPublishResult{}, fmt.Errorf("%w: текст условий слишком длинный", ErrInvalidInput)
	}
	// Ставки проверяем тем же кодом, что и условия проекта: правила
	// одинаковые, и расходиться им незачем.
	if _, err := s.SaveTermsCheck(v.Terms); err != nil {
		return TermsPublishResult{}, err
	}
	// Креатору нельзя обещать больше, чем берём с клиента: это не тариф,
	// а убыток на каждом ролике, и почти всегда — опечатка в поле.
	c := v.Terms.CreatorSide()
	if c.SalaryPerMonth > v.SalaryPerMonth ||
		c.RatePer1000Views > v.RatePer1000Views ||
		c.RatePer1000ViewsOver > v.RatePer1000ViewsOver {
		return TermsPublishResult{}, fmt.Errorf(
			"%w: доля креатора больше, чем платит клиент — проверьте ставки", ErrInvalidInput)
	}
	// То же правило для ступеней. Проверяется по лесенкам, а не по полям:
	// незаполненная креаторская ступень означает «как у клиента», и
	// сравнение пустого поля с числом ничего не значило бы.
	// Прежняя двухступенчатая модель. У лесенки произвольной длины свои
	// правила, и они уже проверены в checkRates — по каждой ступени.
	if v.Terms.StepViews != nil && v.Terms.StepFee != nil {
		cl, cr := v.Terms.ClientLadder(), v.Terms.CreatorLadder()
		if cr.FirstPeriodFee > cl.FirstPeriodFee || cr.BaseFee > cl.BaseFee ||
			cr.StepFee > cl.StepFee || cr.StepFeeOver > cl.StepFeeOver ||
			cr.TailRate > cl.TailRate {
			return TermsPublishResult{}, fmt.Errorf(
				"%w: ступени креатора дороже клиентских — проверьте тариф", ErrInvalidInput)
		}
	}
	return s.repo.PublishTermsVersion(ctx, v, actorID)
}

// SaveTermsCheck — только проверка ставок, без записи. Вынесена, чтобы
// прайс и условия проекта валидировались одинаково.
func (s *Service) SaveTermsCheck(t Terms) (Terms, error) {
	t.ProjectID = uuid.Nil
	return checkRates(t)
}
