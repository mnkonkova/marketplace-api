package billing

// Арифметика денег — вся в этом файле.
//
// Вынесена из service.go намеренно: там периоды, пересчёты, выборки и
// права доступа, и правила счёта терялись среди них. Здесь только счёт:
// сколько стоит период клиенту, сколько из этого получает креатор и как
// сумма периода раскладывается по людям.
//
// Правило одно на обе стороны — money() и calcPeriod() считают и счёт, и
// выплату одним кодом. Две копии этих формул разъехались бы на первой же
// правке, и разница между «что заплатил клиент» и «что получил креатор»
// стала бы следствием ошибки, а не тарифа.

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"
)

// steppedAccruals — начисления периода по ступенчатому тарифу.
//
// Ступень берётся просмотрами КАЖДОГО КРЕАТОРА, а не общим объёмом
// проекта: «набрал полмиллиона — ступень 2 500 ₽» сказано про человека,
// и трое по полмиллиона стоят трижды по 2 500, а не один раз 5 000 за
// миллион на всех. Решение владельца продукта, сентябрь 2026.
//
// Раньше период считался целиком от агрегата, а сумма раскладывалась по
// людям пропорционально вкладу. Это давало другое число и требовало
// целого аппарата раскладки: доли, остаток от деления, вычитание
// утверждённых строк из цены периода. Теперь строка человека считается
// прямо из его же фактов, и «сумма строк равна цене периода» выполняется
// само собой — потому что цена периода и ЕСТЬ сумма строк.
//
// Утверждённые строки пересчёт не трогает (SaveAccrual пишет только
// черновики), и вычитать их больше не из чего: соседям они ничего не
// должны.
func (s *Service) steppedAccruals(
	_ context.Context, terms Terms, facts []creatorPeriod, projectID uuid.UUID, p ProjectPeriod,
) ([]Accrual, periodLeftovers, error) {
	pc := periodContext{
		Seq:            p.Seq,
		ClientDebtIn:   p.ClientDebtIn,
		CreatorCarryIn: p.CarryInCreator,
	}
	rows := make([]Accrual, 0, len(facts))
	var left periodLeftovers
	for _, f := range facts {
		// Человек, у которого в периоде не было НИЧЕГО — ни поставленных
		// выкладок, ни вышедших, ни просмотров, — строки не получает.
		//
		// Раньше это выходило само: цена считалась от агрегата и
		// делилась по вкладу, а вклад нулевой. Теперь строка считается
		// из его же фактов, и нижняя ступень лесенки («от 0 просмотров»)
		// досталась бы каждому, кто просто числится в составе: проект
		// платил бы её столько раз, сколько людей в нём записано.
		if f.Planned == 0 && f.Delivered == 0 && f.ViewsTotal == 0 && f.Subscribers == 0 {
			continue
		}
		a, l := calcPeriod(terms, f, pc, projectID, p.StartsOn)
		a.CreatorUserID = f.CreatorID
		rows = append(rows, a)
		// Перенос и долг выключены, и складывать тут нечего; когда их
		// включат обратно, остаток станет величиной ЧЕЛОВЕКА, а не
		// периода, и это место придётся переписать вместе с ними.
		_ = l
	}
	return rows, left, nil
}

// atLeastZero — денег меньше нуля не бывает: отрицательная доля в
// раскладке означала бы, что у креатора что-то отнимают.
func atLeastZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// aggregateFacts — факты периода одной строкой: ступени считаются от
// общего объёма, а не по каждому креатору порознь.
func aggregateFacts(facts []creatorPeriod) creatorPeriod {
	var agg creatorPeriod
	for _, f := range facts {
		agg.Planned += f.Planned
		agg.Delivered += f.Delivered
		agg.PlannedAssigned += f.PlannedAssigned
		agg.DeliveredAssigned += f.DeliveredAssigned
		agg.ViewsTotal += f.ViewsTotal
		agg.ViewsBase += f.ViewsBase
		agg.ViewsOver += f.ViewsOver
		agg.Clicks += f.Clicks
		agg.Subscribers += f.Subscribers
	}
	return agg
}

