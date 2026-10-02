package billing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Ступенчатый тариф проверяется здесь, а не в интеграционных тестах,
// потому что вся его арифметика — чистые функции без базы: контрольные
// точки коммерческого предложения должны сходиться ДО РУБЛЯ, и ловить
// расхождение проще там, где между числом и проверкой нет ни SQL, ни
// HTTP.
//
// Суммы везде в копейках, как в базе.

const rub = 100 // копеек в рубле — чтобы контрольные точки читались рублями

// steppedTerms — действующая ступенчатая версия прайса (миграция 00052).
// Числа те же: разойдутся — тест поймает.
func steppedTerms() Terms {
	i := func(v int64) *int64 { return &v }
	return Terms{
		// Виральный хвост: порог на ролик и пониженная ставка за тысячу.
		RatePer1000Views:     60 * rub,
		BonusViewsThreshold:  1_000_000,
		RatePer1000ViewsOver: 6 * rub,

		StepViews:      i(100_000),
		FirstPeriodFee: i(65_000 * rub),
		BaseFee:        i(60_000 * rub),
		StepFee:        i(6_000 * rub),
		StepTier2From:  i(2_000_000),
		StepFeeOver:    i(1_500 * rub),
		StepCapViews:   i(3_000_000),
		GuaranteeViews: i(300_000),
	}
}

// facts — факты периода: сколько просмотров набрали ролики и сколько из
// них ушло за порог на ролик.
func facts(base, over int64) creatorPeriod {
	return creatorPeriod{
		Planned: 30, Delivered: 30,
		ViewsTotal: base + over,
		ViewsBase:  base,
		ViewsOver:  over,
	}
}

func calc(t Terms, f creatorPeriod, pc periodContext) (Accrual, periodLeftovers) {
	return calcPeriod(t, f, pc, uuid.New(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
}

// TestSteppedControlPoints — контрольные точки из коммерческого
// предложения. Все считаются со второго периода и без виральных роликов.
func TestSteppedControlPoints(t *testing.T) {
	terms := steppedTerms()
	cases := []struct {
		name  string
		views int64
		want  int64
	}{
		{"гарантия 300 тыс.", 300_000, 78_000 * rub},
		{"500 тыс.", 500_000, 90_000 * rub},
		{"1 млн", 1_000_000, 120_000 * rub},
		{"2 млн", 2_000_000, 180_000 * rub},
		{"3 млн", 3_000_000, 195_000 * rub},
		// Выше потолка ступени не оплачиваются вовсе: 4 млн стоят столько
		// же, сколько 3 млн.
		{"4 млн — потолок", 4_000_000, 195_000 * rub},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Просмотры набраны роликами до порога: хвоста нет.
			a, _ := calc(terms, facts(c.views, 0), periodContext{Seq: 2})
			if a.Total != c.want {
				t.Fatalf("счёт клиенту = %d, ожидалось %d", a.Total, c.want)
			}
		})
	}
}

// TestSteppedFirstPeriodIsFixed — первый период оплачивается фиксом
// независимо от просмотров: там оплачивается запуск, а не результат.
func TestSteppedFirstPeriodIsFixed(t *testing.T) {
	terms := steppedTerms()
	for _, views := range []int64{0, 140_000, 3_000_000} {
		a, left := calc(terms, facts(views, 0), periodContext{Seq: 1})
		if a.Total != 65_000*rub {
			t.Fatalf("первый период при %d просмотрах: %d, ожидалось %d", views, a.Total, 65_000*rub)
		}
		// Гарантия в первом периоде не работает, долга он не оставляет.
		if left.ClientDebtOut != 0 {
			t.Fatalf("первый период оставил долг %d", left.ClientDebtOut)
		}
		// И остаток ступени из него не переносится.
		if left.CreatorCarryOut != 0 {
			t.Fatalf("первый период перенёс остаток %d", left.CreatorCarryOut)
		}
	}
}

