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
	// Steps — лесенка ЭТОЙ стороны: пороги объёма периода и цена на
	// каждом, уже сведённая к её числам.
	//
	// Непустая лесенка ОТМЕНЯЕТ salary_per_month и ставку за тысячу выше:
	// на ступенчатой версии условий они в расчёте не участвуют вовсе. Без
	// этого поля кабинет креатора показывал бы ему оклад, которого в его
	// тарифе нет, — а на самом деле это цена клиента, и назвать её его
	// заработком нельзя.
	Steps []SideStep `json:"steps,omitempty"`
	// SubscriberRate — сколько стоит подписчик на этой стороне. Пусто —
	// KPI по подписчикам не считается.
	SubscriberRate *int64     `json:"subscriber_rate,omitempty" extensions:"x-nullable"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

// SideStep — ступень одной стороны сделки: порог и цена на нём.
//
// Отдельный тип, а не TermsStep, ровно потому, что у TermsStep две цены
// сразу — клиента и креатора. Здесь стороны уже разведены, и вторая цена
// в ответе означала бы, что мы показываем креатору цену клиента: до
// разведения сторон он именно её и считал своим заработком.
type SideStep struct {
	FromViews int64 `json:"from_views"`
	Fee       int64 `json:"fee"`
}

// side — общая сборка: поля те же, отличаются только числа.
//
// Лесенку берём ИЗ КЛИЕНТСКОЙ стороны переданного тарифа: t сюда уже
// приходит сведённым к нужной стороне (CreatorSide), и его Steps несут
// её цены. Второй раз применять creatorSteps значило бы взять
// креаторскую цену от креаторской — то есть один и тот же перевод
// дважды.
func side(t Terms) SideTerms {
	steps := make([]SideStep, 0, len(t.Steps))
	for _, st := range t.Steps {
		steps = append(steps, SideStep{FromViews: st.FromViews, Fee: st.ClientFee})
	}
	if len(steps) == 0 {
		// Пустой список и отсутствие списка на этом экране значат разное:
		// «версия не ступенчатая» против «ступени есть, но пустые».
		steps = nil
	}
	return SideTerms{
		Steps:                steps,
		SubscriberRate:       t.SubscriberRate,
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
	// CreatorAvatarURL/CreatorUsername — портрет и адрес страницы
	// исполнителя. Заказчик платит за конкретных людей, и из состава
	// периода он должен уметь перейти к тому, кто ролики снимал: буква в
	// кружке на это не отвечает. Ставок креатора здесь по-прежнему нет —
	// это его сторона сделки, а не клиентская.
	CreatorAvatarURL string `json:"creator_avatar_url,omitempty"`
	CreatorUsername  string `json:"creator_username,omitempty"`
	// CreatorProfilePublic — есть ли по этому адресу открытая страница.
	// Публичная карточка специалиста живёт только при is_published AND
	// moderation_status='approved', на всё прочее отдаёт 404. Признак
	// нужен экрану, чтобы не ставить ссылку туда, где её некуда вести:
	// мёртвая ссылка в составе, за который заказчик платит, читается как
	// «человека у вас нет», а человек есть — просто его страница ещё на
	// модерации.
	CreatorProfilePublic bool      `json:"creator_profile_public"`
	PeriodStart          time.Time `json:"period_start"`
	Salary               int64     `json:"salary"`
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
	// Tariff — из чего сложился этот счёт: первые просмотры каждого
	// ролика по стартовой ставке, всё сверх — по пониженной, плюс работа
	// команды. То же разложение, что в сводке, и тем же кодом.
	//
	// nil, когда разложение не сошлось бы с итогом рядом: ступенчатая
	// версия условий, порога нет вовсе или в счёте есть слагаемое,
	// которого в лесенке не бывает. Лесенка, не делящаяся в стоящую
	// рядом цену тысячи, хуже отсутствующей — её проверяют
	// калькулятором. См. tariffLadder.result.
	Tariff *OverviewTariff `json:"tariff,omitempty"`
	// Period — какой период показан и в каком он состоянии. Заказчику
	// это нужно по той же причине, что и менеджеру: пока период идёт,
	// числа ещё изменятся, и счёт нельзя считать окончательным.
	Period ClientPeriod `json:"period"`
}

// clientAccrual — строка начисления глазами заказчика.
func clientAccrual(a Accrual) ClientAccrual {
	return ClientAccrual{
		ID:               a.ID,
		ProjectID:        a.ProjectID,
		CreatorUserID:    a.CreatorUserID,
		CreatorName:      a.CreatorName,
		CreatorAvatarURL: a.CreatorAvatarURL,
		CreatorUsername:  a.CreatorUsername,

		CreatorProfilePublic: a.CreatorProfilePublic,

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
	// RatingScaleVersion — какой версией справочника порогов оценён
	// период. Пусто, пока период идёт: пороги замораживаются подытогом.
	// Номер, а не id: человеку показывают «оценено по версии 3».
	RatingScaleVersion *int `json:"rating_scale_version,omitempty"`
}

// clientPeriodView — период заказчика из общего периода проекта.
func clientPeriodView(p ProjectPeriod) ClientPeriod {
	return ClientPeriod{
		Seq:                p.Seq,
		StartsOn:           p.StartsOn,
		EndsOn:             p.EndsOn,
		Status:             p.Status,
		SnapshotAsOf:       p.SnapshotAsOf,
		SnapshotApprox:     p.SnapshotApprox,
		CarryIn:            p.CarryInClient,
		CarryOut:           p.CarryOutClient,
		RatingScaleVersion: p.RatingScaleVersion,
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
	// RatingScaleVersion — какой версией справочника порогов оценён
	// период. Пусто, пока период идёт.
	RatingScaleVersion *int `json:"rating_scale_version,omitempty"`
}

// creatorPeriodView — период креатора из общего периода проекта.
func creatorPeriodView(p ProjectPeriod) CreatorPeriod {
	return CreatorPeriod{
		Seq:                p.Seq,
		StartsOn:           p.StartsOn,
		EndsOn:             p.EndsOn,
		Status:             p.Status,
		SnapshotAsOf:       p.SnapshotAsOf,
		SnapshotApprox:     p.SnapshotApprox,
		CarryIn:            p.CarryInCreator,
		CarryOut:           p.CarryOutCreator,
		RatingScaleVersion: p.RatingScaleVersion,
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

	// creatorMinMatureVideos — со скольких СВОИХ зрелых роликов человеку
	// считают по его собственной истории. Меньше десяти — выборка
	// слишком короткая: один удачный ролик сдвигает медиану вдвое, и
	// «до ступени осталось» прыгало бы от него.
	creatorMinMatureVideos = 10
)

// Источники «типичного ролика» — от самого точного к самому общему.
const (
	// TypicalFromCreator — посчитано по его собственным роликам.
	TypicalFromCreator = "creator"
	// TypicalFromProject — по проекту: своих данных мало, но проект
	// достаточно большой, чтобы его медиана оставалась агрегатом.
	TypicalFromProject = "project"
	// TypicalFromDefault — не по чему считать вовсе, взято значение по
	// умолчанию.
	TypicalFromDefault = "default"
)

// Значение «типичного ролика» по умолчанию живёт не здесь: оно переехало
// в версионируемый справочник порогов (internal/ratings, поле
// typical_video_views первой версии). Там же его источник и дата —
// аналитика владельца продукта по 253 роликам за июнь–сентябрь 2026.
//
// Константы в коде тут больше нет намеренно: пороги должны
// версионироваться вместе с остальными, иначе правка числа задним числом
// молча меняет прошлые периоды.

// ProjectBenchmark — обезличенный ориентир проекта.
//
// Имён, идентификаторов и чьих-либо отдельных чисел здесь нет и быть не
// может: это агрегат, и любая строка, по которой угадывается человек,
// превращает его в чужие данные.
type ProjectBenchmark struct {
	// TypicalVideoViews — «типичный ролик»: от него считается, сколько
	// роликов осталось до следующей ступени. Лесенку выбора целиком
	// проходит сервер, фронту остаётся показать число и подпись.
	TypicalVideoViews int64 `json:"typical_video_views"`
	// TypicalVideoSource — на чьих данных посчитано: creator | project |
	// default.
	//
	// Обязателен, а не украшение: человеку говорят «до ступени двадцать
	// роликов», и он вправе знать, чьи это данные. «По твоим роликам» и
	// «ориентир по площадке, своих данных пока мало» — разные обещания,
	// и второе не должно выдавать себя за первое.
	TypicalVideoSource string `json:"typical_video_source"`

	// MyMatureVideos — сколько зрелых роликов у него самого. Его
	// собственное число, обезличивать нечего; заодно объясняет, почему
	// источник именно такой.
	MyMatureVideos int `json:"my_mature_videos"`

	// ProjectMedianViews — медиана просмотров зрелых роликов проекта.
	// Пусто, если проект не прошёл порог обезличивания.
	ProjectMedianViews *int64 `json:"project_median_views,omitempty"`
	// MatureVideos — сколько зрелых роликов проекта вошло в расчёт.
	// Пусто по той же причине: ниже порога это число говорит о составе
	// проекта больше, чем следует.
	MatureVideos *int `json:"mature_videos,omitempty"`
	// MyPercentile — где человек относительно медианы проекта: доля
	// зрелых роликов проекта, которые слабее его медианного. 0 — ниже
	// всех, 100 — выше всех. Пусто, если сравнивать не с чем или не по
	// чему.
	MyPercentile *int `json:"my_percentile,omitempty"`
}

// StepViews — ступень тарифа: сколько просмотров закрывают одну.
//
// Сто тысяч. В самом тарифе этого числа пока нет: ступенчатый тариф ещё
// не выпущен, в действующих условиях ставка за тысячу без ступеней.
// Когда его выпустят, ступень переедет в версию условий и станет
// настраиваемой — отсюда, из одного места.
//
// Ступень нужна уже сейчас: по ней считается перенос остатка и полоса в
// кабинете креатора — «до следующей ступени столько-то».
const StepViews int64 = 100_000

// NextStepForecast — что даст следующая ступень.
//
// Это ПРОГНОЗ, а не начисленное, и назван он так, чтобы перепутать было
// нельзя: в кабинете полоса идёт от начисленного к этому числу, и если
// подписать его заработанным, человек будет ждать денег, которых ещё нет.
//
// Только креаторская сторона: сколько получит он сам. Клиентских сумм
// здесь нет ни под каким именем.
type NextStepForecast struct {
	// StepViews — размер ступени.
	StepViews int64 `json:"step_views"`
	// ViewsToGo — сколько просмотров осталось до неё. Считается от
	// просмотров периода ПЛЮС перенесённый остаток: он уже в счёте, и не
	// учесть его значило бы отправить человека добирать то, что у него
	// уже есть.
	ViewsToGo int64 `json:"views_to_go"`
	// CarryInIncluded — сколько перенесённого остатка учтено. Ноль, пока
	// арифметику переноса не включили вместе со ступенчатым тарифом.
	CarryInIncluded int64 `json:"carry_in_included"`
	// ForecastPayout — сколько эти просмотры принесут ЕМУ по условиям
	// этого проекта, копейки.
	//
	// Считается тем же кодом, что и само начисление: разница между
	// выплатой сейчас и выплатой при выросших просмотрах. Отдельной
	// формулы «для прогноза» нет намеренно — она разошлась бы с
	// фактической выплатой, и разошлась бы молча.
	//
	// Имя начинается с forecast, а не с payout, намеренно: payout_* в
	// этом коде — начисленное в строке расчёта, и прогноз под тем же
	// префиксом однажды прочитали бы как заработанное. Тест приватности
	// на префиксе payout_ это и поймал.
	ForecastPayout int64 `json:"forecast_payout"`
}
