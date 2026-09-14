package billing

import (
	"time"

	"github.com/google/uuid"
)

// Ответы тем, кто не в команде: заказчику и креатору.
//
// Отдельные типы, а не почищенный ProjectBilling. Зануление полей в
// сервисе решает сегодняшнюю утечку и ничего не обещает на завтра:
// добавят в общую структуру новое поле про деньги — и оно снова молча
// уедет наружу. Здесь наоборот: новое поле по умолчанию НЕ попадает в
// чужой ответ, пока его не положат сюда руками. Умолчание должно быть
// на стороне «не показываем».
//
// Что было: GET /me/projects/{id}/billing отдавал ту же структуру, что
// менеджерская ручка, — вместе с выплатами креаторам (payout_*),
// маржой площадки (totals.margin, totals.payouts) и креаторской
// стороной тарифа. Фронт этих полей не рисовал, то есть тайна держалась
// на вёрстке: заказчику хватало вкладки «Сеть» в браузере.

// SideTerms — тариф одной стороны: только те числа, которые касаются
// того, кто смотрит.
//
// Одна форма и для заказчика, и для креатора — разные в ней только
// значения: заказчику кладём цену клиента, креатору его ставки (см.
// Terms.CreatorSide). Креаторских полей здесь нет вовсе: для креатора
// они и есть основные числа, а для заказчика это чужая сторона сделки.
type SideTerms struct {
	ProjectID uuid.UUID `json:"project_id"`
	// TermsVersionID — с какой версии прайса сняты числа.
	TermsVersionID       *uuid.UUID `json:"terms_version_id,omitempty"`
	SalaryPerMonth       int64      `json:"salary_per_month"`
	VideosFirstMonth     int        `json:"videos_first_month"`
	VideosNextMonths     int        `json:"videos_next_months"`
	RatePer1000Views     int64      `json:"rate_per_1000_views"`
	BonusViewsThreshold  int64      `json:"bonus_views_threshold"`
	RatePer1000ViewsOver int64      `json:"rate_per_1000_views_over"`
	ClickBonusRate       *int64     `json:"click_bonus_rate,omitempty" extensions:"x-nullable"`
	ClickBonusThreshold  int        `json:"click_bonus_threshold"`
	ClickBonusRateOver   *int64     `json:"click_bonus_rate_over,omitempty" extensions:"x-nullable"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

// side — общая сборка: поля те же, отличаются только числа.
func side(t Terms) SideTerms {
	return SideTerms{
		ProjectID:            t.ProjectID,
		TermsVersionID:       t.TermsVersionID,
		SalaryPerMonth:       t.SalaryPerMonth,
		VideosFirstMonth:     t.VideosFirstMonth,
		VideosNextMonths:     t.VideosNextMonths,
		RatePer1000Views:     t.RatePer1000Views,
		BonusViewsThreshold:  t.BonusViewsThreshold,
		RatePer1000ViewsOver: t.RatePer1000ViewsOver,
		ClickBonusRate:       t.ClickBonusRate,
		ClickBonusThreshold:  t.ClickBonusThreshold,
		ClickBonusRateOver:   t.ClickBonusRateOver,
		UpdatedAt:            t.UpdatedAt,
	}
}

// ClientTerms — тариф глазами того, кто платит: цена клиента без того,
// сколько из неё достаётся креатору.
func (t Terms) ClientTerms() SideTerms { return side(t) }

// CreatorTerms — тариф глазами того, кто получает: его ставки под теми
// же именами полей. Незаполненная креаторская ставка означает «как у
// клиента» — это делает CreatorSide.
func (t Terms) CreatorTerms() SideTerms { return side(t.CreatorSide()) }

// ClientAccrual — строка «Команды месяца» у заказчика: за что и сколько
// ему выставлено по конкретному человеку.
//
// Начисления заказчик видит намеренно: он за эту команду платит, и
// строка «Маша · 1 ролик · 65 000 просмотров · 60 000 + 5 850» — его
// счёт. Чего здесь нет — сколько из этих денег получит сама Маша.
type ClientAccrual struct {
	ID            uuid.UUID `json:"id"`
	ProjectID     uuid.UUID `json:"project_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	PeriodStart   time.Time `json:"period_start"`
	Salary        int64     `json:"salary"`
	// VideosPlanned/VideosDelivered — сколько выкладок стояло и сколько
	// закрыто; разница объясняет вычет.
	VideosPlanned   int   `json:"videos_planned"`
	VideosDelivered int   `json:"videos_delivered"`
	Deduction       int64 `json:"deduction"`
	ViewsTotal      int64 `json:"views_total"`
	ViewsBase       int64 `json:"views_base"`
	ViewsOver       int64 `json:"views_over"`
	ViewsBonus      int64 `json:"views_bonus"`
	Clicks          int   `json:"clicks"`
	ClickBonus      int64 `json:"click_bonus"`
	// Total — счёт заказчику за этого человека.
	Total int64 `json:"total"`
	// Status/ApprovedAt/PaidAt — в каком состоянии расчёт за месяц.
	// Сумм в них нет, а заказчику видно, посчитан месяц или ещё нет.
	Status     AccrualStatus `json:"status" enums:"draft,approved,paid"`
	ApprovedAt *time.Time    `json:"approved_at,omitempty"`
	PaidAt     *time.Time    `json:"paid_at,omitempty"`
	// Priority — каким по приоритету человек попал в подборку. Поля
	// нет, если проект заведён руками, а не вырос из заказа.
	Priority     int       `json:"priority,omitempty" extensions:"x-omitempty"`
	CalculatedAt time.Time `json:"calculated_at"`
}

