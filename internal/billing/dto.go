// Package billing — деньги проекта: условия, платежи заказчика и
// начисления креаторам.
//
// Платёжного провайдера здесь нет. Клиент читает условия и платит мимо
// системы, менеджер подтверждает получение кнопкой — тем же способом,
// каким двигает этапы в продакшн-проекте. Автоматически считается ровно
// одно: бонус по просмотрам, потому что просмотры собираются каждый день
// и складывать их руками бессмысленно.
//
// Все суммы — в КОПЕЙКАХ. Деление на 1000 просмотров в целых рублях даёт
// расхождение в последнем знаке, и оно всплывает при сверке.
package billing

import (
	"time"

	"github.com/google/uuid"
)

// PaymentKind — вид платежа заказчика. Половина вперёд, половина по
// завершении — других видов не бывает.
type PaymentKind string

const (
	PaymentPrepayment PaymentKind = "prepayment"
	PaymentFinal      PaymentKind = "final"
)

// PaymentStatus — состояние платежа.
type PaymentStatus string

const (
	PaymentAwaiting  PaymentStatus = "awaiting"
	PaymentConfirmed PaymentStatus = "confirmed"
	PaymentCancelled PaymentStatus = "cancelled"
)

// AccrualStatus — состояние начисления. Утверждение и выплата — разные
// кнопки: между «посчитали» и «отправили деньги» проходит время.
type AccrualStatus string

const (
	AccrualDraft    AccrualStatus = "draft"
	AccrualApproved AccrualStatus = "approved"
	AccrualPaid     AccrualStatus = "paid"
)