// TestViralTailIsPaidApartFromSteps — два порога тарифа работают по
// очереди и об разном.
//
// Три миллиона просмотров стоят клиенту по-разному в зависимости от
// того, как они набраны: месяцем работы — 195 000, одним виральным
// роликом — 132 000. Ради этой разницы порог на ролик и существует.
func TestViralTailIsPaidApartFromSteps(t *testing.T) {
	terms := steppedTerms()

	// 30 роликов по 100 000: ни один не перешагнул порог.
	work, _ := calc(terms, facts(3_000_000, 0), periodContext{Seq: 2})
	if work.Total != 195_000*rub {
		t.Fatalf("месяц работы: %d, ожидалось %d", work.Total, 195_000*rub)
	}

	// Один ролик на 3 млн: миллион до порога идёт в ступени, два сверх —
	// по 6 ₽ за тысячу и мимо ступеней.
	viral, _ := calc(terms, facts(1_000_000, 2_000_000), periodContext{Seq: 2})
	if viral.Salary != 120_000*rub {
		t.Fatalf("ступени вирального периода: %d, ожидалось %d", viral.Salary, 120_000*rub)
	}
	if viral.ViewsBonus != 12_000*rub {
		t.Fatalf("виральный хвост: %d, ожидалось %d", viral.ViewsBonus, 12_000*rub)
	}
	if viral.Total != 132_000*rub {
		t.Fatalf("счёт за виральный период: %d, ожидалось %d", viral.Total, 132_000*rub)
	}
	if work.Total-viral.Total != 63_000*rub {
		t.Fatalf("разница между работой и виральностью: %d, ожидалось %d",
			work.Total-viral.Total, 63_000*rub)
	}
}

// TestPartialStepIsNotBilledAndNotCarried — неполная ступень клиенту не
// выставляется и НЕ переносится. Решение владельца продукта, а не
// упущение расчёта: тест стоит, чтобы перенос не завели «по аналогии с
// креатором».
func TestPartialStepIsNotBilledAndNotCarried(t *testing.T) {
	terms := steppedTerms()
	a, left := calc(terms, facts(350_000, 0), periodContext{Seq: 2})
	if a.Total != 78_000*rub {
		t.Fatalf("350 тыс. просмотров: %d, ожидалось столько же, сколько 300 тыс. (%d)",
			a.Total, 78_000*rub)
	}
	if left.ClientDebtOut != 0 {
		t.Fatalf("неполная ступень клиента перенеслась: %d", left.ClientDebtOut)
	}
}

// TestGuaranteeShortfallBecomesDebtInViews — недобор гарантии
// оплачивается как гарантия, а недостающие просмотры уходят в долг и
// гасятся из следующего периода. Долг в ПРОСМОТРАХ, а не в деньгах.
func TestGuaranteeOff(t *testing.T) {
	terms := steppedTerms()

	// Гарантия и перенос выключены (сентябрь 2026): период стоит ровно
	// столько, сколько просмотров набрал, и ничего не передаёт дальше.
	// Сам тариф гарантию по-прежнему объявляет — проверяем, что расчёт
	// её больше не слушает.
	if terms.GuaranteeViews == nil || *terms.GuaranteeViews == 0 {
		t.Fatal("тариф теста должен объявлять гарантию, иначе проверять нечего")
	}

	short, left := calc(terms, facts(200_000, 0), periodContext{Seq: 2})
	if short.Total != 72_000*rub {
		t.Fatalf("две ступени по факту: %d, ожидалось %d", short.Total, 72_000*rub)
	}
	if left.ClientDebtOut != 0 {
		t.Fatalf("долг по гарантии выключен, а записан: %d", left.ClientDebtOut)
	}

	// Входящий долг тоже игнорируется: его больше некому было записать,
	// но если он остался в базе от прежней модели — счёт он не меняет.
	withDebt, wl := calc(terms, facts(200_000, 0), periodContext{Seq: 3, ClientDebtIn: 100_000})
	if withDebt.Total != short.Total {
		t.Fatalf("входящий долг повлиял на счёт: %d против %d", withDebt.Total, short.Total)
	}
	if wl.ClientDebtOut != 0 {
		t.Fatalf("долг записан: %d", wl.ClientDebtOut)
	}
}