// viewsThreshold — порог просмотров на ролик для SQL. Ноль в условиях
// означает «порога нет», то есть весь объём идёт по полной ставке — в
// запросе это выражается недостижимо большим порогом, а не нулём: ноль
// отправил бы все просмотры в пониженную ступень.
func viewsThreshold(t Terms) int64 {
	if t.BonusViewsThreshold <= 0 {
		return math.MaxInt64
	}
	return t.BonusViewsThreshold
}

// periodContext — что расчёт знает о периоде помимо фактов.
//
// Ступенчатый тариф считается по периоду целиком, а не по строке
// креатора: фикс платится за период, а ступени берутся от общего объёма.
// Поэтому расчёту нужен номер периода, долг перед клиентом и
// перенесённый остаток креатора.
type periodContext struct {
	// Seq — какой это период по счёту. Первый оплачивается фиксом: там
	// оплачивается запуск, а не результат.
	Seq int
	// ClientDebtIn — сколько просмотров мы должны клиенту на входе: они
	// не выставляются ему к оплате.
	ClientDebtIn int64
	// CreatorCarryIn — остаток ступени, перенесённый креатору из
	// прошлого периода: складывается с просмотрами этого.
	CreatorCarryIn int64
}

// periodLeftovers — что период оставляет следующему.
//
// Величины периода, а не строки начисления: замораживаются подытогом
// вместе со срезом. Пересчёт задним числом поехал бы цепочкой по уже
// оплаченным периодам.
type periodLeftovers struct {
	// ClientDebtOut — сколько просмотров остались должны клиенту.
	ClientDebtOut int64
	// CreatorCarryOut — остаток, не добравший до полной ступени. У
	// клиента такой остаток сгорает (решение владельца продукта), у
	// креатора переносится.
	CreatorCarryOut int64
}

// calcAccrual — вся арифметика начисления в одном месте, без обращений
// к базе: так её видно целиком и можно проверить таблицей случаев.
func calcAccrual(t Terms, f creatorPeriod, projectID uuid.UUID, period time.Time) Accrual {
	a := Accrual{
		ProjectID:       projectID,
		CreatorUserID:   f.CreatorID,
		PeriodStart:     period,
		VideosPlanned:   f.Planned,
		VideosDelivered: f.Delivered,
		ViewsTotal:      f.ViewsTotal,
		ViewsBase:       f.ViewsBase,
		ViewsOver:       f.ViewsOver,
		Clicks:          f.Clicks,
		Subscribers:     f.Subscribers,
	}

	// Счёт заказчику и выплата креатору считаются ОДНИМ И ТЕМ ЖЕ
	// правилом, просто по разным ставкам: у тарифа две стороны. Пока
	// креаторская сторона не задана, стороны совпадают и маржи нет.
	client := money(t, f)
	a.Salary, a.Deduction, a.ViewsBonus, a.ClickBonus, a.SubscriberBonus, a.Total =
		client.salary, client.deduction, client.viewsBonus, client.clickBonus,
		client.subscriberBonus, client.total

	creator := money(t.CreatorSide(), f)
	a.PayoutSalary, a.PayoutDeduction, a.PayoutViewsBonus, a.PayoutClickBonus,
		a.PayoutSubscriberBonus, a.PayoutTotal =
		creator.salary, creator.deduction, creator.viewsBonus, creator.clickBonus,
		creator.subscriberBonus, creator.total

	return a
}