// Terms — условия проекта. Снимок с версии правил: изменение прайса не
// переписывает историю уже идущего проекта (ЗК-БП10).
type Terms struct {
	ProjectID uuid.UUID `json:"project_id"`
	// TermsVersionID — с какой версии сняты числа. Только для истории.
	TermsVersionID *uuid.UUID `json:"terms_version_id,omitempty"`
	// SalaryPerMonth — оклад креатора за месяц, копейки.
	SalaryPerMonth int64 `json:"salary_per_month"`
	// VideosFirstMonth/VideosNextMonths — за какой объём назван оклад.
	// В тарифе это подпись «30 видео первый месяц, 60 со второго»: без
	// неё сумма оклада ни о чём не говорит.
	VideosFirstMonth int `json:"videos_first_month"`
	VideosNextMonths int `json:"videos_next_months"`
	// RatePer1000Views — ставка за тысячу просмотров ДО порога, копейки.
	RatePer1000Views int64 `json:"rate_per_1000_views"`
	// BonusViewsThreshold — порог НА РОЛИК, суммой по пяти площадкам.
	// До него платим полную ставку, свыше — пониженную. 0 = порога нет,
	// весь объём идёт по полной ставке.
	BonusViewsThreshold int64 `json:"bonus_views_threshold"`
	// RatePer1000ViewsOver — ставка за тысячу просмотров СВЕРХ порога.
	// Ниже основной намеренно: так виральный ролик не съедает бюджет.
	RatePer1000ViewsOver int64 `json:"rate_per_1000_views_over"`
	// ClickBonusRate — ставка за переход по UTM до месячного порога,
	// копейки. nil = бонус за переходы не считается: источник кликов не
	// подключён, а поля в тарифе есть.
	ClickBonusRate *int64 `json:"click_bonus_rate,omitempty" extensions:"x-nullable"`
	// ClickBonusThreshold — сколько переходов ЗА МЕСЯЦ идёт по полной
	// ставке. Порог здесь месячный, а не на ролик — в отличие от просмотров.
	ClickBonusThreshold int `json:"click_bonus_threshold"`
	// ClickBonusRateOver — ставка за каждый следующий переход.
	ClickBonusRateOver *int64 `json:"click_bonus_rate_over,omitempty" extensions:"x-nullable"`

	// Креаторская сторона тарифа: что получает исполнитель.
	//
	// nil означает «столько же, сколько платит клиент»: до заполнения этих
	// полей выплата равна счёту и маржи у платформы нет. Две стороны нужны
	// потому, что это разные деньги — счёт заказчику и обязательство перед
	// креатором; одно число за оба показывало креатору в личном кабинете
	// цену клиента как его собственный заработок.
	CreatorSalaryPerMonth       *int64 `json:"creator_salary_per_month,omitempty" extensions:"x-nullable"`
	CreatorRatePer1000Views     *int64 `json:"creator_rate_per_1000_views,omitempty" extensions:"x-nullable"`
	CreatorRatePer1000ViewsOver *int64 `json:"creator_rate_per_1000_views_over,omitempty" extensions:"x-nullable"`

	// ---- ступенчатый тариф ----
	//
	// Заполненный StepViews означает, что версия ступенчатая и считается
	// по этим полям; поля старой модели (оклад и ставка за тысячу) в ней
	// не участвуют. У прежних версий они пусты, и расчёт уходит в старую
	// ветку — проекты, стоящие на них, не пересчитываются.

	// StepViews — размер ступени в просмотрах. Раньше сто тысяч были
	// константой в коде.
	StepViews *int64 `json:"step_views,omitempty" extensions:"x-nullable"`
	// FirstPeriodFee — фикс за первый период проекта: там оплачивается
	// запуск, а не результат, и ступени не считаются вовсе.
	FirstPeriodFee *int64 `json:"first_period_fee,omitempty" extensions:"x-nullable"`
	// BaseFee — фикс со второго периода.
	BaseFee *int64 `json:"base_fee,omitempty" extensions:"x-nullable"`
	// StepFee — цена полной ступени до StepTier2From.
	StepFee *int64 `json:"step_fee,omitempty" extensions:"x-nullable"`
	// StepTier2From — с какого объёма ступень дешевеет.
	StepTier2From *int64 `json:"step_tier2_from,omitempty" extensions:"x-nullable"`
	// StepFeeOver — цена ступени после этого порога.
	StepFeeOver *int64 `json:"step_fee_over,omitempty" extensions:"x-nullable"`
	// StepCapViews — выше этого объёма ступени не оплачиваются.
	StepCapViews *int64 `json:"step_cap_views,omitempty" extensions:"x-nullable"`
	// GuaranteeViews — гарантия в просмотрах. Недобрали — период всё
	// равно оплачивается как гарантия, а недостающие просмотры уходят в
	// долг перед клиентом и гасятся из следующего периода. Долг в
	// ПРОСМОТРАХ, а не в деньгах: так в оферте.
	GuaranteeViews *int64 `json:"guarantee_views,omitempty" extensions:"x-nullable"`

	// Креаторская сторона тех же ступеней. nil означает «столько же,
	// сколько у клиента» — то же правило, что у нынешних креаторских
	// ставок.
	CreatorFirstPeriodFee *int64 `json:"creator_first_period_fee,omitempty" extensions:"x-nullable"`
	CreatorBaseFee        *int64 `json:"creator_base_fee,omitempty" extensions:"x-nullable"`
	CreatorStepFee        *int64 `json:"creator_step_fee,omitempty" extensions:"x-nullable"`
	CreatorStepFeeOver    *int64 `json:"creator_step_fee_over,omitempty" extensions:"x-nullable"`

	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Stepped — версия условий считается по ступеням.
//
// Признак — заполненная ступень: без неё считать ступенчато не по чему,
// и расчёт уходит в прежнюю модель (оклад плюс ставка за тысячу).
// Проверка в одном месте: разложи её по вызывающим — и однажды половина
// кода посчитает версию ступенчатой, а половина нет.
func (t Terms) Stepped() bool {
	return t.StepViews != nil && *t.StepViews > 0 && t.StepFee != nil
}

// StepLadder — ступени одной стороны сделки.
//
// Клиентская и креаторская стороны различаются только числами, поэтому
// лесенка одна: две копии этих правил разъехались бы на первой правке,
// и разница между «что заплатил клиент» и «что получил креатор» стала бы
// следствием ошибки, а не тарифа.
type StepLadder struct {
	StepViews      int64
	FirstPeriodFee int64
	BaseFee        int64
	StepFee        int64
	Tier2From      int64
	StepFeeOver    int64
	CapViews       int64
	// TailRate — ставка за тысячу просмотров вирального хвоста, то есть
	// сверх порога НА РОЛИК (Terms.BonusViewsThreshold). Хвост в ступени
	// не идёт и оплачивается отдельно по этой пониженной ставке.
	TailRate int64
}

// Tail — сколько стоит виральный хвост периода.
//
// Порогов в тарифе два, и они про разное. Порог на ролик защищает от
// одной виральной удачи: миллион просмотров одного ролика — это не рост
// аккаунта, и платить за него как за работу месяца клиент не должен.
// Месячные ступени, наоборот, отражают именно рост. Одно другое не
// заменяет, поэтому хвост вынут из объёма ДО расчёта ступеней и
// оплачивается своей ставкой.
//
// Тысячи только полные: остаток меньше тысячи не оплачивается — то же
// правило, что у ступеней.
func (l StepLadder) Tail(viewsOver int64) int64 {
	if viewsOver <= 0 || l.TailRate <= 0 {
		return 0
	}
	return viewsOver / 1000 * l.TailRate
}

// ClientLadder — ступени клиентской стороны.
func (t Terms) ClientLadder() StepLadder {
	return StepLadder{
		StepViews:      derefOr(t.StepViews, 0),
		FirstPeriodFee: derefOr(t.FirstPeriodFee, 0),
		BaseFee:        derefOr(t.BaseFee, 0),
		StepFee:        derefOr(t.StepFee, 0),
		Tier2From:      derefOr(t.StepTier2From, 0),
		StepFeeOver:    derefOr(t.StepFeeOver, 0),
		CapViews:       derefOr(t.StepCapViews, 0),
		TailRate:       t.RatePer1000ViewsOver,
	}
}

// CreatorLadder — ступени креаторской стороны. Незаполненное число
// означает «как у клиента»: до выпуска настоящих креаторских ставок
// стороны совпадают и маржи на просмотрах нет.
//
// ВНИМАНИЕ: при равных ставках и одностороннем переносе остатка креатору
// уходит БОЛЬШЕ ступеней, чем выставлено клиенту — клиентский остаток
// сгорает, креаторский копится. Это следствие решения владельца
// продукта, а не ошибка счёта; видно здесь, чтобы не искали причину в
// арифметике.
func (t Terms) CreatorLadder() StepLadder {
	l := t.ClientLadder()
	if t.CreatorFirstPeriodFee != nil {
		l.FirstPeriodFee = *t.CreatorFirstPeriodFee
	}
	if t.CreatorBaseFee != nil {
		l.BaseFee = *t.CreatorBaseFee
	}
	if t.CreatorStepFee != nil {
		l.StepFee = *t.CreatorStepFee
	}
	if t.CreatorStepFeeOver != nil {
		l.StepFeeOver = *t.CreatorStepFeeOver
	}
	l.TailRate = t.creatorTailRate(l.StepViews, l.StepFee)
	return l
}

// tailRateDivisor — во сколько раз ставка за виральный хвост ниже
// базовой. Правило тарифа, а не совпадение: и в почасовой версии (90 ₽ и
// 9 ₽), и в ступенчатой (60 ₽ и 6 ₽) отношение одно и то же.
const tailRateDivisor = 10

// creatorTailRate — ставка креатора за виральный хвост.
//
// Порядок намеренный: названное руками число главнее правила, но если
// названа только базовая ставка креатора — пониженная выводится из неё
// сама. Оставить её пустой значило бы «как у клиента», то есть отдать
// креатору за хвост клиентскую цену и съесть маржу ровно там, где тариф
// специально понижен.
//
// Базовая ставка креатора берётся из его ставки за тысячу, а если
// сторона задана ступенями — из его ступени: ступень и есть та же
// ставка, выраженная за сто тысяч просмотров.
func (t Terms) creatorTailRate(stepViews, creatorStepFee int64) int64 {
	switch {
	case t.CreatorRatePer1000ViewsOver != nil:
		return *t.CreatorRatePer1000ViewsOver
	case t.CreatorRatePer1000Views != nil:
		return *t.CreatorRatePer1000Views / tailRateDivisor
	case t.CreatorStepFee != nil && stepViews > 0:
		return creatorStepFee * 1000 / stepViews / tailRateDivisor
	default:
		return t.RatePer1000ViewsOver
	}
}

// Fee — сколько стоит период с таким объёмом просмотров.
//
// Возвращает сумму и число оплаченных ступеней. Первый период считается
// фиксом: там оплачивается запуск, а не результат, и ступеней в нём нет.
//
// Ступени только ПОЛНЫЕ: 149 999 просмотров — это одна ступень, а не
// полторы. Что делать с остатком, решает вызывающий: у клиента он
// сгорает, у креатора переносится.
func (l StepLadder) Fee(seq int, views int64) (fee int64, steps int64) {
	if seq <= 1 {
		return l.FirstPeriodFee, 0
	}
	fee = l.BaseFee
	if l.StepViews <= 0 {
		return fee, 0
	}
	// Выше потолка ступени не оплачиваются вовсе.
	counted := views
	if l.CapViews > 0 && counted > l.CapViews {
		counted = l.CapViews
	}
	steps = counted / l.StepViews

	// Первая ступень дороже второй: ставка падает намеренно, чтобы
	// виральный ролик не съедал бюджет клиента.
	cheapFrom := l.Tier2From
	if cheapFrom <= 0 || cheapFrom > counted {
		return fee + steps*l.StepFee, steps
	}
	full := cheapFrom / l.StepViews
	if full > steps {
		full = steps
	}
	fee += full * l.StepFee
	fee += (steps - full) * l.StepFeeOver
	return fee, steps
}

func derefOr(v *int64, def int64) int64 {
	if v == nil {
		return def
	}
	return *v
}

// CreatorSide — тот же тариф, но ставками креатора. Незаполненная
// креаторская ставка означает «как у клиента», поэтому расчёт выплаты
// пользуется тем же кодом, что и расчёт счёта, — просто другими числами.
func (t Terms) CreatorSide() Terms {
	c := t
	if t.CreatorSalaryPerMonth != nil {
		c.SalaryPerMonth = *t.CreatorSalaryPerMonth
	}
	if t.CreatorRatePer1000Views != nil {
		c.RatePer1000Views = *t.CreatorRatePer1000Views
	}
	c.RatePer1000ViewsOver = t.creatorTailRate(derefOr(t.StepViews, 0), derefOr(t.CreatorStepFee, 0))
	return c
}

// HasMargin — стороны тарифа различаются, то есть платформа что-то
// оставляет себе. Интерфейсу это нужно, чтобы не рисовать две одинаковые
// колонки там, где маржи нет.
func (t Terms) HasMargin() bool {
	return t.CreatorSalaryPerMonth != nil ||
		t.CreatorRatePer1000Views != nil ||
		t.CreatorRatePer1000ViewsOver != nil
}

// ClickBonusEnabled — считается ли бонус за переходы.
func (t Terms) ClickBonusEnabled() bool { return t.ClickBonusRate != nil && *t.ClickBonusRate > 0 }

// Payment — платёж заказчика.
type Payment struct {
	ID        uuid.UUID   `json:"id"`
	ProjectID uuid.UUID   `json:"project_id"`
	Kind      PaymentKind `json:"kind" enums:"prepayment,final"`
	// Amount — сколько ждём, копейки.
	Amount      int64         `json:"amount"`
	Status      PaymentStatus `json:"status" enums:"awaiting,confirmed,cancelled"`
	Note        string        `json:"note,omitempty"`
	ConfirmedBy *uuid.UUID    `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time    `json:"confirmed_at,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
}

// Accrual — начисление креатору за месяц.
type Accrual struct {
	ID            uuid.UUID `json:"id"`
	ProjectID     uuid.UUID `json:"project_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	// PeriodStart — начало периода, которому принадлежит начисление.
	// Периоды катятся от первой публикации проекта, а не по календарю,
	// поэтому это не первое число месяца (см. billing.ProjectPeriod).
	PeriodStart time.Time `json:"period_start"`
	Salary      int64     `json:"salary"`
	// VideosPlanned/VideosDelivered — сколько выкладок стояло и сколько
	// закрыто. Разница — недостача.
	VideosPlanned   int `json:"videos_planned"`
	VideosDelivered int `json:"videos_delivered"`
	// Deduction — вычет за недосданное: недосданные ролики не оплачиваются.
	Deduction int64 `json:"deduction"`
	// Просмотры по ступеням тарифа: ViewsBase — то, что попало под полную
	// ставку (до порога на каждом ролике), ViewsOver — то, что сверх и
	// считается по пониженной. Обе части, потому что по одному итогу
	// потом не разобрать, почему бонус именно такой.
	ViewsTotal int64 `json:"views_total"`
	ViewsBase  int64 `json:"views_base"`
	ViewsOver  int64 `json:"views_over"`
	ViewsBonus int64 `json:"views_bonus"`
	Clicks     int   `json:"clicks"`
	ClickBonus int64 `json:"click_bonus"`
	// Total — счёт заказчику за этого креатора.
	Total int64 `json:"total"`

	// Payout* — что получает сам креатор. Считается теми же правилами, но
	// по его ставкам; при незаданной креаторской стороне тарифа совпадает
	// со счётом. Раскладка своя, потому что объяснять «почему вышло
	// столько» креатору надо его числами, а не клиентскими.
	PayoutSalary     int64 `json:"payout_salary"`
	PayoutDeduction  int64 `json:"payout_deduction"`
	PayoutViewsBonus int64 `json:"payout_views_bonus"`
	PayoutClickBonus int64 `json:"payout_click_bonus"`
	PayoutTotal      int64 `json:"payout_total"`

	Status AccrualStatus `json:"status" enums:"draft,approved,paid"`
	// Priority — каким по приоритету человек попал в подборку. Поля нет
	// вовсе, если проект заведён руками, а не вырос из заказа: «команда
	// собрана по вашему приоритету» — это про него. Отсюда omitempty:
	// приоритета «ноль» не бывает, бывает его отсутствие.
	Priority     int        `json:"priority,omitempty" extensions:"x-omitempty"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	PaidAt       *time.Time `json:"paid_at,omitempty"`
	CalculatedAt time.Time  `json:"calculated_at"`
	// IsPreview — строка посчитана на лету и в базе её нет. Так выглядит
	// месяц, который ещё не пересчитывали: цифры по фактам уже есть, и
	// прятать их за кнопкой «Пересчитать» значит показывать заказчику
	// ноль там, где на самом деле счёт. Утвердить и выплатить такую
	// строку нельзя — сперва её сохраняет пересчёт.
	IsPreview bool `json:"is_preview,omitempty" extensions:"x-omitempty"`
}

// UTMLink — метка креатора в проекте. Ставит менеджер.
type UTMLink struct {
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	URL           string    `json:"url"`
	// Clicks — заполняется снаружи, из аналитики. Пока бонус за переходы
	// выключен, число справочное.
	Clicks    int       `json:"clicks"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectBilling — всё про деньги проекта одним ответом: по частям это
// три запроса на один экран.
type ProjectBilling struct {
	Terms    Terms     `json:"terms"`
	Payments []Payment `json:"payments"`
	// Accruals — за запрошенный месяц. Пусто, пока не считали.
	//
	// Заказчик их тоже видит: в макете это «Команда месяца» — кто сколько
	// роликов сдал и во сколько это ему обошлось. Он за них и платит,
	// поэтому строка «60 000 + 5 850» — его счёт, а не чужие данные.
	Accruals []Accrual `json:"accruals"`
	UTM      []UTMLink `json:"utm,omitempty"`
	// Totals — итог периода. Считается из тех же строк, но на сервере:
	// «оклады 180 000 + бонус 24 678 · 746 ₽ за 1000» показывают и
	// менеджеру, и заказчику, и складывать это дважды на двух экранах
	// значит получить два разных числа.
	Totals PeriodTotals `json:"totals"`
	// Period — какой период показан и в каком он состоянии. Отсюда же
	// понятно, почему строки помечены «предварительно».
	Period ProjectPeriod `json:"period"`
}

// PeriodTotals — сводка по месяцу.
type PeriodTotals struct {
	Salaries   int64 `json:"salaries"`
	Deductions int64 `json:"deductions"`
	ViewsBonus int64 `json:"views_bonus"`
	ClickBonus int64 `json:"click_bonus"`
	// Total — сколько выставлено заказчику; Payouts — сколько должны
	// креаторам; Margin — разница, то есть что остаётся платформе.
	// При незаданной креаторской стороне тарифа Payouts равен Total, а
	// Margin нулевой: платформа ничего не удерживает.
	Total   int64 `json:"total"`
	Payouts int64 `json:"payouts"`
	Margin  int64 `json:"margin"`
	// Videos/VideosDelivered — сколько выкладок стояло и сколько закрыто.
	// «по 6 роликам» в шапке считается отсюда.
	Videos          int   `json:"videos"`
	VideosDelivered int   `json:"videos_delivered"`
	Views           int64 `json:"views"`
	// CostPer1000 — во сколько обошлась тысяча просмотров: (оклады +
	// бонусы) ÷ просмотры. nil, пока просмотров нет — делить не на что.
	CostPer1000 *int64 `json:"cost_per_1000,omitempty"`
}

// CreatorEarnings — «мой заработок» у креатора: по каким условиям и
// сколько вышло. Чужих цифр здесь нет.
type CreatorEarnings struct {
	// Terms — его сторона тарифа. Клиентской цены здесь нет: см.
	// SideTerms в client_view.go.
	Terms SideTerms `json:"terms"`
	// Accruals — мои начисления по всем периодам проекта, свежие первыми.
	Accruals []CreatorAccrual `json:"accruals"`
	UTM      *UTMLink         `json:"utm,omitempty"`
	// Period — текущий период проекта. nil, пока не вышел ни один ролик:
	// периоды отсчитываются от первой публикации, и до неё периода нет.
	Period *CreatorPeriod `json:"period,omitempty"`
	// NextStep — что даст следующая ступень: сколько просмотров до неё и
	// сколько денег они принесут. Прогноз, а не начисленное.
	//
	// nil, если у проекта пустой тариф (ни оклада, ни ставки) или
	// периода ещё нет: ноль читался бы как «ступень ничего не даст», а
	// это неправда.
	NextStep *NextStepForecast `json:"next_step_forecast,omitempty"`
	// Benchmark — обезличенный ориентир проекта: медиана просмотров
	// зрелых роликов и место человека относительно неё.
	//
	// nil, пока в проекте мало зрелых роликов или они принадлежат
	// одному-двум людям: тогда «медиана проекта» — это показатель
	// соседа, а не агрегат. Своих данных у креатора может не хватать на
	// расчёт, но чужие ему видеть нечего, и второе важнее первого.
	Benchmark *ProjectBenchmark `json:"benchmark,omitempty"`
	// Periods — все периоды проекта, от первого к последнему.
	//
	// Здесь же, а не отдельной ручкой: строки начислений приходят по
	// всем периодам сразу, и у каждой из них фронту нужны границы. Без
	// списка он считал бы их сам — прибавлял месяц к period_start, — то
	// есть держал бы второе описание правила периода.
	Periods []CreatorPeriod `json:"periods"`
}