// TestCreatorRemainderIsCarried — у креатора неполная ступень, наоборот,
// переносится и складывается с просмотрами следующего периода.
func TestCreatorCarryOff(t *testing.T) {
	terms := steppedTerms()

	// Перенос неполной ступени выключен вместе с гарантией: остаток
	// сгорает и у креатора тоже.
	a, left := calc(terms, facts(1_250_000, 0), periodContext{Seq: 2})
	if left.CreatorCarryOut != 0 {
		t.Fatalf("перенос выключен, а записан: %d", left.CreatorCarryOut)
	}

	// Входящий перенос счёт не меняет.
	next, _ := calc(terms, facts(1_250_000, 0), periodContext{Seq: 3, CreatorCarryIn: 50_000})
	if next.PayoutTotal != a.PayoutTotal {
		t.Fatalf("входящий перенос повлиял на выплату: %d против %d",
			next.PayoutTotal, a.PayoutTotal)
	}
}

// TestCreatorTailRateDerivedFromBase — пониженная ставка креатора
// выводится из базовой по правилу «в десять раз ниже», если названа
// только базовая. Пустая означала бы «как у клиента» — то есть отдать
// креатору клиентскую цену вирального хвоста.
func TestCreatorTailRateDerivedFromBase(t *testing.T) {
	i := func(v int64) *int64 { return &v }

	t.Run("из ставки за тысячу", func(t *testing.T) {
		terms := steppedTerms()
		terms.CreatorRatePer1000Views = i(40 * rub)
		if got := terms.CreatorLadder().TailRate; got != 4*rub {
			t.Fatalf("ставка за хвост: %d, ожидалось %d", got, 4*rub)
		}
	})

	t.Run("из ступени", func(t *testing.T) {
		terms := steppedTerms()
		terms.CreatorStepFee = i(3_000 * rub)
		if got := terms.CreatorLadder().TailRate; got != 3*rub {
			t.Fatalf("ставка за хвост: %d, ожидалось %d", got, 3*rub)
		}
	})

	t.Run("названная руками главнее правила", func(t *testing.T) {
		terms := steppedTerms()
		terms.CreatorRatePer1000Views = i(40 * rub)
		terms.CreatorRatePer1000ViewsOver = i(1 * rub)
		if got := terms.CreatorLadder().TailRate; got != 1*rub {
			t.Fatalf("ставка за хвост: %d, ожидалось %d", got, 1*rub)
		}
	})

	t.Run("креаторской стороны нет — как у клиента", func(t *testing.T) {
		terms := steppedTerms()
		if got := terms.CreatorLadder().TailRate; got != terms.RatePer1000ViewsOver {
			t.Fatalf("ставка за хвост: %d, ожидалось %d", got, terms.RatePer1000ViewsOver)
		}
	})
}

// TestCreatorSideCheaperThanClient — при заданной креаторской стороне
// выплата меньше счёта на каждой части: ступенях и хвосте.
func TestCreatorSideCheaperThanClient(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	terms := steppedTerms()
	terms.CreatorBaseFee = i(40_000 * rub)
	terms.CreatorStepFee = i(4_000 * rub)
	terms.CreatorStepFeeOver = i(1_000 * rub)

	a, _ := calc(terms, facts(1_000_000, 2_000_000), periodContext{Seq: 2})
	// Клиент: 60 000 + 10×6 000 + 12 000 хвоста.
	if a.Total != 132_000*rub {
		t.Fatalf("счёт клиенту: %d, ожидалось %d", a.Total, 132_000*rub)
	}
	// Креатор: 40 000 + 10×4 000 + хвост по 4 ₽ (ступень 4 000 ₽ за сто
	// тысяч — это 40 ₽ за тысячу, делённые на десять).
	if a.PayoutSalary != 80_000*rub {
		t.Fatalf("ступени креатора: %d, ожидалось %d", a.PayoutSalary, 80_000*rub)
	}
	if a.PayoutViewsBonus != 8_000*rub {
		t.Fatalf("хвост креатора: %d, ожидалось %d", a.PayoutViewsBonus, 8_000*rub)
	}
	if a.PayoutTotal >= a.Total {
		t.Fatalf("выплата %d не меньше счёта %d — маржи нет", a.PayoutTotal, a.Total)
	}
}

