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
	// Заказчик, ведущий менеджер или админ. Менеджеру и админу это
	// нужно ради «посмотреть глазами заказчика»: клиентский экран
	// тянет свои ручки, и без этого кнопка вела в «Проект не найден».
	// Больше, чем они и так видят, тут не открывается — меньше:
	// клиентская сторона не показывает ни маржи, ни выплат.
	err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM projects p
    WHERE p.id = $1 AND (
        p.client_user_id = $2
     OR p.assigned_to_user_id = $2
     OR EXISTS (SELECT 1 FROM users u WHERE u.id = $2 AND u.is_admin)
    )
)`, projectID, clientID).Scan(&exists)
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
	saved, err := s.repo.SaveTerms(ctx, t, actor)
	if err != nil {
		return Terms{}, err
	}
	// Новый тариф действует с ТЕКУЩЕГО периода — и виден сразу.
	//
	// Суммы начислений хранятся строками, а не считаются на лету: без
	// пересчёта менеджер сохранял лесенку и видел на экране прежние
	// деньги, пока кто-нибудь не нажмёт «Пересчитать». Из этого он
	// делал единственно возможный вывод — что правка не сохранилась.
	//
	// Подытоженный период не трогаем ВООБЩЕ: он заморожен вместе со
	// срезом просмотров, по нему выставлен счёт, и переписать его задним
	// числом значило бы поменять то, по чему уже рассчитались. Правка
	// тарифа — договорённость на будущее, а не пересмотр прошлого.
	if err := s.recalcOpenPeriod(ctx, t.ProjectID, time.Now()); err != nil {
		return saved, err
	}
	return saved, nil
}

// recalcOpenPeriod — пересчитать текущий период, если он ещё открыт.
//
// Молча пропускает проект, у которого периодов нет вовсе (не вышло ни
// одного ролика — считать нечего) и подытоженный период.
func (s *Service) recalcOpenPeriod(ctx context.Context, projectID uuid.UUID, now time.Time) error {
	p, err := s.Period(ctx, projectID, 0, now)
	if errors.Is(err, ErrNoPeriods) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.IsLocked() {
		return nil
	}
	_, err = s.Recalculate(ctx, projectID, p)
	return err
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
	if t.StepViews != nil && t.StepFee != nil {
		if t.StepCapViews != nil && t.StepTier2From != nil &&
			*t.StepCapViews > 0 && *t.StepTier2From > *t.StepCapViews {
			return Terms{}, fmt.Errorf(
				"%w: ступень дешевеет позже, чем тариф перестаёт её считать", ErrInvalidInput)
		}
	}

	// Лесенка произвольной длины.
	//
	// Два порога с одним объёмом — не «уточнение», а неразрешимая
	// неоднозначность: какую цену брать, решал бы порядок строк. То же
	// самое проверяет уникальный индекс в базе, но отказ оттуда приходит
	// кодом 23505, а человеку нужно предложение.
	seen := make(map[int64]bool, len(t.Steps))
	for _, st := range t.Steps {
		if st.FromViews < 0 || st.ClientFee < 0 || (st.CreatorFee != nil && *st.CreatorFee < 0) {
			return Terms{}, fmt.Errorf("%w: ступени тарифа не бывают отрицательными", ErrInvalidInput)
		}
		if seen[st.FromViews] {
			return Terms{}, fmt.Errorf(
				"%w: две ступени с одним порогом — непонятно, по какой считать", ErrInvalidInput)
		}
		seen[st.FromViews] = true
		// Креатору нельзя обещать больше, чем берём с клиента: это не
		// тариф, а убыток на каждом периоде, и почти всегда — опечатка.
		if st.CreatorFee != nil && *st.CreatorFee > st.ClientFee {
			return Terms{}, fmt.Errorf(
				"%w: оклад креатора на ступени больше, чем платит заказчик", ErrInvalidInput)
		}
	}
	if t.SubscriberRate != nil && *t.SubscriberRate < 0 {
		return Terms{}, fmt.Errorf("%w: ставка за подписчика не бывает отрицательной", ErrInvalidInput)
	}
	if t.CreatorSubscriberRate != nil {
		if *t.CreatorSubscriberRate < 0 {
			return Terms{}, fmt.Errorf(
				"%w: ставка за подписчика не бывает отрицательной", ErrInvalidInput)
		}
		if t.SubscriberRate != nil && *t.CreatorSubscriberRate > *t.SubscriberRate {
			return Terms{}, fmt.Errorf(
				"%w: креатору за подписчика больше, чем платит заказчик", ErrInvalidInput)
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
	// Откуда взяты числа — решает periodFacts по статусу периода в этой
	// же структуре. Дальше этот же признак едет в запись: пока мы
	// считали, период мог стать подытоженным, и живым числам поверх
	// замороженных ложиться нельзя (см. SaveAccrual).
	fromSnapshot := p.IsLocked()
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
			if err := s.repo.SaveAccrual(ctx, a, fromSnapshot); err != nil {
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
		if err := s.repo.SaveAccrual(ctx, a, fromSnapshot); err != nil {
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
	// Ступени — величина ПЕРИОДА и делятся между людьми по вкладу;
	// подписчики — величина ЧЕЛОВЕКА: их вписывают каждому свои. Делить
	// их по доле просмотров значило бы отдать часть чужого KPI тому, кто
	// его не набирал.
	clientLadder, creatorLadder := terms.ClientLadder(), terms.CreatorLadder()

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
			Subscribers:     f.Subscribers,
		}
		a.SubscriberBonus = clientLadder.Subscribers(f.Subscribers)
		a.PayoutSubscriberBonus = creatorLadder.Subscribers(f.Subscribers)
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
		a.Total = a.Salary + a.ViewsBonus + a.SubscriberBonus
		a.PayoutSalary = share(total.PayoutSalary)
		a.PayoutViewsBonus = share(total.PayoutViewsBonus)
		a.PayoutTotal = a.PayoutSalary + a.PayoutViewsBonus + a.PayoutSubscriberBonus
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
	rows[biggest].Total = rows[biggest].Salary + rows[biggest].ViewsBonus +
		rows[biggest].SubscriberBonus
	rows[biggest].PayoutSalary += total.PayoutSalary - givenCreator.salary
	rows[biggest].PayoutViewsBonus += total.PayoutViewsBonus - givenCreator.tail
	rows[biggest].PayoutTotal = rows[biggest].PayoutSalary + rows[biggest].PayoutViewsBonus +
		rows[biggest].PayoutSubscriberBonus
	return rows, left, nil
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
	// Ролики считаем сданные: фикс платится за вышедшую работу.
	clientFee, _ := client.Fee(pc.Seq, charged, int64(f.Delivered))
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

	// ---- креатор ----
	//
	// Та же пара правил его ставками. У него остаток ступени, наоборот,
	// переносится: считаем от ступенчатого объёма периода плюс то, что
	// пришло из прошлого.
	creator := t.CreatorLadder()
	// Перенос выключен вместе с клиентским (см. выше). Было:
	//   counted := stepPool + pc.CreatorCarryIn
	counted := stepPool
	creatorFee, steps := creator.Fee(pc.Seq, counted, int64(f.Delivered))
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

// Subscribers — сколько подписчиков записано креаторам за период.
func (s *Service) Subscribers(
	ctx context.Context, projectID uuid.UUID, periodStart time.Time,
) ([]CreatorSubscribers, error) {
	return s.repo.PeriodSubscribers(ctx, projectID, periodStart)
}

// SaveSubscribers — число подписчиков за период вписывает менеджер.
//
// Проверка ровно одна и ровно та, которую нельзя проверить в базе
// осмысленно: отрицательных подписчиков не бывает. Верхней границы нет
// намеренно — придумывать «разумный максимум» для чужого канала мы не
// умеем, а упёршийся в него менеджер не сможет выставить правду.
func (s *Service) SaveSubscribers(
	ctx context.Context, projectID, creatorID uuid.UUID, periodStart time.Time, n int64, actor uuid.UUID,
) (CreatorSubscribers, error) {
	if n < 0 {
		return CreatorSubscribers{}, fmt.Errorf("%w: подписчиков не бывает меньше нуля", ErrInvalidInput)
	}
	return s.repo.SaveSubscribers(ctx, projectID, creatorID, periodStart, n, actor)
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
		return s.withNames(ctx, rows)
	}
	out := make([]Accrual, 0, len(facts))
	for _, f := range facts {
		a := calcAccrual(terms, f, projectID, p.StartsOn)
		// Строка, которой нет в базе, предварительна всегда: её просто
		// ещё не пересчитали.
		a.IsPreview = true
		out = append(out, a)
	}
	return s.withNames(ctx, out)
}

// withNames — подставить имена в строки, посчитанные на лету.
//
// Сохранённые начисления берут имя тем же выражением прямо в запросе, а
// предварительный расчёт приходит из арифметики: в ней людей нет, только
// числа. Менеджер видит предварительный расчёт ПЕРВЫМ — до того, как
// нажмёт «Пересчитать», — и до этой правки видел там «Без имени» напротив
// настоящих сумм. Читается как потерянные данные, а не как «ещё не
// посчитано».
func (s *Service) withNames(ctx context.Context, rows []Accrual) ([]Accrual, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, a := range rows {
		ids = append(ids, a.CreatorUserID)
	}
	cards, err := s.repo.CreatorCards(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		card := cards[rows[i].CreatorUserID]
		if rows[i].CreatorName == "" {
			rows[i].CreatorName = card.Name
		}
		// Портрет и адрес страницы дописываем всегда: в отличие от имени,
		// в предварительной строке их нет вовсе — она собрана из
		// арифметики, а не из запроса с профилями.
		if rows[i].CreatorAvatarURL == "" {
			rows[i].CreatorAvatarURL = card.AvatarURL
		}
		if rows[i].CreatorUsername == "" {
			rows[i].CreatorUsername = card.Username
		}
		// Признак публичности — всегда из карточки: в предварительной
		// строке его нет вовсе, а false по умолчанию значил бы «страницы
		// нет» у всех, кого ещё не пересчитывали.
		rows[i].CreatorProfilePublic = card.Public
	}
	return rows, nil
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
		t.SubscriberBonus += a.SubscriberBonus
		t.Subscribers += a.Subscribers
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
	// Лесенка — тем же накопителем, что и в сводке: одно правило «как
	// раскладывается счёт» на оба экрана. Она появится, только если
	// сойдётся с итогом и просмотрами, которые стоят рядом.
	var ladder tariffLadder
	ladder.add(terms, accruals)
	out.Tariff = ladder.result(out.Totals.Total, out.Totals.Views)
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

	// Идущий период показываем расчётом по фактам, а не пустотой.
	//
	// Сохранённая строка начисления появляется только после пересчёта:
	// его делает менеджер кнопкой либо воркер при подытоге — то есть
	// через две недели после конца периода. Пока период идёт, строки
	// нет, и креатор видел «посчитаем, когда по периоду пройдёт расчёт»
	// — при том что прямо под этой фразой стоят его просмотры и взятые
	// ступени. Просмотры измерены, тариф известен, ступени посчитаны, а
	// деньги показать отказывались: человек читает это как «сколько тебе
	// за это — скажем потом».
	//
	// Заказчику и менеджеру идущий период уже показывался расчётом на
	// лету, и тем же кодом, что считает настоящую выплату. Креатор был
	// единственным, кому не показывали то, что уже посчитано.
	//
	// Опасность «примет предварительное за обещанное» снимается не
	// молчанием, а пометкой: строка приходит с IsPreview, и экран рядом
	// с числом ставит «Предварительно».
	if err := s.fillMissingAccruals(ctx, projectID, creatorID, terms, &out); err != nil {
		return out, err
	}
	return out, nil
}

// fillMissingAccruals — дописать расчёт по периодам, где сохранённой
// строки нет.
//
// Строк может не быть по двум разным причинам, и обе оставляли креатора
// с пустотой там, где всё посчитано.
//
// Идущий период: сохранённая строка появляется только после пересчёта —
// его делает менеджер кнопкой либо воркер при подытоге, то есть через
// две недели после конца периода.
//
// Подытоженный период: пересчёт при подытоге мог не пройти — например,
// период закрыли мимо сервиса. Период при этом заморожен, срез снят, и
// посчитать по нему можно точно так же.
//
// Ничего не перезаписывает: сохранённая строка всегда главнее — по ней
// уже могли утвердить и выплатить, и подменять её пересчётом на лету
// значило бы показать одно, а заплатить другое.
func (s *Service) fillMissingAccruals(
	ctx context.Context, projectID, creatorID uuid.UUID, terms Terms, out *CreatorEarnings,
) error {
	have := make(map[string]bool, len(out.Accruals))
	for _, a := range out.Accruals {
		have[a.PeriodStart.Format("2006-01-02")] = true
	}
	for _, view := range out.Periods {
		if have[view.StartsOn.Format("2006-01-02")] {
			continue
		}
		period, err := s.repo.PeriodBySeq(ctx, projectID, view.Seq)
		if err != nil {
			if errors.Is(err, ErrNoPeriods) {
				continue
			}
			return err
		}
		rows, err := s.accrualsOrPreview(ctx, projectID, period, terms)
		if err != nil {
			return err
		}
		for _, a := range rows {
			if a.CreatorUserID == creatorID {
				out.Accruals = append(out.Accruals, creatorAccrual(a))
				break
			}
		}
	}
	return nil
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

	// Считаем от того объёма, который вообще попадает в ступени. При
	// ступенчатом тарифе это просмотры ДО порога на ролик: виральный
	// хвост оплачивается отдельной ставкой и ступень не приближает —
	// показывать обратное значило бы обещать креатору ступень за
	// просмотры, которые в неё не идут.
	pool := mine.ViewsTotal
	if terms.Stepped() {
		pool = mine.ViewsBase
	}
	// Перенесённый остаток уже в счёте — значит и до ступени с ним ближе.
	counted := pool + period.CarryInCreator

	// Сколько осталось до следующей ступени — и это ДВА разных правила,
	// потому что ступени в двух моделях устроены по-разному.
	//
	// У лесенки порогов (terms_steps) ступени неравной высоты: 0, потом
	// 300 000, потом миллион. «Остатка до полной ступени» там нет вовсе,
	// есть расстояние до следующего порога, и делить с остатком нельзя.
	//
	// У прежней модели ступень — блок одинакового размера, и до неё
	// ровно столько, сколько не хватает до круглого числа. Размер блока
	// живёт в условиях проекта; константа остаётся запасным значением
	// для версий, где ступеней в тарифе нет вовсе, а полосу рисовать всё
	// равно надо.
	var toGo, stepSize int64
	if ladder := terms.CreatorLadder(); ladder.HasSteps() {
		next, ok := ladder.NextFrom(counted)
		if !ok {
			// Верхняя ступень взята: тариф дальше не растёт, и прогноз
			// «ещё немного — и прибавят» был бы неправдой.
			return nil, nil
		}
		toGo = next - counted
		// Высота ИМЕННО ЭТОЙ ступени, а не «размер ступени вообще»:
		// полоса прогресса рисуется от взятого порога до следующего, и
		// на неравных ступенях общего размера не существует.
		stepSize = next - ladder.TakenFrom(counted)
	} else {
		stepSize = StepViews
		if terms.StepViews != nil && *terms.StepViews > 0 {
			stepSize = *terms.StepViews
		}
		toGo = stepSize - counted%stepSize
	}

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
		StepViews:       stepSize,
		ViewsToGo:       toGo,
		CarryInIncluded: period.CarryInCreator,
		ForecastPayout:  gain,
	}, nil
}