// ClientPeriodTotals — итог месяца для заказчика. Payouts и Margin из
// менеджерской сводки здесь нет: это наши деньги, а не его.
type ClientPeriodTotals struct {
	Salaries   int64 `json:"salaries"`
	Deductions int64 `json:"deductions"`
	ViewsBonus int64 `json:"views_bonus"`
	ClickBonus int64 `json:"click_bonus"`
	// Total — сколько выставлено заказчику за месяц.
	Total           int64 `json:"total"`
	Videos          int   `json:"videos"`
	VideosDelivered int   `json:"videos_delivered"`
	Views           int64 `json:"views"`
	// CostPer1000 — во сколько обошлась тысяча просмотров. nil, пока
	// просмотров нет — делить не на что.
	CostPer1000 *int64 `json:"cost_per_1000,omitempty"`
}

// ClientBillingView — ответ GET /me/projects/{id}/billing.
type ClientBillingView struct {
	Terms    SideTerms `json:"terms"`
	Payments []Payment `json:"payments"`
	// Accruals — за запрошенный месяц. Пусто, пока не считали.
	Accruals []ClientAccrual    `json:"accruals"`
	Totals   ClientPeriodTotals `json:"totals"`
	// Period — какой период показан и в каком он состоянии. Заказчику
	// это нужно по той же причине, что и менеджеру: пока период идёт,
	// числа ещё изменятся, и счёт нельзя считать окончательным.
	Period ClientPeriod `json:"period"`
}

// clientAccrual — строка начисления глазами заказчика.
func clientAccrual(a Accrual) ClientAccrual {
	return ClientAccrual{
		ID:              a.ID,
		ProjectID:       a.ProjectID,
		CreatorUserID:   a.CreatorUserID,
		CreatorName:     a.CreatorName,
		PeriodStart:     a.PeriodStart,
		Salary:          a.Salary,
		VideosPlanned:   a.VideosPlanned,
		VideosDelivered: a.VideosDelivered,
		Deduction:       a.Deduction,
		ViewsTotal:      a.ViewsTotal,
		ViewsBase:       a.ViewsBase,
		ViewsOver:       a.ViewsOver,
		ViewsBonus:      a.ViewsBonus,
		Clicks:          a.Clicks,
		ClickBonus:      a.ClickBonus,
		Total:           a.Total,
		Status:          a.Status,
		ApprovedAt:      a.ApprovedAt,
		PaidAt:          a.PaidAt,
		Priority:        a.Priority,
		CalculatedAt:    a.CalculatedAt,
	}
}

// clientTotals — итог месяца без наших денег. Считается из тех же
// строк, что и менеджерский, чтобы два экрана не разошлись в числах.
func clientTotals(t PeriodTotals) ClientPeriodTotals {
	return ClientPeriodTotals{
		Salaries:        t.Salaries,
		Deductions:      t.Deductions,
		ViewsBonus:      t.ViewsBonus,
		ClickBonus:      t.ClickBonus,
		Total:           t.Total,
		Videos:          t.Videos,
		VideosDelivered: t.VideosDelivered,
		Views:           t.Views,
		CostPer1000:     t.CostPer1000,
	}
}

