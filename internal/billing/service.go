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
)

// ErrInvalidInput — некорректные данные запроса.
var ErrInvalidInput = errors.New("invalid input")

type Service struct{ repo *Repo }

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

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

// Recalculate — пересчитать начисления проекта за месяц.
//
// Считается по фактам, а не по вводу руками: выкладки за месяц, их статус
// и просмотры собранные ежедневным сбором. Утверждённые и выплаченные
// строки не трогаются — цифра, по которой уже перевели деньги, задним
// числом не меняется.
func (s *Service) Recalculate(ctx context.Context, projectID uuid.UUID, month time.Time) ([]Accrual, error) {
	terms, err := s.repo.Terms(ctx, projectID)
	if err != nil {
		return nil, err
	}
	facts, err := s.monthFacts(ctx, projectID, month, viewsThreshold(terms))
	if err != nil {
		return nil, err
	}
	period := firstOfMonth(month)
	for _, f := range facts {
		a := calcAccrual(terms, f, projectID, period)
		if err := s.repo.SaveAccrual(ctx, a); err != nil {
			return nil, err
		}
	}
	return s.repo.Accruals(ctx, projectID, &period, nil)
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

// calcAccrual — вся арифметика начисления в одном месте, без обращений
// к базе: так её видно целиком и можно проверить таблицей случаев.
func calcAccrual(t Terms, f creatorPeriod, projectID uuid.UUID, period time.Time) Accrual {
	a := Accrual{
		ProjectID:       projectID,
		CreatorUserID:   f.CreatorID,
		PeriodMonth:     period,
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
func (s *Service) ProjectBilling(ctx context.Context, projectID uuid.UUID, month time.Time) (ProjectBilling, error) {
	var out ProjectBilling
	var err error
	out.PeriodMonth = firstOfMonth(month)
	if out.Terms, err = s.repo.Terms(ctx, projectID); err != nil {
		return out, err
	}
	if out.Payments, err = s.repo.Payments(ctx, projectID); err != nil {
		return out, err
	}
	if out.Accruals, err = s.accrualsOrPreview(ctx, projectID, out.PeriodMonth, out.Terms); err != nil {
		return out, err
	}
	if out.UTM, err = s.repo.UTM(ctx, projectID, nil); err != nil {
		return out, err
	}
	if out.Month, err = s.repo.Month(ctx, projectID, out.PeriodMonth); err != nil {
		return out, err
	}
	out.Totals = totals(out.Accruals)
	return out, nil
}

// accrualsOrPreview — строки месяца: сохранённые, а если их ещё нет —
// посчитанные на лету по тем же правилам.
//
// Раньше непересчитанный месяц выглядел нулём: у заказчика «к оплате
// 0 ₽» при вышедшем ролике на три миллиона просмотров. Данные для счёта
// при этом были все — не было только нажатой кнопки.
//
// «Предварительно» означает «месяц ещё идёт», а не «строк в базе нет».
// Разница не косметическая: пересчитанный, но не зафиксированный месяц —
// это сохранённые строки, которые завтра станут другими, и показывать
// их как окончательные нельзя.
func (s *Service) accrualsOrPreview(
	ctx context.Context, projectID uuid.UUID, period time.Time, terms Terms,
) ([]Accrual, error) {
	m, err := s.repo.Month(ctx, projectID, period)
	if err != nil {
		return nil, err
	}
	saved, err := s.repo.Accruals(ctx, projectID, &period, nil)
	if err != nil {
		return nil, err
	}
	if len(saved) > 0 {
		if !m.IsLocked() {
			for i := range saved {
				saved[i].IsPreview = true
			}
		}
		return saved, nil
	}
	// Тарифа нет — считать не по чему, и это не ошибка: у проекта могли
	// ещё не заполнить условия.
	if terms.SalaryPerMonth == 0 && terms.RatePer1000Views == 0 {
		return saved, nil
	}
	facts, err := s.monthFacts(ctx, projectID, period, viewsThreshold(terms))
	if err != nil {
		return nil, err
	}
	out := make([]Accrual, 0, len(facts))
	for _, f := range facts {
		a := calcAccrual(terms, f, projectID, period)
		// Строка, которой нет в базе, предварительна всегда: её просто
		// ещё не пересчитали.
		a.IsPreview = true
		out = append(out, a)
	}
	return out, nil
}

// monthFacts — числа месяца: живые, пока месяц идёт, и из среза, когда
// он зафиксирован.
//
// Развилка ровно одна и ровно здесь. Разложи её по вызывающим — и
// какой-нибудь экран однажды посчитает зафиксированный месяц по живым
// просмотрам, то есть покажет то, чего в этом месяце не было.
func (s *Service) monthFacts(ctx context.Context, projectID uuid.UUID, month time.Time, threshold int64) ([]creatorPeriod, error) {
	m, err := s.repo.Month(ctx, projectID, month)
	if err != nil {
		return nil, err
	}
	if m.IsLocked() {
		return s.repo.periodFactsLocked(ctx, projectID, month, threshold)
	}
	return s.repo.periodFacts(ctx, projectID, month, threshold)
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

// ClientBilling — что видит заказчик: условия, платежи и состав месяца.
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
func (s *Service) ClientBilling(ctx context.Context, projectID uuid.UUID, month time.Time) (ClientBillingView, error) {
	out := ClientBillingView{PeriodMonth: firstOfMonth(month)}
	terms, err := s.repo.Terms(ctx, projectID)
	if err != nil {
		return out, err
	}
	out.Terms = terms.ClientTerms()
	if out.Payments, err = s.repo.Payments(ctx, projectID); err != nil {
		return out, err
	}
	accruals, err := s.accrualsOrPreview(ctx, projectID, out.PeriodMonth, terms)
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
	if out.Month, err = s.repo.Month(ctx, projectID, out.PeriodMonth); err != nil {
		return out, err
	}
	return out, nil
}

// CreatorEarnings — «мой заработок». Только свои строки: чужих цифр
// креатор не видит нигде, и здесь тоже.
func (s *Service) CreatorEarnings(ctx context.Context, projectID, creatorID uuid.UUID) (CreatorEarnings, error) {
	var out CreatorEarnings
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
	return out, nil
}
