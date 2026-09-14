package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/ratings"
)

// ErrInvalidInput — некорректные данные запроса.
var ErrInvalidInput = errors.New("invalid input")

type Service struct {
	repo *Repo
	// scales — справочник порогов оценок. Из него берётся «типичный
	// ролик», когда считать не по чему: число версионируется вместе с
	// остальными порогами, и константы в коде для него больше нет.
	scales *ratings.Repo
}

func NewService(repo *Repo) *Service {
	return &Service{repo: repo, scales: ratings.NewRepo(repo.db)}
}

// ---- доступ ----
//
// Проверки те же, что в остальных доменах, и повторены здесь намеренно:
// тянуть publications в billing ради трёх запросов значило бы связать два
// домена в одну сторону без нужды.

// ManagerHasAccess — проект есть и он этого менеджера. uuid.Nil = админ,
// ему доступно всё.
func (r *Repo) ManagerHasAccess(ctx context.Context, projectID, managerID uuid.UUID) error {
	var assigned *uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT assigned_to_user_id FROM projects WHERE id = $1`, projectID).Scan(&assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check manager access: %w", err)
	}
	if managerID == uuid.Nil || assigned == nil || *assigned == managerID {
		return nil
	}
	return ErrNotFound
}

// ClientOwnsProject — проект принадлежит этому заказчику.
func (r *Repo) ClientOwnsProject(ctx context.Context, projectID, clientID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND client_user_id = $2)`,
		projectID, clientID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check project owner: %w", err)
	}
	return exists, nil
}