// ClientPeriod — период глазами заказчика.
//
// Отдельный тип по той же причине, что и CreatorPeriod: в периоде лежит
// перенос остатка ПО ОБЕИМ сторонам, и креаторская — не его дело. До
// этого типа в клиентский ответ уезжал ProjectPeriod целиком, вместе с
// carry_*_creator: та же утечка, что с выплатами и маржой, просто
// моложе на один день.
type ClientPeriod struct {
	// Seq — какой это период по счёту.
	Seq int `json:"seq"`
	// StartsOn/EndsOn — границы, обе включительно. Считает сервер: иначе
	// правило периода описано и в браузере тоже, и разъедется.
	StartsOn time.Time `json:"starts_on"`
	EndsOn   time.Time `json:"ends_on"`
	// Status — open (идёт, числа ещё изменятся) | locked (подытожен).
	Status string `json:"status"`
	// SnapshotAsOf — на какую дату сняты числа подытоженного периода.
	SnapshotAsOf *time.Time `json:"snapshot_as_of,omitempty"`
	// SnapshotApprox — числам не на что опереться: поденная статистика к
	// моменту подытога уже удалена.
	SnapshotApprox bool `json:"snapshot_approx,omitempty"`
	// CarryIn/CarryOut — перенос остатка ступени, его сторона. Пока нули.
	CarryIn  int64 `json:"carry_in_client"`
	CarryOut int64 `json:"carry_out_client"`
}

// clientPeriodView — период заказчика из общего периода проекта.
func clientPeriodView(p ProjectPeriod) ClientPeriod {
	return ClientPeriod{
		Seq:            p.Seq,
		StartsOn:       p.StartsOn,
		EndsOn:         p.EndsOn,
		Status:         p.Status,
		SnapshotAsOf:   p.SnapshotAsOf,
		SnapshotApprox: p.SnapshotApprox,
		CarryIn:        p.CarryInClient,
		CarryOut:       p.CarryOutClient,
	}
}

// CreatorPeriod — период глазами креатора.
//
// Отдельный тип, а не ProjectPeriod с вырезанными полями: в периоде
// лежит перенос остатка ПО ОБЕИМ сторонам, и клиентская — такая же
// коммерческая тайна, как креаторские ставки для заказчика. С общим
// типом каждое новое поле уезжало бы креатору само; здесь наоборот —
// пока его не положат сюда руками, оно остаётся внутри.
//
// Идентификаторов периода (id, prev_period_id) тоже нет: цепочка
// переноса — наша механика, а креатору нужно «какой это месяц по счёту,
// с какого по какое и посчитан ли он».
type CreatorPeriod struct {
	// Seq — какой это период по счёту: первый, второй, третий.
	Seq int `json:"seq"`
	// StartsOn/EndsOn — границы, обе включительно. Отдаём с сервера, а
	// не оставляем фронту прибавлять месяц: правило периода живёт в
	// одном месте, иначе браузер продолжит рисовать старые границы после
	// первой же правки правила.
	StartsOn time.Time `json:"starts_on"`
	EndsOn   time.Time `json:"ends_on"`
	// Status — open (идёт, числа ещё изменятся) | locked (подытожен).
	Status string `json:"status"`
	// SnapshotAsOf — на какую дату сняты числа подытоженного периода.
	SnapshotAsOf *time.Time `json:"snapshot_as_of,omitempty"`
	// SnapshotApprox — числам не на что опереться: поденная статистика к
	// моменту подытога уже удалена. Не то же самое, что «предварительно»
	// у строки начисления.
	SnapshotApprox bool `json:"snapshot_approx,omitempty"`
	// CarryIn/CarryOut — перенос остатка ступени, ЕГО сторона: сколько
	// просмотров пришло из прошлого периода и сколько уходит в
	// следующий. Пока нули — арифметику включат вместе с тарифом.
	CarryIn  int64 `json:"carry_in_creator"`
	CarryOut int64 `json:"carry_out_creator"`
}

// creatorPeriodView — период креатора из общего периода проекта.
func creatorPeriodView(p ProjectPeriod) CreatorPeriod {
	return CreatorPeriod{
		Seq:            p.Seq,
		StartsOn:       p.StartsOn,
		EndsOn:         p.EndsOn,
		Status:         p.Status,
		SnapshotAsOf:   p.SnapshotAsOf,
		SnapshotApprox: p.SnapshotApprox,
		CarryIn:        p.CarryInCreator,
		CarryOut:       p.CarryOutCreator,
	}
}

