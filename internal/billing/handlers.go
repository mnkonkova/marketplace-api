package billing

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// errorResponse — тело ошибки. error — машинный код, message — готовый
// русский текст для интерфейса.
type errorResponse struct {
	Error string `json:"error"`
	// Message пустой у ручек, отдающих ошибку без текста.
	Message string `json:"message,omitempty"`
}

// termsReq — тариф. Суммы в копейках.
//
// Ставка ступенчатая: rate_per_1000_views до порога на ролик,
// rate_per_1000_views_over — свыше. У переходов порог месячный.
type termsReq struct {
	SalaryPerMonth int64 `json:"salary_per_month"`
	// За какой объём назван оклад: «30 видео первый месяц, 60 со второго».
	VideosFirstMonth     int   `json:"videos_first_month"`
	VideosNextMonths     int   `json:"videos_next_months"`
	RatePer1000Views     int64 `json:"rate_per_1000_views"`
	BonusViewsThreshold  int64 `json:"bonus_views_threshold"`
	RatePer1000ViewsOver int64 `json:"rate_per_1000_views_over"`
	// null в ставке — это «переходы не считаем», в отличие от нуля,
	// который значил бы заданную нулевую ставку. Помечено для спеки:
	// без пометки читающий только swagger отправит 0.
	ClickBonusRate      *int64 `json:"click_bonus_rate" extensions:"x-nullable"`
	ClickBonusThreshold int    `json:"click_bonus_threshold"`
	ClickBonusRateOver  *int64 `json:"click_bonus_rate_over" extensions:"x-nullable"`

	// Креаторская сторона: что получает исполнитель. null = «столько же,
	// сколько платит клиент», то есть платформа ничего не удерживает.
	CreatorSalaryPerMonth       *int64 `json:"creator_salary_per_month" extensions:"x-nullable"`
	CreatorRatePer1000Views     *int64 `json:"creator_rate_per_1000_views" extensions:"x-nullable"`
	CreatorRatePer1000ViewsOver *int64 `json:"creator_rate_per_1000_views_over" extensions:"x-nullable"`

	// ---- ступенчатый тариф ----
	//
	// Заполненный step_views означает, что версия считается ступенями, а
	// оклад и ставка за тысячу в расчёт не идут — кроме вирального
	// хвоста, который считается по rate_per_1000_views_over и порогу на
	// ролик выше. null во всех этих полях = версия по старой модели;
	// поэтому именно null, а не 0: ноль значил бы «ступень нулевого
	// размера».
	StepViews      *int64 `json:"step_views" extensions:"x-nullable"`
	FirstPeriodFee *int64 `json:"first_period_fee" extensions:"x-nullable"`
	BaseFee        *int64 `json:"base_fee" extensions:"x-nullable"`
	StepFee        *int64 `json:"step_fee" extensions:"x-nullable"`
	StepTier2From  *int64 `json:"step_tier2_from" extensions:"x-nullable"`
	StepFeeOver    *int64 `json:"step_fee_over" extensions:"x-nullable"`
	StepCapViews   *int64 `json:"step_cap_views" extensions:"x-nullable"`
	GuaranteeViews *int64 `json:"guarantee_views" extensions:"x-nullable"`

	// Креаторская сторона ступеней. null = «как у клиента».
	CreatorFirstPeriodFee *int64 `json:"creator_first_period_fee" extensions:"x-nullable"`
	CreatorBaseFee        *int64 `json:"creator_base_fee" extensions:"x-nullable"`
	CreatorStepFee        *int64 `json:"creator_step_fee" extensions:"x-nullable"`
	CreatorStepFeeOver    *int64 `json:"creator_step_fee_over" extensions:"x-nullable"`

	// Steps — лесенка произвольной длины: порог объёма и цена периода на
	// нём. Приходит целиком и целиком же заменяет прежнюю: ступень
	// удаляют не реже, чем добавляют, и «обнови присланное» оставило бы
	// удалённую ступень жить в расчёте.
	//
	// Непустая лесенка отменяет step_views и цену ступени выше: это
	// другое правило счёта, и смешивать их в одном периоде нельзя.
	Steps []TermsStep `json:"steps"`

	// SubscriberRate — сколько платит заказчик за подписчика за период.
	// null = KPI по подписчикам не считаем вовсе (в отличие от нуля,
	// который значил бы объявленную нулевую ставку). Само число
	// подписчиков вписывает менеджер: сборщика по ним нет.
	SubscriberRate *int64 `json:"subscriber_rate" extensions:"x-nullable"`
	// CreatorSubscriberRate — сколько из этого получает креатор.
	// null = «как у клиента».
	CreatorSubscriberRate *int64 `json:"creator_subscriber_rate" extensions:"x-nullable"`
}

