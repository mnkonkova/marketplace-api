package billing

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/publications"
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
	// stats — кто умеет обойти ролики проекта и снять их с обхода.
	// Реализуется publications.Service. nil = подытог идёт как раньше:
	// по тем цифрам, которые успели собраться.
	stats StatsCollector
}

// StatsCollector — сбор просмотров, нужный подытогу.
//
// Интерфейсом, а не прямой зависимостью: billing не знает про выкладки
// и ссылки, и тянуть publications целиком ради двух вызовов значило бы
// связать домены в одну сторону без нужды. Тот же приём, что у
// WithChecklistAttacher в publications.
type StatsCollector interface {
	// ForceRefreshProject — обойти все живые ссылки проекта, не
	// спрашивая о свежести. Зовётся ДО снятия среза.
	//
	// Снятие роликов с обхода после подытога сюда не входит: это делает
	// parkPeriodLinks в той же транзакции, что и срез, — и правильно,
	// потому что «ролик этого периода» определяет срез, а не сбор.
	ForceRefreshProject(ctx context.Context, projectID uuid.UUID, now time.Time) (publications.CollectStats, error)
}

// WithStats — подключить сбор к подытогу.
//
// Без него подытог работает по-прежнему: срез снимается с тех цифр,
// которые успели собраться к последнему заходу в кабинет. Это рабочее
// состояние для api-процесса — подытог по расписанию делает воркер, и
// сборщик нужен ему.
func (s *Service) WithStats(c StatsCollector) *Service {
	s.stats = c
	return s
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
	// Фикс за ролик. Отрицательного не бывает, и креатору нельзя обещать
	// за ролик больше, чем берём за него с заказчика: это не тариф, а
	// убыток на каждой выкладке, и почти всегда просто описка.
	if t.FeePerVideo != nil && *t.FeePerVideo < 0 {
		return Terms{}, fmt.Errorf("%w: фикс за ролик не бывает отрицательным", ErrInvalidInput)
	}
	if t.CreatorFeePerVideo != nil {
		if *t.CreatorFeePerVideo < 0 {
			return Terms{}, fmt.Errorf(
				"%w: фикс креатору за ролик не бывает отрицательным", ErrInvalidInput)
		}
		if t.FeePerVideo != nil && *t.CreatorFeePerVideo > *t.FeePerVideo {
			return Terms{}, fmt.Errorf(
				"%w: креатору за ролик обещано больше, чем платит заказчик", ErrInvalidInput)
		}
	}
	// Стоимость проекта, названная менеджером. Ноль — «не назвали», и
	// это рабочее состояние; отрицательная — описка, и молча записать её
	// значило бы показать заказчику отрицательный СПВ.
	if t.ProjectCost < 0 {
		return Terms{}, fmt.Errorf("%w: стоимость проекта не бывает отрицательной", ErrInvalidInput)
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
	if err := checkLadder(t.Steps, "просмотрам"); err != nil {
		return Terms{}, err
	}
	// Лесенка подписчиков проверяется теми же правилами и своим
	// сообщением: «две ступени с одним порогом» на экране, где рядом
	// стоят две лесенки, не отвечает, которую править.
	if err := checkLadder(t.SubscriberSteps, "подписчикам"); err != nil {
		return Terms{}, err
	}
	if t.SubscriberRate != nil && *t.SubscriberRate < 0 {
		return Terms{}, fmt.Errorf("%w: ставка за подписчика не бывает отрицательной", ErrInvalidInput)
	}
	// Ставка «за одного» вместе с лесенкой — два правила счёта на один
	// KPI. Расчёт выбрал бы лесенку (она сильнее), но менеджер, вписавший
	// оба, имел в виду что-то одно, и угадывать за него нельзя.
	if len(t.SubscriberSteps) > 0 && t.SubscriberRate != nil && *t.SubscriberRate > 0 {
		return Terms{}, fmt.Errorf(
			"%w: по подписчикам задана и цена за одного, и ступени — оставьте одно", ErrInvalidInput)
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
	ctx context.Context, projectID uuid.UUID, p ProjectPeriod,
) ([]CreatorSubscribers, error) {
	return s.repo.PeriodSubscribers(ctx, projectID, p)
}

// DropSubscribers — убрать ручную правку: доплата снова считается по
// снятому обходом.
func (s *Service) DropSubscribers(
	ctx context.Context, projectID, creatorID uuid.UUID, periodStart time.Time,
) error {
	return s.repo.DropSubscribers(ctx, projectID, creatorID, periodStart)
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
	//
	// Объём — ЕГО СОБСТВЕННЫЙ: ступень берётся просмотрами человека, а
	// не общим объёмом проекта. «Набрал полмиллиона — ступень» сказано
	// про него, и считать расстояние до неё по чужим роликам значило бы
	// обещать прибавку, которой он не получит.
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
		// Ступень берётся ЕГО просмотрами, поэтому и прогноз считается
		// по его же фактам — тем же кодом, что и настоящая выплата.
		// Своя формула «для прогноза» разошлась бы с ней молча.
		pc := periodContext{
			Seq:            period.Seq,
			ClientDebtIn:   period.ClientDebtIn,
			CreatorCarryIn: period.CarryInCreator,
		}
		nowRow, _ := calcPeriod(terms, mine, pc, projectID, period.StartsOn)
		thenRow, _ := calcPeriod(terms, grown, pc, projectID, period.StartsOn)
		gain = thenRow.PayoutTotal - nowRow.PayoutTotal
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

// checkLadder — общие правила обеих лесенок тарифа.
//
// Правила у ступеней просмотров и подписчиков одни: порог неотрицателен,
// порог уникален, креатору не обещано больше, чем берём с клиента. Вид
// попадает в текст ошибки, потому что на экране лесенки две, и
// «две ступени с одним порогом» без вида не отвечает, которую править.
//
// Уникальность проверяет и индекс в базе, но оттуда отказ приходит кодом
// 23505, а человеку нужно предложение.
func checkLadder(steps []TermsStep, what string) error {
	seen := make(map[int64]bool, len(steps))
	for _, st := range steps {
		if st.FromViews < 0 || st.ClientFee < 0 || (st.CreatorFee != nil && *st.CreatorFee < 0) {
			return fmt.Errorf("%w: ступени по %s не бывают отрицательными", ErrInvalidInput, what)
		}
		if seen[st.FromViews] {
			return fmt.Errorf("%w: две ступени по %s с одним порогом — непонятно, по какой считать",
				ErrInvalidInput, what)
		}
		seen[st.FromViews] = true
		// Креатору нельзя обещать больше, чем берём с клиента: это не
		// тариф, а убыток на каждом периоде, и почти всегда — опечатка.
		if st.CreatorFee != nil && *st.CreatorFee > st.ClientFee {
			return fmt.Errorf("%w: креатору на ступени по %s больше, чем платит заказчик",
				ErrInvalidInput, what)
		}
	}
	return nil
}
