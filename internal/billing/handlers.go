package billing

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

// monthLayout — месяц в запросах, как в заказах: ГГГГ-ММ.
const monthLayout = "2006-01"

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
}

type paymentReq struct {
	Amount int64  `json:"amount"`
	Note   string `json:"note"`
}

type utmReq struct {
	URL string `json:"url"`
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
func monthParam(r *http.Request) (time.Time, bool) {
	raw := r.URL.Query().Get("month")
	if raw == "" {
		return time.Now().UTC(), true
	}
	m, err := time.Parse(monthLayout, raw)
	if err != nil {
		return time.Time{}, false
	}
	return m, true
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
// @Param    month query string false "ГГГГ-ММ, по умолчанию текущий"
// @Success  200 {object} ProjectBilling
// @Failure  400 {object} errorResponse "bad_id; bad_month — месяц не в формате ГГГГ-ММ"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/billing [get]
func (h *Handler) ManagerBilling(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	month, ok := monthParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month", "Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	out, err := h.svc.ProjectBilling(r.Context(), projectID, month)
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
	t, err := h.svc.SaveTerms(r.Context(), Terms{
		ProjectID:            projectID,
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
	}, uid)
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
// @Param    month query string false "ГГГГ-ММ, по умолчанию текущий"
// @Success  200 {object} accrualsResp
// @Failure  400 {object} errorResponse "bad_id; bad_month"
// @Failure  401 {object} errorResponse "no_user — сессия истекла"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/accruals/recalc [post]
func (h *Handler) ManagerRecalcAccruals(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	month, ok := monthParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month", "Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	items, err := h.svc.Recalculate(r.Context(), projectID, month)
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
// @Param    month query string false "ГГГГ-ММ, по умолчанию текущий"
// @Success  200 {object} ClientBillingView
// @Failure  400 {object} errorResponse "bad_id; bad_month — месяц не в формате ГГГГ-ММ"
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
	month, ok := monthParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month", "Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	out, err := h.svc.ClientBilling(r.Context(), projectID, month)
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
	out, err := h.svc.CreatorEarnings(r.Context(), projectID, uid)
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
	// Needed — сколько креаторов нужно; VideosCount — объём роликов.
	Needed      int `json:"needed"`
	VideosCount int `json:"videos_count"`
	// CreatorIDs — кого выбрал клиент. Порядок здесь не важен: на сумму
	// влияет состав, а не приоритет.
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
// @Tags     client-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body draftEstimateReq true "состав"
// @Success  200 {object} OrderEstimate
// @Failure  400 {object} errorResponse "bad_json; invalid_input — пустой состав, больше 50 человек, ноль роликов или креаторов"
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
		Terms: Terms{
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
		},
		Body: req.Body,
	}, actor)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, v)
}

// ---- фиксация месяца ----

// monthLockResp — состояние месяца после действия.
type monthLockResp struct {
	Month ProjectMonth `json:"month"`
}

// ManagerLockMonth godoc
// @Summary  Зафиксировать месяц (менеджер)
// @Description Сохраняет срез просмотров — по каждой выкладке и каждой
// @Description площадке, на сегодняшнюю отсечку — и пересчитывает месяц
// @Description уже по нему. После этого числа месяца не меняются, даже
// @Description если просмотры продолжают расти. Идемпотентно: повторный вызов на уже
// @Description зафиксированном месяце ничего не меняет и не ошибка.
// @Description Обычно месяц фиксируется сам через 14 дней после его
// @Description конца; эта ручка — «зафиксировать сейчас».
// @Tags     manager-billing
// @Produce  json
// @Security BearerAuth
// @Param    id    path  string true  "project id"
// @Param    month query string false "ГГГГ-ММ, по умолчанию текущий"
// @Success  200 {object} monthLockResp
// @Failure  400 {object} errorResponse "bad_id; bad_month"
// @Failure  404 {object} errorResponse "not_found — проект не найден или ведёт другой менеджер"
// @Router   /manager/projects/{id}/billing/lock_month [post]
func (h *Handler) ManagerLockMonth(w http.ResponseWriter, r *http.Request) {
	projectID, actor, ok := h.managerProject(w, r)
	if !ok {
		return
	}
	month, ok := monthParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month", "Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	// Ручная фиксация: отсечка сегодняшняя. Числа всё равно берутся из
	// поденного ряда, а не последним снимком, — чтобы ручная и
	// автоматическая дороги на одних данных давали одно и то же.
	now := time.Now().UTC()
	m, err := h.svc.LockMonth(r.Context(), projectID, month, &actor, now, now)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, monthLockResp{Month: m})
}

type unlockMonthReq struct {
	// Reason — зачем переоткрыли. Необязательно, но попадает в журнал:
	// через полгода «почему числа поменялись» отвечается только этим.
	Reason string `json:"reason,omitempty"`
}

// AdminUnlockMonth godoc
// @Summary  Вернуть зафиксированный месяц в работу (админ)
// @Description Срез просмотров удаляется, месяц снова считается на лету.
// @Description Действие пишется в журнал админских действий: расфиксация
// @Description переписывает историю расчёта, и след обязателен.
// @Description Идемпотентно: месяц, который и так идёт, ручка оставляет как есть.
// @Tags     admin-billing
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id    path  string true  "project id"
// @Param    month query string false "ГГГГ-ММ, по умолчанию текущий"
// @Param    body  body  unlockMonthReq false "причина"
// @Success  200 {object} monthLockResp
// @Failure  400 {object} errorResponse "bad_id; bad_month"
// @Router   /admin/projects/{id}/billing/unlock_month [post]
func (h *Handler) AdminUnlockMonth(w http.ResponseWriter, r *http.Request) {
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
	month, ok := monthParam(r)
	if !ok {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month", "Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	var in unlockMonthReq
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	if err := h.svc.UnlockMonth(r.Context(), projectID, month, actor, in.Reason); err != nil {
		writeErr(w, err)
		return
	}
	m, err := h.svc.Month(r.Context(), projectID, month)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, monthLockResp{Month: m})
}