// terms — запрос в условия. Одним местом на обе ручки (менеджер правит
// условия проекта, админ выпускает версию прайса): разложи это по двум
// хендлерам — и новое поле однажды доедет только до одного из них.
func (req termsReq) terms() Terms {
	return Terms{
		SalaryPerMonth:       req.SalaryPerMonth,
		VideosFirstMonth:     req.VideosFirstMonth,
		VideosNextMonths:     req.VideosNextMonths,
		RatePer1000Views:     req.RatePer1000Views,
		BonusViewsThreshold:  req.BonusViewsThreshold,
		RatePer1000ViewsOver: req.RatePer1000ViewsOver,
		ClickBonusRate:       req.ClickBonusRate,
		ClickBonusThreshold:  req.ClickBonusThreshold,
		ClickBonusRateOver:   req.ClickBonusRateOver,

		CreatorSalaryPerMonth:       req.CreatorSalaryPerMonth,
		CreatorRatePer1000Views:     req.CreatorRatePer1000Views,
		CreatorRatePer1000ViewsOver: req.CreatorRatePer1000ViewsOver,

		StepViews:      req.StepViews,
		FirstPeriodFee: req.FirstPeriodFee,
		BaseFee:        req.BaseFee,
		StepFee:        req.StepFee,
		StepTier2From:  req.StepTier2From,
		StepFeeOver:    req.StepFeeOver,
		StepCapViews:   req.StepCapViews,
		GuaranteeViews: req.GuaranteeViews,

		CreatorFirstPeriodFee: req.CreatorFirstPeriodFee,
		CreatorBaseFee:        req.CreatorBaseFee,
		CreatorStepFee:        req.CreatorStepFee,
		CreatorStepFeeOver:    req.CreatorStepFeeOver,

		Steps:                 req.Steps,
		SubscriberRate:        req.SubscriberRate,
		CreatorSubscriberRate: req.CreatorSubscriberRate,
	}
}

type paymentReq struct {
	Amount int64  `json:"amount"`
	Note   string `json:"note"`
}

type utmReq struct {
	URL string `json:"url"`
}

// subscribersReq — сколько подписчиков прибавилось креатору за период.
type subscribersReq struct {
	Subscribers int64 `json:"subscribers"`
	// Period — номер периода проекта. 0 = текущий: менеджер вписывает
	// число по ходу периода, а не разыскивает его номер.
	Period int `json:"period"`
}

type subscribersResp struct {
	Items []CreatorSubscribers `json:"items"`
}

type accrualsResp struct {
	Items []Accrual `json:"items"`
}

func writeNoUser(w http.ResponseWriter) {
	httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
}