// calcPeriod — начисление за период целиком по ступенчатому тарифу.
//
// Та же функция для обеих сторон и для прогноза до ступени: разведи их —
// и разница между «что заплатил клиент» и «что получил креатор» станет
// следствием ошибки, а не тарифа.
//
// Возвращает начисление (суммой по периоду) и то, что период оставляет
// следующему: долг перед клиентом и перенесённый остаток креатора.
func calcPeriod(t Terms, f creatorPeriod, pc periodContext, projectID uuid.UUID, period time.Time) (Accrual, periodLeftovers) {
	a := Accrual{
		ProjectID:       projectID,
		PeriodStart:     period,
		VideosPlanned:   f.Planned,
		VideosDelivered: f.Delivered,
		ViewsTotal:      f.ViewsTotal,
		ViewsBase:       f.ViewsBase,
		ViewsOver:       f.ViewsOver,
		Clicks:          f.Clicks,
		Subscribers:     f.Subscribers,
	}
	var left periodLeftovers

	// Порогов в тарифе ДВА, и работают они по очереди.
	//
	// Сначала виральный хвост: просмотры сверх порога НА РОЛИК уже
	// отделены в SQL (f.ViewsOver) и оплачиваются пониженной ставкой за
	// тысячу. В ступени они не идут вовсе — иначе одна виральная удача
	// стоила бы клиенту как месяц работы.
	//
	// Остаток (f.ViewsBase — просмотры до порога, сложенные по всем
	// роликам периода) идёт в месячные ступени: они про рост аккаунта.
	// Одно правило другое не заменяет, «упрощать» их в одно нельзя.
	stepPool := f.ViewsBase

	// ---- клиент ----
	//
	// Долг гасится первым: первые ClientDebtIn просмотров периода мы
	// обещали бесплатно и к оплате не выставляем.
	//
	// Долг и гарантия меряются ступенчатым объёмом, а не общим. На
	// практике это одно и то же число: недобрать гарантию в 300 000 и
	// при этом иметь виральный хвост нельзя — ролик, перешагнувший
	// миллион, сам по себе даёт в ступенчатый объём целый миллион.
	// ПЕРЕНОС ПРОСМОТРОВ И ГАРАНТИЯ ВЫКЛЮЧЕНЫ (решение владельца
	// продукта, сентябрь 2026). Период считается сам по себе: сколько
	// просмотров набрал — столько и стоит.
	//
	// Код оставлен, а не удалён, намеренно: оба правила записаны в
	// оферте и включаются обратно одной правкой. Как было:
	//
	//   billable := stepPool - pc.ClientDebtIn   // долг гасится первым
	//   if billable < 0 { billable = 0 }
	//   if pc.ClientDebtIn > stepPool {          // недогашенный остаток
	//       left.ClientDebtOut = pc.ClientDebtIn - stepPool
	//   }
	//   if pc.Seq > 1 && guarantee > 0 && billable < guarantee {
	//       charged = guarantee                  // платим как за гарантию
	//       left.ClientDebtOut += guarantee - billable
	//   }
	//
	// Вместе с ними выключена и запись остатков: left остаётся пустым,
	// значит client_debt_out/carry_out_creator всегда нули, и цепочка
	// периодов ничего друг другу не передаёт.
	billable := stepPool

	client := t.ClientLadder()
	charged := billable
	// Ролики считаем сданные и ПОСТАВЛЕННЫЕ МЕНЕДЖЕРОМ: фикс платится за
	// вышедшую работу по договорённости. Самодобавленный ролик в фикс не
	// идёт — иначе креатор кнопкой «добавить ролик» выставлял бы
	// заказчику счёт, которого никто не заказывал. Просмотры таких
	// роликов при этом считаются наравне: просмотры есть просмотры.
	clientFee, _ := client.Fee(pc.Seq, charged, int64(f.DeliveredAssigned))
	clientTail := client.Tail(f.ViewsOver)
	// KPI по подписчикам стоит рядом со ступенями, а не внутри них:
	// ступени считаются от просмотров, подписчики — своё число, и
	// сложить их в один объём нельзя.
	clientSubs := client.Subscribers(f.Subscribers)
	a.Salary = clientFee
	a.ViewsBonus = clientTail
	a.SubscriberBonus = clientSubs
	a.Total = clientFee + clientTail + clientSubs

	// Неполная ступень клиенту не выставляется и НЕ переносится: решение
	// владельца продукта, зафиксировано намеренно — это не баг.

	// Вычета за недосдачу здесь нет — и это не пропуск.
	//
	// В прежней модели оклад платили за период целиком, поэтому
	// несданный ролик приходилось вычитать отдельной строкой. Фикс
	// считается ЗА РОЛИК: не вышел — не заплачен, и вычитать уже нечего.
	// Отдельный вычет поверх этого был бы штрафом, которого в тарифе
	// нет, а на экране — двойным наказанием за одно и то же.
	// a.Deduction и a.PayoutDeduction остаются нулями осознанно.

	// ---- креатор ----
	//
	// Та же пара правил его ставками. У него остаток ступени, наоборот,
	// переносится: считаем от ступенчатого объёма периода плюс то, что
	// пришло из прошлого.
	creator := t.CreatorLadder()
	// Перенос выключен вместе с клиентским (см. выше). Было:
	//   counted := stepPool + pc.CreatorCarryIn
	counted := stepPool
	creatorFee, steps := creator.Fee(pc.Seq, counted, int64(f.DeliveredAssigned))
	creatorTail := creator.Tail(f.ViewsOver)
	creatorSubs := creator.Subscribers(f.Subscribers)
	a.PayoutSalary = creatorFee
	a.PayoutViewsBonus = creatorTail
	a.PayoutSubscriberBonus = creatorSubs
	a.PayoutTotal = creatorFee + creatorTail + creatorSubs
	// Перенос неполной ступени — правило ПРЕЖНЕЙ модели, где ступень была
	// блоком в сто тысяч просмотров. У лесенки порогов неполной ступени
	// не существует: есть цена периода на взятом пороге и всё. Перенести
	// «остаток» там значило бы придумать величину, которой нет в тарифе.
	//
	// Выключено вместе с остальным переносом. Было:
	//   if pc.Seq > 1 && !creator.HasSteps() && creator.StepViews > 0 {
	//       paid := steps * creator.StepViews
	//       if counted > paid { left.CreatorCarryOut = counted - paid }
	//   }
	_ = steps
	return a, left
}