// TestSteppedIsOptIn — версия без ступеней считается по-старому.
// Признак один, и он проверяется здесь: разъедется — половина кода
// посчитает версию ступенчатой, а половина нет.
func TestSteppedIsOptIn(t *testing.T) {
	var old Terms
	old.SalaryPerMonth = 60_000 * rub
	old.RatePer1000Views = 90 * rub
	if old.Stepped() {
		t.Fatal("версия без ступеней считается ступенчатой")
	}
	if !steppedTerms().Stepped() {
		t.Fatal("ступенчатая версия не распознана")
	}
}

// ---- лесенка в сводке заказчика ----
//
// Тарифная лесенка — главный коммерческий аргумент кабинета: «первый
// миллион по 90 ₽, всё сверх — по 9 ₽, отсюда и 56 ₽». Её проверяют
// калькулятором, поэтому проверяем и мы: в копейках и до рубля.

// ladderTerms — условия стенда: 90 ₽ до миллиона на ролик, 9 ₽ сверх,
// оклад 60 000 ₽ за месяц.
func ladderTerms() Terms {
	return Terms{
		SalaryPerMonth:       60_000 * rub,
		RatePer1000Views:     90 * rub,
		BonusViewsThreshold:  1_000_000,
		RatePer1000ViewsOver: 9 * rub,
	}
}

// ladderAccrual — строка начисления с уже разложенными ступенями: так их
// и отдаёт SQL (LEAST/GREATEST по каждому ролику).
func ladderAccrual(base, over int64, t Terms) Accrual {
	a := Accrual{
		Salary:     t.SalaryPerMonth,
		ViewsBase:  base,
		ViewsOver:  over,
		ViewsTotal: base + over,
	}
	a.ViewsBonus = base/1000*t.RatePer1000Views + over/1000*t.RatePer1000ViewsOver
	a.Total = a.Salary + a.ViewsBonus
	return a
}

func TestTariffLadderMatchesTheBill(t *testing.T) {
	terms := ladderTerms()
	a := ladderAccrual(1_000_000, 2_000_000, terms)

	var l tariffLadder
	l.add(terms, []Accrual{a})
	got := l.result(a.Total, a.ViewsTotal)
	if got == nil {
		t.Fatalf("лесенка должна сойтись: total=%d views=%d", a.Total, a.ViewsTotal)
	}
	// Контрольные точки коммерческого предложения — те самые числа,
	// которые стоят на экране заказчика.
	if got.BaseAmount != 90_000*rub {
		t.Errorf("первый миллион: %d, ждали %d", got.BaseAmount, 90_000*rub)
	}
	if got.OverAmount != 18_000*rub {
		t.Errorf("сверх миллиона: %d, ждали %d", got.OverAmount, 18_000*rub)
	}
	if got.Fixed != 60_000*rub {
		t.Errorf("работа команды: %d, ждали %d", got.Fixed, 60_000*rub)
	}
	if got.Total != 168_000*rub {
		t.Errorf("итог: %d, ждали %d", got.Total, 168_000*rub)
	}
	// И главное: лесенка обязана делиться в цену тысячи, которая стоит
	// рядом с ней на экране.
	if cp := costPer1000(got.Total, got.Views); cp == nil || *cp != 56*rub {
		t.Errorf("цена тысячи из лесенки: %v, ждали %d", cp, 56*rub)
	}
}