// writeErr — карта ошибок домена в HTTP.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект или начисление не найдены.")
	case errors.Is(err, ErrNoPeriods):
		// Не ошибка данных, а состояние проекта: периоды начинаются с
		// первой публикации, и до неё отсчитывать не от чего.
		httpx.WriteErrMsg(w, http.StatusNotFound, "no_periods",
			"У проекта ещё нет периодов: не вышло ни одного ролика.")
	case errors.Is(err, ErrAlreadyConfirmed):
		httpx.WriteErrMsg(w, http.StatusConflict, "already_confirmed",
			"Платёж уже подтверждён — сумму задним числом не меняем.")
	case errors.Is(err, ErrAccrualLocked):
		httpx.WriteErrMsg(w, http.StatusConflict, "accrual_locked",
			"Начисление утверждено — пересчитать его нельзя.")
	case errors.Is(err, ErrWrongAccrualStatus):
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_accrual_status",
			"Сначала утвердите период, потом отмечайте выплату.")
	default:
		// Наружу — «internal» без подробностей, в лог — причина.
		//
		// Раньше она терялась здесь целиком: в логе оставалась строка
		// доступа со статусом 500 и ни одной записи уровня ERROR. Причину
		// приходилось добывать прямым запросом к базе, угадывая, что
		// именно сломалось. Сообщение об ошибке — единственное, что
		// отличает «иногда что-то падает» от починки.
		slog.Error("billing: внутренняя ошибка", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// managerProject — id проекта и проверка, что он этого менеджера.
func (h *Handler) managerProject(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return uuid.Nil, uuid.Nil, false
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return uuid.Nil, uuid.Nil, false
	}
	effective := uid
	if role, _ := auth.RoleFrom(r.Context()); role == auth.RoleAdmin {
		effective = uuid.Nil
	}
	if err := h.svc.ManagerHasAccess(r.Context(), projectID, effective); err != nil {
		writeErr(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	return projectID, uid, true
}

// monthParam — месяц из query. Пусто = текущий.
// periodParam — какой период показать. Пусто = текущий.
//
// Номер, а не дата: период — сущность проекта со своим счётом («второй
// месяц креатора»), и адресовать его календарной датой значило бы
// вернуться к тому, от чего ушли.
func periodParam(r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("period"))
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// ---- менеджер ----

// ManagerBilling godoc
// @Summary  Деньги проекта (менеджер)
// @Description Условия, платежи заказчика, начисления креаторам за месяц,
// @Description UTM-метки и итог периода — одним ответом: по частям это
// @Description пять запросов на один экран. Все суммы в копейках.
// @Description В каждой строке начисления две суммы: total — счёт
// @Description заказчику за этого креатора, payout_total — выплата
// @Description самому креатору. В totals они сведены: total, payouts и
// @Description margin (что остаётся платформе).
// @Description Тариф ступенчатый: до порога на ролик — полная ставка,
// @Description свыше — пониженная.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id    path  string true  "project id"
// @Param    period query int false "номер периода проекта, по умолчанию текущий"
// @Success  200 {object} ProjectBilling
// @Failure  400 {object} errorResponse "bad_id; bad_period — номер периода не целое число"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/billing [get]
func (h *Handler) ManagerBilling(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	seq, ok := periodParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_period",
			"period — номер периода, целое число начиная с единицы.")
		return
	}
	out, err := h.svc.ProjectBilling(r.Context(), projectID, seq, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ManagerSaveTerms godoc
// @Summary  Задать условия проекта (менеджер)
// @Description Условия — снимок, а не ссылка на действующий прайс: правка
// @Description прайса не переписывает историю уже идущего проекта.
// @Description У тарифа две стороны: что платит клиент и что получает
// @Description креатор. Креаторские ставки null означают «столько же» —
// @Description тогда выплата равна счёту и маржи у платформы нет.
// @Tags     manager-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string   true "project id"
// @Param    body body termsReq true "ставки в копейках"
// @Success  200  {object} Terms
// @Failure  400  {object} errorResponse "bad_json; bad_id; invalid_input — отрицательная ставка"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/billing [put]
func (h *Handler) ManagerSaveTerms(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req termsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	t := req.terms()
	t.ProjectID = projectID
	t, err := h.svc.SaveTerms(r.Context(), t, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

// ManagerAdoptTerms godoc
// @Summary  Взять условия из действующего прайса (менеджер)
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} Terms
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден, или прайс ещё не заведён"
// @Router   /manager/projects/{id}/billing/adopt [post]
func (h *Handler) ManagerAdoptTerms(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	t, err := h.svc.AdoptLatestTerms(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

// ManagerSetPayment godoc
// @Summary  Сколько ждём от заказчика (менеджер)
// @Description Видов два: prepayment и final — половина вперёд, половина по
// @Description завершении. Подтверждённый платёж не переписывается.
// @Tags     manager-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "project id"
// @Param    kind path string     true "prepayment или final"
// @Param    body body paymentReq true "сумма в копейках"
// @Success  200  {object} Payment
// @Failure  400  {object} errorResponse "bad_json; bad_id; invalid_input — неизвестный вид платежа или отрицательная сумма"
// @Failure  401  {object} errorResponse "no_user — сессия истекла"
// @Failure  404  {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Failure  409  {object} errorResponse "already_confirmed — платёж подтверждён, сумму задним числом не меняем"
// @Router   /manager/projects/{id}/payments/{kind} [put]
func (h *Handler) ManagerSetPayment(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	var req paymentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.SetPayment(r.Context(), projectID, PaymentKind(chi.URLParam(r, "kind")), req.Amount, req.Note, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ManagerConfirmPayment godoc
// @Summary  Подтвердить платёж (менеджер)
// @Description Платёжного провайдера нет: деньги приходят мимо системы, а
// @Description менеджер подтверждает получение — поэтому подтверждение
// @Description именное и заносится в журнал.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id   path string true "project id"
// @Param    kind path string true "prepayment или final"
// @Success  200 {object} Payment
// @Failure  400 {object} errorResponse "bad_id; invalid_input — неизвестный вид платежа"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — платёж не заведён или проект не ваш"
// @Failure  409 {object} errorResponse "already_confirmed — платёж уже подтверждён"
// @Router   /manager/projects/{id}/payments/{kind}/confirm [post]
func (h *Handler) ManagerConfirmPayment(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	p, err := h.svc.ConfirmPayment(r.Context(), projectID, PaymentKind(chi.URLParam(r, "kind")), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// ManagerRecalcAccruals godoc
// @Summary  Пересчитать начисления за месяц (менеджер)
// @Description Считается по фактам: выкладки месяца, их статус и просмотры
// @Description из ежедневного сбора. Недосданные ролики не оплачиваются —
// @Description вычет пропорционален недостаче. Бонус идёт только с роликов,
// @Description перешагнувших порог просмотров. Утверждённые и выплаченные
// @Description строки не трогаются.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id    path  string true  "project id"
// @Param    period query int false "номер периода проекта, по умолчанию текущий"
// @Success  200 {object} accrualsResp
// @Failure  400 {object} errorResponse "bad_id; bad_period"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/accruals/recalc [post]
func (h *Handler) ManagerRecalcAccruals(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	seq, ok := periodParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_period",
			"period — номер периода, целое число начиная с единицы.")
		return
	}
	period, err := h.svc.Period(r.Context(), projectID, seq, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	items, err := h.svc.Recalculate(r.Context(), projectID, period)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, accrualsResp{Items: items})
}

// ManagerApproveAccrual godoc
// @Summary  Утвердить период (менеджер)
// @Description После утверждения строка не пересчитывается.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id         path string true "project id"
// @Param    accrual_id path string true "accrual id"
// @Success  200 {object} Accrual
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — начисление не найдено"
// @Failure  409 {object} errorResponse "wrong_accrual_status — период уже утверждён или выплачен"
// @Router   /manager/projects/{id}/accruals/{accrual_id}/approve [post]
func (h *Handler) ManagerApproveAccrual(w http.ResponseWriter, r *http.Request) {
	h.decideAccrual(w, r, false)
}

// ManagerPayAccrual godoc
// @Summary  Отметить выплату (менеджер)
// @Description Только после утверждения: между «посчитали» и «отправили
// @Description деньги» проходит время, и путать их нельзя.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id         path string true "project id"
// @Param    accrual_id path string true "accrual id"
// @Success  200 {object} Accrual
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — начисление не найдено"
// @Failure  409 {object} errorResponse "wrong_accrual_status — период не утверждён или уже выплачен"
// @Router   /manager/projects/{id}/accruals/{accrual_id}/paid [post]
func (h *Handler) ManagerPayAccrual(w http.ResponseWriter, r *http.Request) {
	h.decideAccrual(w, r, true)
}

func (h *Handler) decideAccrual(w http.ResponseWriter, r *http.Request, pay bool) {
	_, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	accrualID, err := uuid.Parse(chi.URLParam(r, "accrual_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id начисления.")
		return
	}
	var a Accrual
	if pay {
		a, err = h.svc.MarkAccrualPaid(r.Context(), accrualID, uid)
	} else {
		a, err = h.svc.ApproveAccrual(r.Context(), accrualID, uid)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

// ManagerSaveUTM godoc
// @Summary  Поставить UTM-метку креатору (менеджер)
// @Description Метку ставит менеджер, креатор её только видит. Переходы
// @Description заполняются снаружи; бонус за них пока выключен (BETA) и
// @Description считается, только если задать ставку в условиях.
// @Tags     manager-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id         path string true "project id"
// @Param    creator_id path string true "creator id"
// @Param    body       body utmReq true "ссылка"
// @Success  200 {object} UTMLink
// @Failure  400 {object} errorResponse "bad_json; bad_id; invalid_input — пустая ссылка или не http(s)"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/creators/{creator_id}/utm [put]
func (h *Handler) ManagerSaveUTM(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	creatorID, err := uuid.Parse(chi.URLParam(r, "creator_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id креатора.")
		return
	}
	var req utmReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	l, err := h.svc.SaveUTM(r.Context(), projectID, creatorID, req.URL, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, l)
}

// ManagerSubscribers godoc
// @Summary  Подписчики за период (менеджер)
// @Description Сколько подписчиков записано креаторам проекта за период.
// @Description Автоматического источника у этого числа нет: сборщика по
// @Description подписчикам в продукте не существует, и ставка в тарифе
// @Description объявляется под число, которое вписывает менеджер.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id     path  string true  "project id"
// @Param    period query int    false "номер периода проекта, по умолчанию текущий"
// @Success  200 {object} subscribersResp
// @Failure  400 {object} errorResponse "bad_id; bad_period"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/subscribers [get]
func (h *Handler) ManagerSubscribers(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	seq, ok := periodParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_period",
			"Номер периода должен быть целым числом.")
		return
	}
	p, err := h.svc.Period(r.Context(), projectID, seq, time.Now())
	if errors.Is(err, ErrNoPeriods) {
		// Периодов нет — и подписчиков не к чему привязать. Пустой
		// список честнее 404: проект есть, просто работа не началась.
		httpx.WriteJSON(w, http.StatusOK, subscribersResp{Items: []CreatorSubscribers{}})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	items, err := h.svc.Subscribers(r.Context(), projectID, p.StartsOn)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, subscribersResp{Items: items})
}

// ManagerSaveSubscribers godoc
// @Summary  Вписать подписчиков за период (менеджер)
// @Description Число вводится руками: сборщика подписчиков нет, а KPI по
// @Description ним в тарифе объявлен. Так же заведены переходы по UTM.
// @Tags     manager-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id         path string true "project id"
// @Param    creator_id path string true "creator id"
// @Param    body       body subscribersReq true "подписчики"
// @Success  200 {object} CreatorSubscribers
// @Failure  400 {object} errorResponse "bad_json; bad_id; invalid_input — отрицательное число"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден, ведёт другой менеджер или периодов ещё нет"
// @Router   /manager/projects/{id}/creators/{creator_id}/subscribers [put]
func (h *Handler) ManagerSaveSubscribers(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	creatorID, err := uuid.Parse(chi.URLParam(r, "creator_id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id креатора.")
		return
	}
	var req subscribersReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	p, err := h.svc.Period(r.Context(), projectID, req.Period, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	c, err := h.svc.SaveSubscribers(r.Context(), projectID, creatorID, p.StartsOn, req.Subscribers, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

// ---- заказчик ----

// ClientBilling godoc
// @Summary  Условия и оплата по проекту (заказчик)
// @Description Что подписано, что оплачено и состав месяца: кто сколько
// @Description роликов сдал и во сколько это обошлось. Заказчик за эту
// @Description команду платит, поэтому строки начислений — его счёт.
// @Description UTM-метки ему не отдаются: это инструмент менеджера.
// @Description Выплат креаторам, маржи площадки и креаторской стороны
// @Description тарифа в ответе нет — ответ собирается отдельным типом,
// @Description а не чисткой менеджерской структуры.
// @Tags     client-billing
// @Produce  json
// @Security BearerAuth
// @Param    id    path  string true  "project id"
// @Param    period query int false "номер периода проекта, по умолчанию текущий"
// @Success  200 {object} ClientBillingView
// @Failure  400 {object} errorResponse "bad_id; bad_period — номер периода не целое число"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или он не ваш"
// @Router   /me/projects/{id}/billing [get]
func (h *Handler) ClientBilling(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	owns, err := h.svc.ClientOwnsProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !owns {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return
	}
	seq, ok := periodParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_period",
			"period — номер периода, целое число начиная с единицы.")
		return
	}
	out, err := h.svc.ClientBilling(r.Context(), projectID, seq, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- креатор ----

// CreatorEarnings godoc
// @Summary  Мой заработок по проекту (креатор)
// @Description По каким условиям и сколько вышло по месяцам. Показывается
// @Description креаторская сторона тарифа и его собственные выплаты: что
// @Description за него платит клиент — не его данные.
// @Tags     creator-billing
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} CreatorEarnings
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или вы не в его составе"
// @Router   /me/creator/projects/{id}/earnings [get]
func (h *Handler) CreatorEarnings(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	in, err := h.svc.CreatorInProject(r.Context(), projectID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !in {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Проект не найден.")
		return
	}
	out, err := h.svc.CreatorEarnings(r.Context(), projectID, uid, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ClientOrderEstimate godoc
// @Summary  Во сколько обойдётся заказ (клиент)
// @Description Оклады известны точно, бонус — прогноз: он зависит от
// @Description просмотров, которых ещё нет. Прогноз строится по истории
// @Description тех самых людей, что в подборке — по их сданным роликам в
// @Description любых проектах. По кому истории нет, тот в среднее не
// @Description входит и посчитан в without_history: считать новичка нулём
// @Description значит занизить смету ровно настолько, насколько подборка
// @Description новая. Если истории нет ни у кого, has_forecast=false и
// @Description показывать надо только оклады — бонус не ноль, а неизвестен.
// @Description Условия берутся той версии, с которой клиент согласился при
// @Description создании заказа, а не действующей на сегодня.
// @Tags     client-billing
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} OrderEstimate
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — заказ не найден или он не ваш"
// @Router   /me/orders/{id}/estimate [get]
func (h *Handler) ClientOrderEstimate(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заказа.")
		return
	}
	out, err := h.svc.EstimateOrder(r.Context(), orderID, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// draftEstimateReq — состав, который клиент ещё собирает.
type draftEstimateReq struct {
	// Needed — сколько креаторов нужно. Единственное обязательное поле:
	// без него не посчитать даже оклады.
	Needed int `json:"needed"`
	// VideosCount — объём роликов. Ноль значит «ещё не знаю»: смету
	// спрашивают и до того, как решили, сколько роликов в месяц.
	VideosCount int `json:"videos_count"`
	// CreatorIDs — кого выбрал клиент. Порядок здесь не важен: на сумму
	// влияет состав, а не приоритет. Пусто — состав ещё не набран, и
	// прогноз бонуса не считается.
	CreatorIDs []uuid.UUID `json:"creator_ids"`
}

// ClientDraftEstimate godoc
// @Summary  Сколько будет стоить пакет (заказчик, до создания заказа)
// @Description Сумма для страницы подбора: клиент собирает состав, и она
// @Description пересчитывается на каждое изменение. Оклады известны точно,
// @Description бонус — прогноз по истории выбранных людей, разложенный по
// @Description ступеням тарифа. Кто без истории, тот в среднее не входит и
// @Description посчитан в without_history: считать новичка нулём значит
// @Description занизить смету ровно настолько, насколько подборка новая.
// @Description has_forecast=false означает «бонус неизвестен», а не «ноль».
// @Description Условия берутся действующие — те, с которыми клиент и
// @Description согласится при оформлении.
// @Description Обязателен только needed: смету спрашивают и до того, как
// @Description набран состав и решён объём. Без них приходят точные
// @Description оклады и has_forecast=false.
// @Tags     client-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body draftEstimateReq true "состав"
// @Success  200 {object} OrderEstimate
// @Failure  400 {object} errorResponse "bad_json; invalid_input — needed меньше единицы, отрицательный videos_count или больше 50 человек"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — прайс ещё не заведён"
// @Router   /me/orders/estimate [post]
func (h *Handler) ClientDraftEstimate(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFrom(r.Context()); !ok {
		writeNoUser(w)
		return
	}
	var req draftEstimateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	out, err := h.svc.EstimateDraft(r.Context(), req.Needed, req.VideosCount, req.CreatorIDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- прайс площадки (админ) ----

// termsVersionsResp — список версий прайса.
type termsVersionsResp struct {
	Items []TermsVersion `json:"items"`
}

// publishTermsReq — новая версия прайса. Ставки те же, что у условий
// проекта, плюс текст, с которым соглашается клиент.
type publishTermsReq struct {
	termsReq
	Body string `json:"body"`
}

// AdminListTerms godoc
// @Summary  Прайс площадки: все версии (админ)
// @Tags     admin-billing
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} termsVersionsResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  403 {object} errorResponse "forbidden — нужна роль admin"
// @Router   /admin/terms [get]
func (h *Handler) AdminListTerms(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListTermsVersions(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, termsVersionsResp{Items: items})
}

// AdminPublishTerms godoc
// @Summary  Выпустить новую версию прайса (админ)
// @Description Существующая версия не переписывается: под ней стоит согласие клиентов, а проекты сняли с неё числа снимком.
// @Description В ответе — разница с прежней действующей версией (changes) и
// @Description сколько клиентов должны согласиться заново (consents_required).
// @Tags     admin-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body publishTermsReq true "ставки и текст условий"
// @Success  201 {object} TermsPublishResult "версия + разница с прежней + сколько клиентов должны согласиться заново"
// @Failure  400 {object} errorResponse "bad_json, invalid_input — текст пуст или доля креатора больше цены клиента"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  403 {object} errorResponse "forbidden — нужна роль admin"
// @Router   /admin/terms [post]
func (h *Handler) AdminPublishTerms(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	var req publishTermsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	v, err := h.svc.PublishTermsVersion(r.Context(), TermsVersion{
		Terms: req.terms(),
		Body:  req.Body,
	}, actor)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, v)
}

// ---- подытог периода ----

// periodResp — состояние периода после действия.
type periodResp struct {
	Period ProjectPeriod `json:"period"`
}

type unlockPeriodReq struct {
	// Reason — зачем переоткрыли. Необязательно, но попадает в журнал:
	// через полгода «почему числа поменялись» отвечается только этим.
	Reason string `json:"reason,omitempty"`
}

// AdminUnlockPeriod godoc
// @Summary  Вернуть подытоженный период в работу (админ)
// @Description Срез просмотров удаляется, период снова считается на лету.
// @Description Действие пишется в журнал админских действий: переоткрытие
// @Description переписывает историю расчёта, и след обязателен.
// @Description Идемпотентно: период, который и так идёт, ручка оставляет как есть.
// @Description
// @Description ПОСЛЕДСТВИЕ: у следующего периода вход (carry_in_*) посчитан
// @Description от выхода этого, и после переоткрытия он недостоверен —
// @Description как и вся цепочка дальше. Пока арифметики переноса нет,
// @Description это предупреждение; в ответе видно, сколько подытоженных
// @Description периодов идёт следом.
// @Tags     admin-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id     path  string true  "project id"
// @Param    period query int    false "номер периода, по умолчанию текущий"
// @Param    body   body  unlockPeriodReq false "причина"
// @Success  200 {object} periodResp
// @Failure  400 {object} errorResponse "bad_id; bad_period"
// @Failure  404 {object} errorResponse "not_found — у проекта ещё нет периодов"
// @Router   /admin/projects/{id}/billing/unlock_period [post]
type confirmPeriodEndReq struct {
	// EndsOn — ГГГГ-ММ-ДД. Пусто — подтверждаем ту дату, что стоит:
	// это отметка «я проверил», а не правка.
	EndsOn string `json:"ends_on"`
}

// ManagerConfirmPeriodEnd godoc
// @Summary  Подтвердить конец периода
// @Description Менеджер подтверждает, каким числом кончается период.
// @Description Границу считает автомат — месяц от первой выкладки, — и он
// @Description остаётся главным путём. Но подтверждённая дата СИЛЬНЕЕ
// @Description вычисленной: план знает человек, а календарь только считает
// @Description месяцы.
// @Description
// @Description Дата та же — это отметка «проверил», она гасит тревогу и
// @Description больше ничего не меняет. Другая — граница переезжает, и
// @Description вместе с ней вся цепочка дальше: начало следующего периода,
// @Description его конец, отсечка.
// @Description
// @Description Подытоженный период не подтверждают: под ним уже стоит счёт.
// @Tags     manager-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id     path  string true  "project id"
// @Param    period query int    false "номер периода, по умолчанию текущий"
// @Param    body   body  confirmPeriodEndReq false "дата конца"
// @Success  200 {object} periodResp
// @Failure  400 {object} errorResponse "bad_id; bad_period; bad_date; bad_range"
// @Failure  409 {object} errorResponse "period_locked — период подытожен"
// @Router   /manager/projects/{id}/billing/confirm_period_end [post]
func (h *Handler) ManagerConfirmPeriodEnd(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	seq, ok := periodParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_period",
			"period — номер периода, целое число начиная с единицы.")
		return
	}
	var in confirmPeriodEndReq
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	now := time.Now()
	period, err := h.svc.Period(r.Context(), projectID, seq, now.UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	endsOn := period.EndsOn
	if in.EndsOn != "" {
		endsOn, err = time.Parse("2006-01-02", in.EndsOn)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date",
				"ends_on — дата в формате ГГГГ-ММ-ДД.")
			return
		}
	}
	out, err := h.svc.ConfirmPeriodEnd(r.Context(), period.ID, endsOn, actor, now)
	switch {
	case errors.Is(err, ErrPeriodLocked):
		httpx.WriteErrMsg(w, http.StatusConflict, "period_locked",
			"Период подытожен — его границу больше не двигают.")
		return
	case errors.Is(err, ErrBadPeriodEnd):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_range",
			"Конец периода не может быть раньше его начала.")
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, periodResp{Period: out})
}

func (h *Handler) AdminUnlockPeriod(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	seq, ok := periodParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_period",
			"period — номер периода, целое число начиная с единицы.")
		return
	}
	var in unlockPeriodReq
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	period, err := h.svc.Period(r.Context(), projectID, seq, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := h.svc.UnlockPeriod(r.Context(), period.ID, actor, in.Reason, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, periodResp{Period: out})
}

// ManagerPeriods godoc
// @Summary  Периоды проекта (менеджер)
// @Description Периоды катятся от даты первой публикации: вышел первый
// @Description ролик 15-го — периоды идут с 15-го по 14-е. Пока не вышло
// @Description ничего, периодов нет — и это не ошибка.
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} periodsResp
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/billing/periods [get]
func (h *Handler) ManagerPeriods(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	items, err := h.svc.Periods(r.Context(), projectID, time.Now().UTC())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, periodsResp{Items: items})
}

type periodsResp struct {
	Items []ProjectPeriod `json:"items"`
}

type tariffRegistryResp struct {
	Items []TariffRow `json:"items"`
}

// AdminTariffRegistry godoc
// @Summary  Тарифы всех проектов одной таблицей (админ)
// @Description Строка — проект. Прайс площадки один на всех, а
// @Description договариваются с каждым заказчиком отдельно: вопрос к этому
// @Description разделу звучит не «какой у нас прайс», а «по каким условиям
// @Description идёт вот этот проект и чем он отличается от соседнего».
// @Description Видно и худшее состояние — проект без тарифа: он считается
// @Description по нулям, а экран денег показывает ровные нули, и выглядит
// @Description это как «ещё не начислили».
// @Tags     admin-billing
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} tariffRegistryResp
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  403 {object} errorResponse "forbidden — нужна роль admin"
// @Router   /admin/tariff/projects [get]
func (h *Handler) AdminTariffRegistry(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.TariffRegistry(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tariffRegistryResp{Items: items})
}