// amounts — раскладка одной стороны тарифа.
type amounts struct {
	salary, deduction, viewsBonus, clickBonus, subscriberBonus, total int64
}

// money — вся арифметика периода по одному набору ставок.
//
// Вынесена, чтобы счёт клиенту и выплата креатору считались буквально
// одним кодом: две копии этих правил разъехались бы на первой же правке,
// и разница между «что заплатил клиент» и «что получил креатор» стала бы
// следствием ошибки, а не тарифа.
func money(t Terms, f creatorPeriod) amounts {
	var a amounts

	// Оклад платится за месяц работы. Месяц, на который креатору не
	// поставили ни одной выкладки, работой не был — оклада за него нет.
	//
	// Считаем ТОЛЬКО по выкладкам менеджера. Ролик, который креатор
	// добавил себе сам, работой по договорённости не является: ни оклада
	// он не открывает, ни недосдачи не создаёт. Иначе кнопка «добрать до
	// ступени» отнимала бы у нажавшего часть оклада за ролики, которых
	// ему никто не поручал — человек своими руками сделал бы себе хуже.
	if f.PlannedAssigned > 0 {
		a.salary = t.SalaryPerMonth
		// Недосданное не оплачивается: вычитаем долю невыполненного.
		// Пропорционально, а не «всё или ничего»: сдавший 11 роликов из
		// 12 сделал работу, а не провалил её.
		if f.DeliveredAssigned < f.PlannedAssigned {
			missing := int64(f.PlannedAssigned - f.DeliveredAssigned)
			a.deduction = t.SalaryPerMonth * missing / int64(f.PlannedAssigned)
		}
	}

	// Тариф ступенчатый: до порога — полная ставка, свыше — пониженная.
	// Ставка падает намеренно, чтобы виральный ролик не съедал бюджет
	// клиента. Ступени уже разложены по роликам в periodFacts.
	a.viewsBonus = f.ViewsBase/1000*t.RatePer1000Views +
		f.ViewsOver/1000*t.RatePer1000ViewsOver

	// Переходы устроены так же, но порог у них МЕСЯЧНЫЙ, а не на ролик.
	if t.ClickBonusEnabled() {
		base, over := int64(f.Clicks), int64(0)
		if t.ClickBonusThreshold > 0 && f.Clicks > t.ClickBonusThreshold {
			base = int64(t.ClickBonusThreshold)
			over = int64(f.Clicks - t.ClickBonusThreshold)
		}
		a.clickBonus = base * *t.ClickBonusRate
		if over > 0 && t.ClickBonusRateOver != nil {
			a.clickBonus += over * *t.ClickBonusRateOver
		}
	}

	// Подписчики — третий KPI, и считается он умножением: ступеней по ним
	// владелец продукта не называл. Число приходит не от сборщика, а от
	// менеджера: сборщика подписчиков у нас нет, и выдумывать его под
	// объявленную ставку нельзя.
	if t.SubscriberKPIEnabled() {
		a.subscriberBonus = f.Subscribers * *t.SubscriberRate
	}

	a.total = a.salary - a.deduction + a.viewsBonus + a.clickBonus + a.subscriberBonus
	return a
}