func TestTariffLadderHiddenWhenItWouldNotAddUp(t *testing.T) {
	terms := ladderTerms()
	a := ladderAccrual(1_000_000, 2_000_000, terms)

	cases := []struct {
		name         string
		build        func(l *tariffLadder)
		money, views int64
	}{{
		// Подытоженный период: его начислений уже нет, в сводке от него
		// только сумма среза. Лесенка покрыла бы часть счёта, а стояла бы
		// рядом с целым — и не сошлась бы.
		name:  "часть счёта осталась за лесенкой",
		build: func(l *tariffLadder) { l.add(terms, []Accrual{a}) },
		money: a.Total + 50_000*rub, views: a.ViewsTotal,
	}, {
		name:  "часть просмотров осталась за лесенкой",
		build: func(l *tariffLadder) { l.add(terms, []Accrual{a}) },
		money: a.Total, views: a.ViewsTotal + 500_000,
	}, {
		// У двух проектов разные ставки: одной лесенкой их не описать, а
		// строка «первый миллион по 90 ₽» была бы неправдой для второго.
		name: "у проектов разные ставки",
		build: func(l *tariffLadder) {
			other := terms
			other.RatePer1000Views = 70 * rub
			l.add(terms, []Accrual{a})
			l.add(other, []Accrual{ladderAccrual(1_000_000, 0, other)})
		},
		money: a.Total, views: a.ViewsTotal,
	}, {
		// Ступенчатая версия условий считается по другим полям — ни
		// оклада, ни ставки за тысячу в её расчёте нет.
		name:  "ступенчатая версия условий",
		build: func(l *tariffLadder) { l.add(steppedTerms(), []Accrual{a}) },
		money: a.Total, views: a.ViewsTotal,
	}, {
		// В счёте есть строка, которой в лесенке нет вовсе (бонус за
		// переходы). Показать лесенку значило бы показать счёт, в котором
		// не хватает слагаемого.
		name: "в счёте есть слагаемое мимо лесенки",
		build: func(l *tariffLadder) {
			withClicks := a
			withClicks.ClickBonus = 5_000 * rub
			withClicks.Total += withClicks.ClickBonus
			l.add(terms, []Accrual{withClicks})
		},
		money: a.Total + 5_000*rub, views: a.ViewsTotal,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var l tariffLadder
			c.build(&l)
			if got := l.result(c.money, c.views); got != nil {
				t.Errorf("лесенку показывать нельзя, а она есть: %+v", got)
			}
		})
	}
}

// Фикс считается ЗА РОЛИК, а не за период.
//
// Прежняя модель платила цену периода независимо от объёма: месяц с
// пятью выкладками стоил столько же, сколько с тридцатью. Теперь фикс —
// цена одного ролика, и он занимает место нижней ступени лесенки: там и
// был фикс за период. Ступени выше нуля не меняются — они про
// просмотры, а не про объём работы.
func TestFeePerVideoReplacesPeriodFee(t *testing.T) {
	terms := steppedTerms()
	fee := int64(1_000 * rub)
	creatorFee := int64(500 * rub)
	terms.FeePerVideo = &fee
	terms.CreatorFeePerVideo = &creatorFee

	// Просмотров мало — до настоящих ступеней не дотянули, платим за
	// ролики. Десять выкладок: клиенту 10 000 ₽, креатору 5 000 ₽.
	f := facts(10_000, 0)
	f.Delivered, f.DeliveredAssigned = 10, 10
	a, _ := calc(terms, f, periodContext{Seq: 2})
	if a.Salary != 10*fee {
		t.Errorf("клиенту за 10 роликов: %d, ожидалось %d", a.Salary, 10*fee)
	}
	if a.PayoutSalary != 10*creatorFee {
		t.Errorf("креатору за 10 роликов: %d, ожидалось %d", a.PayoutSalary, 10*creatorFee)
	}

	// Вдвое меньше роликов — вдвое меньше фикс. В прежней модели цена
	// периода от объёма не зависела вовсе.
	f.Delivered, f.DeliveredAssigned = 5, 5
	half, _ := calc(terms, f, periodContext{Seq: 2})
	if half.Salary != 5*fee {
		t.Errorf("клиенту за 5 роликов: %d, ожидалось %d", half.Salary, 5*fee)
	}

	// Первый период тоже за ролики: цена запуска фиксом больше не
	// назначается.
	first, _ := calc(terms, f, periodContext{Seq: 1})
	if first.Salary != 5*fee {
		t.Errorf("первый период: %d, ожидалось %d", first.Salary, 5*fee)
	}

	// А дотянули до настоящей ступени — платим её цену, а не ролики:
	// ступень и есть цена периода на взятом пороге.
	big := facts(400_000, 0)
	big.Delivered, big.DeliveredAssigned = 10, 10
	step, _ := calc(terms, big, periodContext{Seq: 2})
	if step.Salary == 10*fee {
		t.Errorf("ступень не взяла верх над фиксом за ролик: %d", step.Salary)
	}
}

