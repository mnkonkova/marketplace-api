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
func TestGuaranteeShortfallBecomesDebtInViews(t *testing.T) {
	terms := steppedTerms()

	short, left := calc(terms, facts(200_000, 0), periodContext{Seq: 2})
	if short.Total != 78_000*rub {
		t.Fatalf("недобор гарантии: %d, ожидалось %d", short.Total, 78_000*rub)
	}
	if left.ClientDebtOut != 100_000 {
		t.Fatalf("долг по гарантии: %d просмотров, ожидалось 100 000", left.ClientDebtOut)
	}

	// Следующий период: 400 000 просмотров минус 100 000 долга — к
	// оплате 300 000, то есть три ступени, а не четыре.
	next, nextLeft := calc(terms, facts(400_000, 0), periodContext{Seq: 3, ClientDebtIn: left.ClientDebtOut})
	if next.Total != 78_000*rub {
		t.Fatalf("период после долга: %d, ожидалось %d", next.Total, 78_000*rub)
	}
	if nextLeft.ClientDebtOut != 0 {
		t.Fatalf("долг не погашен: осталось %d", nextLeft.ClientDebtOut)
	}

	// Долг больше, чем набрали: остаток долга едет дальше, счёт — по
	// гарантии.
	_, tail := calc(terms, facts(50_000, 0), periodContext{Seq: 3, ClientDebtIn: 200_000})
	if tail.ClientDebtOut != 150_000+300_000 {
		t.Fatalf("остаток долга плюс новый недобор: %d, ожидалось %d",
			tail.ClientDebtOut, 150_000+300_000)
	}
}

// TestCreatorRemainderIsCarried — у креатора неполная ступень, наоборот,
// переносится и складывается с просмотрами следующего периода.
func TestCreatorRemainderIsCarried(t *testing.T) {
	terms := steppedTerms()

	// Объёмы выше гарантии: иначе в счёт клиента вмешался бы недобор, и
	// тест проверял бы не то, что заявлено.
	a, left := calc(terms, facts(1_250_000, 0), periodContext{Seq: 2})
	if left.CreatorCarryOut != 50_000 {
		t.Fatalf("перенос креатора: %d, ожидалось 50 000", left.CreatorCarryOut)
	}
	if a.PayoutTotal != 132_000*rub {
		t.Fatalf("выплата за 12 ступеней: %d, ожидалось %d", a.PayoutTotal, 132_000*rub)
	}

	// 1 150 000 своих плюс 50 000 перенесённых — двенадцать ступеней, а
	// не одиннадцать.
	next, nextLeft := calc(terms, facts(1_150_000, 0), periodContext{Seq: 3, CreatorCarryIn: left.CreatorCarryOut})
	if next.PayoutTotal != 132_000*rub {
		t.Fatalf("выплата с переносом: %d, ожидалось %d", next.PayoutTotal, 132_000*rub)
	}
	if nextLeft.CreatorCarryOut != 0 {
		t.Fatalf("перенос после ровных ступеней: %d, ожидалось 0", nextLeft.CreatorCarryOut)
	}

	// А клиенту за те же 1 150 000 выставлено одиннадцать ступеней: его
	// остаток сгорел. Перекос односторонний и намеренный — при равных
	// ставках креатор получает на ступень больше, чем выставлено
	// клиенту, и эта ступень идёт из маржи (см. CreatorLadder).
	if next.Total != 126_000*rub {
		t.Fatalf("счёт клиенту: %d, ожидалось %d", next.Total, 126_000*rub)
	}
	if next.PayoutTotal <= next.Total {
		t.Fatalf("односторонний перенос не виден: выплата %d, счёт %d", next.PayoutTotal, next.Total)
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