// CreatorInProject — пользователь в действующем составе проекта.
func (r *Repo) CreatorInProject(ctx context.Context, projectID, creatorID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_creators
    WHERE project_id = $1 AND creator_user_id = $2 AND removed_at IS NULL
)`, projectID, creatorID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check creator membership: %w", err)
	}
	return exists, nil
}

func (s *Service) ManagerHasAccess(ctx context.Context, projectID, managerID uuid.UUID) error {
	return s.repo.ManagerHasAccess(ctx, projectID, managerID)
}

func (s *Service) ClientOwnsProject(ctx context.Context, projectID, clientID uuid.UUID) (bool, error) {
	return s.repo.ClientOwnsProject(ctx, projectID, clientID)
}

func (s *Service) CreatorInProject(ctx context.Context, projectID, creatorID uuid.UUID) (bool, error) {
	return s.repo.CreatorInProject(ctx, projectID, creatorID)
}

// ---- условия ----

// Terms — условия проекта.
func (s *Service) Terms(ctx context.Context, projectID uuid.UUID) (Terms, error) {
	return s.repo.Terms(ctx, projectID)
}

// SaveTerms — задать условия. Ставки не бывают отрицательными; ноль —
// бывает и означает «не платим по этой строке».
func (s *Service) SaveTerms(ctx context.Context, t Terms, actor uuid.UUID) (Terms, error) {
	if _, err := checkRates(t); err != nil {
		return Terms{}, err
	}
	return s.repo.SaveTerms(ctx, t, actor)
}

// checkRates — проверка ставок, общая для условий проекта и для прайса
// площадки. Правила у них одни, и две копии этих проверок разошлись бы:
// админ выпустил бы прайс, который проект принять не может.
func checkRates(t Terms) (Terms, error) {
	for _, v := range []*int64{t.CreatorSalaryPerMonth, t.CreatorRatePer1000Views, t.CreatorRatePer1000ViewsOver} {
		if v != nil && *v < 0 {
			return Terms{}, fmt.Errorf("%w: ставки креатора не бывают отрицательными", ErrInvalidInput)
		}
	}
	if t.VideosFirstMonth < 0 || t.VideosNextMonths < 0 {
		return Terms{}, fmt.Errorf("%w: объём роликов не бывает отрицательным", ErrInvalidInput)
	}
	if t.SalaryPerMonth < 0 || t.RatePer1000Views < 0 ||
		t.RatePer1000ViewsOver < 0 || t.BonusViewsThreshold < 0 ||
		t.ClickBonusThreshold < 0 {
		return Terms{}, fmt.Errorf("%w: ставки не бывают отрицательными", ErrInvalidInput)
	}
	if (t.ClickBonusRate != nil && *t.ClickBonusRate < 0) ||
		(t.ClickBonusRateOver != nil && *t.ClickBonusRateOver < 0) {
		return Terms{}, fmt.Errorf("%w: ставка за переходы не бывает отрицательной", ErrInvalidInput)
	}

	// Ступени. Пустые поля — это «версия по старой модели», а не ноль;
	// проверяем только заполненное.
	for _, v := range []*int64{
		t.StepViews, t.FirstPeriodFee, t.BaseFee, t.StepFee, t.StepTier2From,
		t.StepFeeOver, t.StepCapViews, t.GuaranteeViews,
		t.CreatorFirstPeriodFee, t.CreatorBaseFee, t.CreatorStepFee, t.CreatorStepFeeOver,
	} {
		if v != nil && *v < 0 {
			return Terms{}, fmt.Errorf("%w: ступени тарифа не бывают отрицательными", ErrInvalidInput)
		}
	}
	if t.StepViews != nil && *t.StepViews == 0 {
		// Ступень нулевого размера — это деление на ноль в расчёте.
		// Хотели «без ступеней» — не присылайте поле вовсе.
		return Terms{}, fmt.Errorf(
			"%w: размер ступени не бывает нулевым — уберите поле, если тариф без ступеней", ErrInvalidInput)
	}
	if t.Stepped() {
		if t.StepCapViews != nil && t.StepTier2From != nil &&
			*t.StepCapViews > 0 && *t.StepTier2From > *t.StepCapViews {
			return Terms{}, fmt.Errorf(
				"%w: ступень дешевеет позже, чем тариф перестаёт её считать", ErrInvalidInput)
		}
	}
	return t, nil
}

// AdoptLatestTerms — снять условия с действующей версии правил. Именно
// снять, а не сослаться: прайс потом поменяют, а проект досчитывается
// по тем числам, по которым начинался.
func (s *Service) AdoptLatestTerms(ctx context.Context, projectID, actor uuid.UUID) (Terms, error) {
	t, err := s.repo.LatestTerms(ctx)
	if err != nil {
		return Terms{}, err
	}
	t.ProjectID = projectID
	return s.repo.SaveTerms(ctx, t, actor)
}

// ---- платежи ----

// SetPayment — сколько ждём предоплатой или по завершении.
func (s *Service) SetPayment(ctx context.Context, projectID uuid.UUID, kind PaymentKind, amount int64, note string, actor uuid.UUID) (Payment, error) {
	if kind != PaymentPrepayment && kind != PaymentFinal {
		return Payment{}, fmt.Errorf("%w: kind must be prepayment or final", ErrInvalidInput)
	}
	if amount < 0 {
		return Payment{}, fmt.Errorf("%w: amount cannot be negative", ErrInvalidInput)
	}
	return s.repo.UpsertPayment(ctx, projectID, kind, amount, strings.TrimSpace(note), actor)
}

// ConfirmPayment — кнопка менеджера «деньги пришли». Провайдера нет:
// подтверждение — это утверждение человека, и оно поэтому именное.
func (s *Service) ConfirmPayment(ctx context.Context, projectID uuid.UUID, kind PaymentKind, actor uuid.UUID) (Payment, error) {
	if kind != PaymentPrepayment && kind != PaymentFinal {
		return Payment{}, fmt.Errorf("%w: kind must be prepayment or final", ErrInvalidInput)
	}
	return s.repo.ConfirmPayment(ctx, projectID, kind, actor)
}

// ---- начисления ----

// Recalculate — пересчитать начисления проекта за период.
//
// Считается по фактам, а не по вводу руками: ролики, вышедшие в границах
// периода, их статус и просмотры, собранные ежедневным сбором.
// Утверждённые и выплаченные строки не трогаются — цифра, по которой уже
// перевели деньги, задним числом не меняется.
func (s *Service) Recalculate(ctx context.Context, projectID uuid.UUID, p ProjectPeriod) ([]Accrual, error) {
	terms, err := s.repo.Terms(ctx, projectID)
	if err != nil {
		return nil, err
	}
	facts, err := s.periodFacts(ctx, projectID, p, viewsThreshold(terms))
	if err != nil {
		return nil, err
	}

	if terms.Stepped() {
		// Вход периода — это выход предыдущего: долг перед клиентом и
		// перенесённый остаток креатора. Подтягиваем перед счётом, иначе
		// период посчитается по устаревшему входу.
		p, err = s.repo.SyncCarryIn(ctx, p)
		if err != nil {
			return nil, err
		}
		rows, left, err := s.steppedAccruals(ctx, terms, facts, projectID, p)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			if err := s.repo.SaveAccrual(ctx, a); err != nil {
				return nil, err
			}
		}
		// Долг и перенос — величины периода: сохраняем их рядом с ним, а
		// не в строках начисления. Подытог их заморозит вместе со срезом.
		if err := s.repo.SaveLeftovers(ctx, p.ID, left); err != nil {
			return nil, err
		}
		start := p.StartsOn
		return s.repo.Accruals(ctx, projectID, &start, nil)
	}

	for _, f := range facts {
		a := calcAccrual(terms, f, projectID, p.StartsOn)
		if err := s.repo.SaveAccrual(ctx, a); err != nil {
			return nil, err
		}
	}
	start := p.StartsOn
	return s.repo.Accruals(ctx, projectID, &start, nil)
}

// steppedAccruals — начисления периода по ступенчатому тарифу.
//
// Тариф ступенчатый ПО ПЕРИОДУ: фикс платится за период, а ступени
// берутся от общего объёма — сумма ступеней по креаторам порознь дала бы
// другое число, и контрольные точки оферты перестали бы сходиться.
// Поэтому считаем период целиком одной calcPeriod, а потом раскладываем
// сумму по строкам креаторов пропорционально их просмотрам.
//
// Раскладка — это подача, а не правило тарифа: сумма строк равна сумме
// периода копейка в копейку, остаток от деления достаётся тому, у кого
// больше просмотров.
func (s *Service) steppedAccruals(
	ctx context.Context, terms Terms, facts []creatorPeriod, projectID uuid.UUID, p ProjectPeriod,
) ([]Accrual, periodLeftovers, error) {
	pc := periodContext{
		Seq:            p.Seq,
		ClientDebtIn:   p.ClientDebtIn,
		CreatorCarryIn: p.CarryInCreator,
	}
	agg := aggregateFacts(facts)
	total, left := calcPeriod(terms, agg, pc, projectID, p.StartsOn)

	rows := make([]Accrual, 0, len(facts))
	if len(facts) == 0 {
		return rows, left, nil
	}

	// Кому достанется остаток от деления: тому, у кого больше просмотров.
	// Произвольный выбор здесь был бы не страшен для суммы, но менял бы
	// строки от пересчёта к пересчёту.
	biggest := 0
	for i := range facts {
		if facts[i].ViewsTotal > facts[biggest].ViewsTotal {
			biggest = i
		}
	}

	// Раздано по строкам — по каждой части раскладки отдельно.
	var givenClient, givenCreator struct{ salary, tail int64 }
	for i, f := range facts {
		a := Accrual{
			ProjectID:       projectID,
			CreatorUserID:   f.CreatorID,
			PeriodStart:     p.StartsOn,
			VideosPlanned:   f.Planned,
			VideosDelivered: f.Delivered,
			ViewsTotal:      f.ViewsTotal,
			ViewsBase:       f.ViewsBase,
			ViewsOver:       f.ViewsOver,
			Clicks:          f.Clicks,
		}
		share := func(sum int64) int64 {
			if agg.ViewsTotal <= 0 {
				// Просмотров нет вовсе — делим поровну: фикс за период
				// платится и тогда, когда ничего не набрали.
				return sum / int64(len(facts))
			}
			return sum * f.ViewsTotal / agg.ViewsTotal
		}
		a.Salary = share(total.Salary)
		a.ViewsBonus = share(total.ViewsBonus)
		a.Total = a.Salary + a.ViewsBonus
		a.PayoutSalary = share(total.PayoutSalary)
		a.PayoutViewsBonus = share(total.PayoutViewsBonus)
		a.PayoutTotal = a.PayoutSalary + a.PayoutViewsBonus
		givenClient.salary += a.Salary
		givenClient.tail += a.ViewsBonus
		givenCreator.salary += a.PayoutSalary
		givenCreator.tail += a.PayoutViewsBonus
		rows = append(rows, a)
		_ = i
	}
	// Остаток от деления — тому, у кого больше просмотров. По каждой
	// части отдельно: свалить всё в фикс значило бы показать в раскладке
	// хвост, которого столько не было.
	rows[biggest].Salary += total.Salary - givenClient.salary
	rows[biggest].ViewsBonus += total.ViewsBonus - givenClient.tail
	rows[biggest].Total = rows[biggest].Salary + rows[biggest].ViewsBonus
	rows[biggest].PayoutSalary += total.PayoutSalary - givenCreator.salary
	rows[biggest].PayoutViewsBonus += total.PayoutViewsBonus - givenCreator.tail
	rows[biggest].PayoutTotal = rows[biggest].PayoutSalary + rows[biggest].PayoutViewsBonus
	return rows, left, nil
}

// aggregateFacts — факты периода одной строкой: ступени считаются от
// общего объёма, а не по каждому креатору порознь.
func aggregateFacts(facts []creatorPeriod) creatorPeriod {
	var agg creatorPeriod
	for _, f := range facts {
		agg.Planned += f.Planned
		agg.Delivered += f.Delivered
		agg.ViewsTotal += f.ViewsTotal
		agg.ViewsBase += f.ViewsBase
		agg.ViewsOver += f.ViewsOver
		agg.Clicks += f.Clicks
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
	}

	// Счёт заказчику и выплата креатору считаются ОДНИМ И ТЕМ ЖЕ
	// правилом, просто по разным ставкам: у тарифа две стороны. Пока
	// креаторская сторона не задана, стороны совпадают и маржи нет.
	client := money(t, f)
	a.Salary, a.Deduction, a.ViewsBonus, a.ClickBonus, a.Total =
		client.salary, client.deduction, client.viewsBonus, client.clickBonus, client.total

	creator := money(t.CreatorSide(), f)
	a.PayoutSalary, a.PayoutDeduction, a.PayoutViewsBonus, a.PayoutClickBonus, a.PayoutTotal =
		creator.salary, creator.deduction, creator.viewsBonus, creator.clickBonus, creator.total

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
	billable := stepPool - pc.ClientDebtIn
	if billable < 0 {
		billable = 0
	}
	// Недоставленная часть долга остаётся долгом.
	if pc.ClientDebtIn > stepPool {
		left.ClientDebtOut = pc.ClientDebtIn - stepPool
	}

	client := t.ClientLadder()
	guarantee := derefOr(t.GuaranteeViews, 0)
	charged := billable
	if pc.Seq > 1 && guarantee > 0 && billable < guarantee {
		// Недобрали гарантию: период оплачивается как гарантия, а
		// недостающие просмотры уходят в долг и гасятся из следующего.
		// Долг в ПРОСМОТРАХ, а не в деньгах — так в оферте: «недостающее
		// доберём бесплатно».
		charged = guarantee
		left.ClientDebtOut += guarantee - billable
	}
	clientFee, _ := client.Fee(pc.Seq, charged)
	clientTail := client.Tail(f.ViewsOver)
	a.Salary = clientFee
	a.ViewsBonus = clientTail
	a.Total = clientFee + clientTail

	// Неполная ступень клиенту не выставляется и НЕ переносится: решение
	// владельца продукта, зафиксировано намеренно — это не баг.

	// ---- креатор ----
	//
	// Та же пара правил его ставками. У него остаток ступени, наоборот,
	// переносится: считаем от ступенчатого объёма периода плюс то, что
	// пришло из прошлого.
	creator := t.CreatorLadder()
	counted := stepPool + pc.CreatorCarryIn
	creatorFee, steps := creator.Fee(pc.Seq, counted)
	creatorTail := creator.Tail(f.ViewsOver)
	a.PayoutSalary = creatorFee
	a.PayoutViewsBonus = creatorTail
	a.PayoutTotal = creatorFee + creatorTail
	if pc.Seq > 1 && creator.StepViews > 0 {
		paid := steps * creator.StepViews
		if counted > paid {
			left.CreatorCarryOut = counted - paid
		}
	}
	return a, left
}

// amounts — раскладка одной стороны тарифа.
type amounts struct {
	salary, deduction, viewsBonus, clickBonus, total int64
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
	if f.Planned > 0 {
		a.salary = t.SalaryPerMonth
		// Недосданное не оплачивается: вычитаем долю невыполненного.
		// Пропорционально, а не «всё или ничего»: сдавший 11 роликов из
		// 12 сделал работу, а не провалил её.
		if f.Delivered < f.Planned {
			missing := int64(f.Planned - f.Delivered)
			a.deduction = t.SalaryPerMonth * missing / int64(f.Planned)
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

	a.total = a.salary - a.deduction + a.viewsBonus + a.clickBonus
	return a
}

// Accruals — начисления проекта. month nil = все месяцы.
func (s *Service) Accruals(ctx context.Context, projectID uuid.UUID, month *time.Time, creatorID *uuid.UUID) ([]Accrual, error) {
	return s.repo.Accruals(ctx, projectID, month, creatorID)
}

// ApproveAccrual — кнопка «утвердить период».
func (s *Service) ApproveAccrual(ctx context.Context, accrualID, actor uuid.UUID) (Accrual, error) {
	return s.repo.DecideAccrual(ctx, accrualID, actor, AccrualApproved)
}

// MarkAccrualPaid — кнопка «выплачено». Только после утверждения.
func (s *Service) MarkAccrualPaid(ctx context.Context, accrualID, actor uuid.UUID) (Accrual, error) {
	return s.repo.DecideAccrual(ctx, accrualID, actor, AccrualPaid)
}

// ---- UTM ----

func (s *Service) UTM(ctx context.Context, projectID uuid.UUID, creatorID *uuid.UUID) ([]UTMLink, error) {
	return s.repo.UTM(ctx, projectID, creatorID)
}

// SaveUTM — метку ставит менеджер.
func (s *Service) SaveUTM(ctx context.Context, projectID, creatorID uuid.UUID, link string, actor uuid.UUID) (UTMLink, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return UTMLink{}, fmt.Errorf("%w: url is required", ErrInvalidInput)
	}
	if len(link) > 2000 {
		return UTMLink{}, fmt.Errorf("%w: url is too long", ErrInvalidInput)
	}
	// Метка уезжает в интерфейс кликабельной ссылкой, поэтому схема
	// проверяется явно.
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return UTMLink{}, fmt.Errorf("%w: url must be an http(s) link", ErrInvalidInput)
	}
	return s.repo.SaveUTM(ctx, projectID, creatorID, link, actor)
}

// ---- сборные ответы ----

// ProjectBilling — весь денежный экран менеджера за один запрос.
//
// seq — какой период показать; seq <= 0 означает текущий.
func (s *Service) ProjectBilling(ctx context.Context, projectID uuid.UUID, seq int, now time.Time) (ProjectBilling, error) {
	var out ProjectBilling
	period, err := s.Period(ctx, projectID, seq, now)
	if err != nil {
		return out, err
	}
	out.Period = period
	if out.Terms, err = s.repo.Terms(ctx, projectID); err != nil {
		return out, err
	}
	if out.Payments, err = s.repo.Payments(ctx, projectID); err != nil {
		return out, err
	}
	if out.Accruals, err = s.accrualsOrPreview(ctx, projectID, period, out.Terms); err != nil {
		return out, err
	}
	if out.UTM, err = s.repo.UTM(ctx, projectID, nil); err != nil {
		return out, err
	}
	out.Totals = totals(out.Accruals)
	return out, nil
}

// accrualsOrPreview — строки периода: сохранённые, а если их ещё нет —
// посчитанные на лету по тем же правилам.
//
// Раньше непересчитанный период выглядел нулём: у заказчика «к оплате
// 0 ₽» при вышедшем ролике на три миллиона просмотров. Данные для счёта
// при этом были все — не было только нажатой кнопки.
//
// «Предварительно» означает «период ещё идёт», а не «строк в базе нет».
// Разница не косметическая: пересчитанный, но не подытоженный период —
// это сохранённые строки, которые завтра станут другими, и показывать
// их как окончательные нельзя.
func (s *Service) accrualsOrPreview(
	ctx context.Context, projectID uuid.UUID, p ProjectPeriod, terms Terms,
) ([]Accrual, error) {
	start := p.StartsOn
	saved, err := s.repo.Accruals(ctx, projectID, &start, nil)
	if err != nil {
		return nil, err
	}
	if len(saved) > 0 {
		if !p.IsLocked() {
			for i := range saved {
				saved[i].IsPreview = true
			}
		}
		return saved, nil
	}
	// Тарифа нет — считать не по чему, и это не ошибка: у проекта могли
	// ещё не заполнить условия.
	if !terms.Stepped() && terms.SalaryPerMonth == 0 && terms.RatePer1000Views == 0 {
		return saved, nil
	}
	facts, err := s.periodFacts(ctx, projectID, p, viewsThreshold(terms))
	if err != nil {
		return nil, err
	}
	if terms.Stepped() {
		// Предпросмотр считается тем же кодом, что и настоящий пересчёт,
		// просто без записи: своя формула «для показа» разошлась бы с
		// будущим счётом молча — а именно этот экран человек и увидит
		// первым.
		rows, _, err := s.steppedAccruals(ctx, terms, facts, projectID, p)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			rows[i].IsPreview = true
		}
		return rows, nil
	}
	out := make([]Accrual, 0, len(facts))
	for _, f := range facts {
		a := calcAccrual(terms, f, projectID, p.StartsOn)
		// Строка, которой нет в базе, предварительна всегда: её просто
		// ещё не пересчитали.
		a.IsPreview = true
		out = append(out, a)
	}
	return out, nil
}

// periodFacts — числа периода: живые, пока период идёт, и из среза,
// когда он подытожен.
//
// Развилка ровно одна и ровно здесь. Разложи её по вызывающим — и
// какой-нибудь экран однажды посчитает подытоженный период по живым
// просмотрам, то есть покажет то, чего в этом периоде не было.
func (s *Service) periodFacts(ctx context.Context, projectID uuid.UUID, p ProjectPeriod, threshold int64) ([]creatorPeriod, error) {
	if p.IsLocked() {
		return s.repo.periodFactsLocked(ctx, projectID, p, threshold)
	}
	return s.repo.periodFacts(ctx, projectID, p, threshold)
}

// totals — сводка месяца. Считается на сервере, потому что показывается
// и менеджеру, и заказчику: сложенное дважды на двух экранах — это два
// разных числа при первой же правке.
func totals(items []Accrual) PeriodTotals {
	var t PeriodTotals
	for _, a := range items {
		t.Salaries += a.Salary
		t.Deductions += a.Deduction
		t.ViewsBonus += a.ViewsBonus
		t.ClickBonus += a.ClickBonus
		t.Total += a.Total
		t.Payouts += a.PayoutTotal
		t.Views += a.ViewsTotal
		t.Videos += a.VideosPlanned
		t.VideosDelivered += a.VideosDelivered
	}
	t.Margin = t.Total - t.Payouts
	if t.Views > 0 {
		per := t.Total * 1000 / t.Views
		t.CostPer1000 = &per
	}
	return t
}

// ClientBilling — что видит заказчик: условия, платежи и состав периода.
//
// Начисления креаторам он видит тоже. Сначала я их спрятал как «чужие
// данные», и это была ошибка: заказчик за эту команду платит, и строка
// «Маша · 1 ролик · 65 000 просмотров · 60 000 + 5 850» — его счёт, а не
// чужая зарплата. Скрыто от него другое — UTM-метки: они рабочий
// инструмент менеджера.
//
// Собирается в собственный тип, а не в общий ProjectBilling. Раньше
// ручка отдавала менеджерскую структуру целиком, и заказчику уезжали
// выплаты креаторам, маржа площадки и креаторская сторона тарифа —
// фронт их просто не рисовал. Вёрстка не граница доступа: вкладки
// «Сеть» в браузере достаточно, чтобы всё это прочитать.
func (s *Service) ClientBilling(ctx context.Context, projectID uuid.UUID, seq int, now time.Time) (ClientBillingView, error) {
	var out ClientBillingView
	period, err := s.Period(ctx, projectID, seq, now)
	if err != nil {
		return out, err
	}
	out.Period = clientPeriodView(period)
	terms, err := s.repo.Terms(ctx, projectID)
	if err != nil {
		return out, err
	}
	out.Terms = terms.ClientTerms()
	if out.Payments, err = s.repo.Payments(ctx, projectID); err != nil {
		return out, err
	}
	accruals, err := s.accrualsOrPreview(ctx, projectID, period, terms)
	if err != nil {
		return out, err
	}
	out.Accruals = make([]ClientAccrual, 0, len(accruals))
	for _, a := range accruals {
		out.Accruals = append(out.Accruals, clientAccrual(a))
	}
	// Итог считаем по тем же строкам, что и менеджеру, и только потом
	// отбрасываем наши деньги: два экрана не должны складывать
	// по-разному.
	out.Totals = clientTotals(totals(accruals))
	return out, nil
}

// CreatorEarnings — «мой заработок». Только свои строки: чужих цифр
// креатор не видит нигде, и здесь тоже.
func (s *Service) CreatorEarnings(ctx context.Context, projectID, creatorID uuid.UUID, now time.Time) (CreatorEarnings, error) {
	out := CreatorEarnings{Periods: []CreatorPeriod{}}
	terms, err := s.repo.Terms(ctx, projectID)
	if err != nil {
		return out, err
	}
	// Креатору показываем ЕГО сторону тарифа. Что за него платит клиент —
	// не его дело и коммерческая тайна платформы; до этой правки он видел
	// именно цену клиента и считал её своим заработком.
	out.Terms = terms.CreatorTerms()

	accruals, err := s.repo.Accruals(ctx, projectID, nil, &creatorID)
	if err != nil {
		return out, err
	}
	// Клиентских чисел в ответе нет ни под каким именем: раньше они
	// затирались его значениями, и достаточно было забыть затереть
	// очередное новое поле.
	out.Accruals = make([]CreatorAccrual, 0, len(accruals))
	for _, a := range accruals {
		out.Accruals = append(out.Accruals, creatorAccrual(a))
	}
	links, err := s.repo.UTM(ctx, projectID, &creatorID)
	if err != nil {
		return out, err
	}
	if len(links) > 0 {
		out.UTM = &links[0]
	}

	// «Типичный ролик» и обезличенный ориентир проекта. Лесенку выбора
	// (своя история → проект → значение по умолчанию) целиком проходит
	// репозиторий; порог обезличивания там же.
	//
	// Значение по умолчанию берём из справочника порогов той версии, на
	// которой стоит проект: в коде его больше нет, иначе правка числа
	// молча переписала бы прошлое.
	// ForProject заодно снимает копию действующей версии, если проект
	// ещё не на шкале: первый вопрос об оценке и есть момент, когда он
	// на неё встаёт.
	scale, serr := s.scales.ForProject(ctx, projectID)
	if serr != nil {
		return out, serr
	}
	fallback := scale.TypicalVideoViews
	if out.Benchmark, err = s.repo.ProjectBenchmark(ctx, projectID, creatorID, now, fallback); err != nil {
		return out, err
	}

	// Что даст следующая ступень. Считается тем же кодом, что и само
	// начисление: отдельная формула «для прогноза» разошлась бы с
	// фактической выплатой молча.
	if out.NextStep, err = s.nextStepForecast(ctx, projectID, creatorID, terms, now); err != nil {
		return out, err
	}

	// Периоды — его стороной. Пока не вышел ни один ролик, периодов нет
	// вовсе, и это не ошибка: у креатора просто пустой кабинет.
	periods, err := s.Periods(ctx, projectID, now)
	if err != nil && !errors.Is(err, ErrNoPeriods) {
		return out, err
	}
	for _, p := range periods {
		out.Periods = append(out.Periods, creatorPeriodView(p))
		if p.Contains(now) {
			cur := creatorPeriodView(p)
			out.Period = &cur
		}
	}
	// Проект мог давно ничего не выкладывать: тогда «текущий» — это
	// последний заведённый период, а не пустота.
	if out.Period == nil && len(out.Periods) > 0 {
		last := out.Periods[len(out.Periods)-1]
		out.Period = &last
	}
	return out, nil
}

// nextStepForecast — сколько просмотров осталось до следующей ступени и
// сколько денег они принесут креатору.
//
// Прогноз считается разницей двух прогонов ОДНОЙ И ТОЙ ЖЕ функции
// расчёта: сколько ему причитается сейчас и сколько причиталось бы при
// выросших просмотрах. Поэтому он не может разойтись с фактической
// выплатой из-за отдельной формулы — расходиться будет только там, где
// реальность разойдётся с допущением о росте (см. ниже).
//
// nil, если считать не по чему: у проекта нет периодов или пустой тариф.
// Ноль в этом случае читался бы как «ступень ничего не даст», а это
// другое утверждение.
func (s *Service) nextStepForecast(
	ctx context.Context, projectID, creatorID uuid.UUID, terms Terms, now time.Time,
) (*NextStepForecast, error) {
	if !terms.Stepped() && terms.SalaryPerMonth == 0 && terms.RatePer1000Views == 0 {
		return nil, nil
	}
	period, err := s.Period(ctx, projectID, 0, now)
	if errors.Is(err, ErrNoPeriods) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	facts, err := s.periodFacts(ctx, projectID, period, viewsThreshold(terms))
	if err != nil {
		return nil, err
	}
	var mine creatorPeriod
	found := false
	for _, f := range facts {
		if f.CreatorID == creatorID {
			mine, found = f, true
			break
		}
	}
	if !found {
		// Он в проекте, но не в составе периода: считать нечего.
		return nil, nil
	}

	// Перенесённый остаток уже в счёте — значит и до ступени с ним ближе.
	//
	// Ступень берём из условий проекта, если тариф ступенчатый: число
	// сто тысяч переехало из константы в версию условий. Константа
	// остаётся запасным значением для проектов на старых версиях, где
	// ступеней в тарифе нет вовсе, а полосу рисовать всё равно надо.
	step := StepViews
	if terms.Stepped() {
		step = *terms.StepViews
	}
	// Считаем от того объёма, который вообще попадает в ступени. При
	// ступенчатом тарифе это просмотры ДО порога на ролик: виральный
	// хвост оплачивается отдельной ставкой и ступень не приближает —
	// показывать обратное значило бы обещать креатору ступень за
	// просмотры, которые в неё не идут.
	pool := mine.ViewsTotal
	if terms.Stepped() {
		pool = mine.ViewsBase
	}
	counted := pool + period.CarryInCreator
	toGo := step - counted%step

	// Куда лягут будущие просмотры — в полную ставку или в пониженную,
	// зависит от того, на каком ролике они наберутся. Раскладываем их в
	// той же пропорции, в какой стоят нынешние: это лучшее, что можно
	// сказать заранее. Если прирост случится на ролике, уже
	// перешагнувшем порог, факт окажется ниже — потому это и прогноз,
	// а не обещание.
	grown := mine
	grown.ViewsTotal += toGo
	switch {
	case terms.Stepped():
		// Ступень набирается только просмотрами до порога на ролик:
		// прирост, который её закрывает, по определению идёт туда же.
		grown.ViewsBase += toGo
	case mine.ViewsTotal <= 0:
		// Просмотров ещё нет, делить нечего: первые идут по полной
		// ставке — так же, как посчитал бы их расчёт.
		grown.ViewsBase += toGo
	default:
		base := toGo * mine.ViewsBase / mine.ViewsTotal
		grown.ViewsBase += base
		grown.ViewsOver += toGo - base
	}

	var gain int64
	if terms.Stepped() {
		// Ступенчатый тариф считается по периоду целиком, поэтому и
		// прогноз — по нему же: одна функция на счёт, выплату и прогноз.
		pc := periodContext{
			Seq:            period.Seq,
			ClientDebtIn:   period.ClientDebtIn,
			CreatorCarryIn: period.CarryInCreator,
		}
		aggFacts, err := s.periodFacts(ctx, projectID, period, viewsThreshold(terms))
		if err != nil {
			return nil, err
		}
		agg := aggregateFacts(aggFacts)
		grownAgg := agg
		grownAgg.ViewsTotal += toGo
		grownAgg.ViewsBase += toGo
		nowPeriod, _ := calcPeriod(terms, agg, pc, projectID, period.StartsOn)
		thenPeriod, _ := calcPeriod(terms, grownAgg, pc, projectID, period.StartsOn)
		gain = thenPeriod.PayoutTotal - nowPeriod.PayoutTotal
	} else {
		nowAccrual := calcAccrual(terms, mine, projectID, period.StartsOn)
		thenAccrual := calcAccrual(terms, grown, projectID, period.StartsOn)
		gain = thenAccrual.PayoutTotal - nowAccrual.PayoutTotal
	}
	if gain < 0 {
		// Отрицательный прогноз означал бы, что рост просмотров
		// уменьшает выплату: такого в тарифе нет, и показывать это
		// человеку нельзя.
		gain = 0
	}
	return &NextStepForecast{
		StepViews:       step,
		ViewsToGo:       toGo,
		CarryInIncluded: period.CarryInCreator,
		ForecastPayout:  gain,
	}, nil
}