// Самодобавленный ролик фикса не приносит.
//
// Кнопка «добавить ролик» в кабинете креатора — про добор ступени по
// просмотрам, и платить за неё фикс значило бы дать креатору выставлять
// заказчику счёт, которого никто не заказывал. Просмотры таких роликов
// при этом считаются наравне: просмотры есть просмотры.
func TestFeePerVideoIgnoresSelfAdded(t *testing.T) {
	terms := steppedTerms()
	fee := int64(1_000 * rub)
	terms.FeePerVideo = &fee

	f := facts(10_000, 0)
	// Десять роликов вышло, но поставил менеджер только четыре.
	f.Delivered, f.DeliveredAssigned = 10, 4
	a, _ := calc(terms, f, periodContext{Seq: 2})
	if a.Salary != 4*fee {
		t.Errorf("фикс за самодобавленные: %d, ожидалось %d", a.Salary, 4*fee)
	}
}

// Ступень берётся просмотрами КАЖДОГО КРЕАТОРА, а не общим объёмом.
//
// «Набрал полмиллиона — ступень 2 500 ₽» сказано про человека. Раньше
// период считался от агрегата и раскладывался по людям пропорционально:
// тот, кто не дотянул до порога сам, получал долю ступени, взятой
// соседом, а проект платил за неё один раз вместо двух.
func TestStepIsTakenByEachCreatorSeparately(t *testing.T) {
	terms := Terms{
		// Лесенка порогов: полмиллиона стоит 2 500 ₽, миллион — 5 000 ₽.
		// Между порогами цена не растёт: берётся последняя взятая
		// ступень, а не пропорция.
		Steps: []TermsStep{
			{FromViews: 500_000, ClientFee: 2_500 * rub},
			{FromViews: 1_000_000, ClientFee: 5_000 * rub},
		},
	}

	strong := creatorPeriod{
		Planned: 1, Delivered: 1, PlannedAssigned: 1, DeliveredAssigned: 1,
		ViewsTotal: 900_000, ViewsBase: 900_000,
	}
	weak := creatorPeriod{
		Planned: 1, Delivered: 1, PlannedAssigned: 1, DeliveredAssigned: 1,
		ViewsTotal: 100_000, ViewsBase: 100_000,
	}

	a, _ := calc(terms, strong, periodContext{Seq: 2})
	b, _ := calc(terms, weak, periodContext{Seq: 2})

	// Сильный взял первую ступень сам, слабый не взял ни одной.
	if a.Salary != 2_500*rub {
		t.Errorf("ступень сильного: %d, ожидалось %d", a.Salary, 2_500*rub)
	}
	if b.Salary != 0 {
		t.Errorf("слабый не дотянул до порога, а ему начислили %d", b.Salary)
	}

	// А вместе их объём — миллион, и по прежнему правилу проект платил
	// бы 5 000 ₽ за одну общую ступень. Теперь платит 2 500: ступень
	// взял один человек, и второй раз её никто не брал.
	together, _ := calc(terms, aggregateFacts([]creatorPeriod{strong, weak}),
		periodContext{Seq: 2})
	if a.Salary+b.Salary == together.Salary {
		t.Errorf("сумма строк совпала со ступенью от агрегата (%d) — считаем по-старому",
			together.Salary)
	}
}

// У первого периода левой границы отбора выкладок нет.
//
// Он и начинается с первой выкладки, поэтому «левее начала» означает
// ролик, вышедший или исправленный задним числом уже после заморозки
// якоря. При строгой границе такой ролик проваливался мимо всех
// периодов: за него не платили, его ссылки обходились вечно.
func TestPeriodFromIsOpenOnTheFirstPeriod(t *testing.T) {
	start := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if got := periodFrom(ProjectPeriod{Seq: 1, StartsOn: start}); !got.IsZero() {
		t.Errorf("у первого периода граница %v, ожидалась открытая", got)
	}
	if got := periodFrom(ProjectPeriod{Seq: 2, StartsOn: start}); !got.Equal(start) {
		t.Errorf("у второго периода граница %v, ожидалось %v", got, start)
	}
}