// CreatorAccrual — строка заработка креатора: его числа и только его.
//
// Клиентской стороны здесь нет ни под каким именем. Раньше креатору
// уезжала та же структура, что менеджеру, — с клиентскими полями,
// затёртыми его значениями: утечки не было, но держалась она на том,
// что кто-то не забудет затереть очередное новое поле.
type CreatorAccrual struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	// CreatorUserID — он сам. Не тайна и не лишнее: строки приходят по
	// месяцам, и фронту нужно, чем их метить.
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	PeriodStart   time.Time `json:"period_start"`
	// Salary/Deduction — оклад и вычет за недосданное, по его ставкам.
	Salary    int64 `json:"salary"`
	Deduction int64 `json:"deduction"`
	// VideosPlanned/VideosDelivered — сколько выкладок стояло и сколько
	// он закрыл: из них и растёт вычет.
	VideosPlanned   int   `json:"videos_planned"`
	VideosDelivered int   `json:"videos_delivered"`
	ViewsTotal      int64 `json:"views_total"`
	ViewsBase       int64 `json:"views_base"`
	ViewsOver       int64 `json:"views_over"`
	ViewsBonus      int64 `json:"views_bonus"`
	Clicks          int   `json:"clicks"`
	ClickBonus      int64 `json:"click_bonus"`
	// Total — сколько он получит за месяц.
	Total      int64         `json:"total"`
	Status     AccrualStatus `json:"status" enums:"draft,approved,paid"`
	ApprovedAt *time.Time    `json:"approved_at,omitempty"`
	PaidAt     *time.Time    `json:"paid_at,omitempty"`
	// CalculatedAt — когда посчитано.
	CalculatedAt time.Time `json:"calculated_at"`
}

// creatorAccrual — строка начисления глазами креатора: payout-раскладка
// под нейтральными именами, клиентских чисел нет.
func creatorAccrual(a Accrual) CreatorAccrual {
	return CreatorAccrual{
		ID:              a.ID,
		ProjectID:       a.ProjectID,
		CreatorUserID:   a.CreatorUserID,
		PeriodStart:     a.PeriodStart,
		Salary:          a.PayoutSalary,
		Deduction:       a.PayoutDeduction,
		VideosPlanned:   a.VideosPlanned,
		VideosDelivered: a.VideosDelivered,
		ViewsTotal:      a.ViewsTotal,
		ViewsBase:       a.ViewsBase,
		ViewsOver:       a.ViewsOver,
		ViewsBonus:      a.PayoutViewsBonus,
		Clicks:          a.Clicks,
		ClickBonus:      a.PayoutClickBonus,
		Total:           a.PayoutTotal,
		Status:          a.Status,
		ApprovedAt:      a.ApprovedAt,
		PaidAt:          a.PaidAt,
		CalculatedAt:    a.CalculatedAt,
	}
}

// Обезличенный ориентир для кабинета креатора.
//
// Своих зрелых роликов у человека может быть мало — тогда «сколько
// роликов до следующей ступени» считать не по чему. Медиана по проекту
// эту дыру закрывает, но только пока она остаётся АГРЕГАТОМ.

const (
	// matureVideoAge — со скольких дней ролик считается зрелым. Те же
	// две недели, что у отсечки периода: за это время ролик набирает
	// основную массу просмотров, и раньше сравнивать его с другими
	// нечестно — он ещё растёт.
	matureVideoAge = 14 * 24 * time.Hour

	// medianMinVideos/medianMinCreators — порог обезличивания.
	//
	// Это не оптимизация, а граница между агрегатом и чужими данными. В
	// проекте с двумя креаторами и пятью роликами «медиана проекта» —
	// это, по сути, показатель соседа, и отдав её, мы своими руками
	// покажем одному креатору результаты другого. Десять роликов минимум
	// от трёх человек: при таком составе ни одно отдельное число из
	// медианы не восстанавливается, даже если знать свои.
	//
	// Ниже порога поля просто нет — «мало данных» честнее, чем число, за
	// которым стоит один человек.
	medianMinVideos   = 10
	medianMinCreators = 3
)

// ProjectBenchmark — обезличенный ориентир проекта.
//
// Имён, идентификаторов и чьих-либо отдельных чисел здесь нет и быть не
// может: это агрегат, и любая строка, по которой угадывается человек,
// превращает его в чужие данные.
type ProjectBenchmark struct {
	// MedianViews — медиана просмотров зрелых роликов проекта.
	MedianViews int64 `json:"project_median_views"`
	// MyPercentile — где человек относительно этой медианы: доля зрелых
	// роликов проекта, у которых просмотров меньше, чем у его медианного
	// ролика. 0 — ниже всех, 100 — выше всех. Пусто, если своих зрелых
	// роликов нет вовсе: сравнивать нечего.
	MyPercentile *int `json:"my_percentile,omitempty"`
	// MatureVideos — сколько зрелых роликов вошло в расчёт. Число само по
	// себе обезличено и объясняет, насколько ориентиру можно верить.
	MatureVideos int `json:"mature_videos"`
}