// Подписчики: «за одного» и «ступенями» — две формы, лесенка сильнее.
//
// Поштучная цена на росте в сотню тысяч даёт сумму, которую никто не
// закладывал, — отсюда вторая форма. Смешивать их нельзя: это третье
// правило, которого никто не называл, и расчёт обязан выбрать одно.
func TestSubscriberPriceTwoForms(t *testing.T) {
	perOne := StepLadder{SubscriberRate: 300}
	if got := perOne.Subscribers(1200); got != 360_000 {
		t.Errorf("за одного: %d, ожидалось 1200×300", got)
	}
	if perOne.SubscribersStepped() {
		t.Error("ставка за одного принята за лесенку")
	}

	stepped := StepLadder{
		SubscriberRate: 300, // задана и она — лесенка всё равно сильнее
		SubscriberSteps: []LadderStep{
			{FromViews: 1000, Fee: 500_000},
			{FromViews: 5000, Fee: 1_500_000},
		},
	}
	if !stepped.SubscribersStepped() {
		t.Fatal("лесенка не опознана")
	}
	cases := []struct {
		gained int64
		want   int64
		why    string
	}{
		{0, 0, "не прибавилось — платить не за что"},
		{-700, 0, "убыль: отнимать за отписки владелец не просил"},
		{999, 0, "не дотянул до нижнего порога"},
		{1000, 500_000, "взял первую ступень ровно"},
		{4999, 500_000, "до второй не дотянул — цена первой"},
		{5000, 1_500_000, "взял вторую"},
		{50_000, 1_500_000, "выше верхней ступени тариф не растёт"},
	}
	for _, c := range cases {
		if got := stepped.Subscribers(c.gained); got != c.want {
			t.Errorf("прирост %d: %d, ожидалось %d (%s)", c.gained, got, c.want, c.why)
		}
	}
}

// Лесенка подписчиков сводится к стороне сделки так же, как лесенка
// просмотров: пустая креаторская цена значит «как у заказчика».
func TestSubscriberLadderHasTwoSides(t *testing.T) {
	creatorFee := int64(200_000)
	terms := Terms{
		SubscriberSteps: []TermsStep{
			{FromViews: 5000, ClientFee: 1_000_000, CreatorFee: &creatorFee},
			{FromViews: 1000, ClientFee: 400_000},
		},
	}
	client := terms.ClientLadder()
	// Сортировка по порогу обязательна: расчёт ищет последнюю взятую
	// ступень, и перепутанный порядок молча дал бы не ту цену.
	if len(client.SubscriberSteps) != 2 || client.SubscriberSteps[0].FromViews != 1000 {
		t.Fatalf("клиентская лесенка не отсортирована: %+v", client.SubscriberSteps)
	}
	if got := client.Subscribers(6000); got != 1_000_000 {
		t.Errorf("клиент за 6000 подписчиков: %d", got)
	}
	creator := terms.CreatorLadder()
	if got := creator.Subscribers(6000); got != creatorFee {
		t.Errorf("креатор за 6000 подписчиков: %d, ожидалось %d", got, creatorFee)
	}
	// Нижняя ступень креаторской цены не называет — значит столько же.
	if got := creator.Subscribers(1200); got != 400_000 {
		t.Errorf("креатор на нижней ступени: %d, ожидалось «как у клиента»", got)
	}

	// Сведённый к креатору тариф отдаёт ЕГО цену под тем же полем:
	// иначе кабинет покажет цену клиента и назовёт её заработком.
	side := terms.CreatorSide()
	if len(side.SubscriberSteps) != 2 {
		t.Fatalf("сведённая лесенка: %+v", side.SubscriberSteps)
	}
	for _, st := range side.SubscriberSteps {
		if st.FromViews == 5000 && st.ClientFee != creatorFee {
			t.Errorf("в кабинете креатора под его ступенью стоит %d", st.ClientFee)
		}
	}
}

// Лесенка — объявленная цена наравне со ставкой.
//
// Иначе проект, где KPI задан только ступенями, считался бы «подписчиков
// не считаем»: кабинет не спросил бы число, а расчёт не заплатил бы по
// заданной лесенке.
func TestSubscriberKPIEnabledBySteps(t *testing.T) {
	if (Terms{}).SubscriberKPIEnabled() {
		t.Error("без цены KPI по подписчикам включён")
	}
	steps := Terms{SubscriberSteps: []TermsStep{{FromViews: 1000, ClientFee: 1}}}
	if !steps.SubscriberKPIEnabled() {
		t.Error("лесенка задана, а KPI выключен")
	}
	rate := int64(300)
	if !(Terms{SubscriberRate: &rate}).SubscriberKPIEnabled() {
		t.Error("ставка задана, а KPI выключен")
	}
}
