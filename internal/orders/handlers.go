package orders

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

const monthLayout = "2006-01"

func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

func writeNoUser(w http.ResponseWriter) {
	httpx.WriteErrMsg(w, http.StatusUnauthorized, "no_user", "Сессия истекла — войдите снова")
}

// writeErr — карта ошибок домена в HTTP. Отдельной функцией, чтобы ни
// одна ветка не отдала 500 там, где человеку нужно объяснение.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPrioritySetMismatch):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "priority_set_mismatch",
			"Перестановка меняет порядок, а не состав: пришлите ровно тех, кого ещё не приглашали.")
	case errors.Is(err, ErrPriorityLocked):
		httpx.WriteErrMsg(w, http.StatusConflict, "priority_locked",
			"Заказ уже собран — порядок приглашений больше не важен.")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNoConsent):
		httpx.WriteErrMsg(w, http.StatusForbidden, "no_consent",
			"Сначала примите условия работы — без этого подбор недоступен.")
	case errors.Is(err, ErrTooManyCreators):
		httpx.WriteErrMsg(w, http.StatusConflict, "too_many_creators",
			"Больше креаторов пока взять нельзя: в первый месяц работы доступен один, со второго — до трёх.")
	case errors.Is(err, ErrNotEnoughCandidates):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "not_enough_candidates",
			"В подборке меньше людей, чем нужно взять.")
	case errors.Is(err, ErrDuplicateCandidate):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "duplicate_candidate",
			"Один и тот же человек добавлен в подборку дважды.")
	case errors.Is(err, ErrNotACreator):
		httpx.WriteErrMsg(w, http.StatusConflict, "not_a_creator",
			"В пакет берутся только креаторы — блогеры и авторы UGC. Монтажёры и продакшн — из другой ветки.")
	case errors.Is(err, ErrCreatorBusy):
		httpx.WriteErrMsg(w, http.StatusConflict, "creator_busy",
			"Кто-то из выбранных занят в этом месяце.")
	case errors.Is(err, ErrWrongStatus):
		httpx.WriteErrMsg(w, http.StatusConflict, "wrong_status", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotInvited):
		httpx.WriteErrMsg(w, http.StatusConflict, "not_invited",
			"Активного приглашения нет: возможно, оно сгорело или место уже занято.")
	case errors.Is(err, ErrNothingAttached):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "nothing_attached",
			"Приложите ролик или выберите его из своих — иначе показывать менеджеру нечего.")
	case errors.Is(err, ErrNotYourPortfolio):
		httpx.WriteErrMsg(w, http.StatusForbidden, "not_your_portfolio",
			"В отклик можно приложить только свои ролики.")
	case errors.Is(err, ErrNoProject):
		httpx.WriteErrMsg(w, http.StatusConflict, "no_project",
			"У заявки нет проекта — добавлять людей некуда. Заведите проект вручную.")
	case errors.Is(err, ErrNoFreeSlot):
		httpx.WriteErrMsg(w, http.StatusConflict, "no_free_slot",
			"Звать некого: свободных мест нет или резерв кончился.")
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Заказ не найден.")
	default:
		// Незнакомая ошибка — это 500, и по коду 500 в отчёте не видно
		// ничего. Пишем её в лог здесь, а не в каждом хендлере: иначе
		// половина веток отдаёт «internal» молча, и разбирать приходится
		// по времени запроса.
		slog.Error("orders: необработанная ошибка", "err", err)
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

// ---- клиент ----

type termsResp struct {
	Terms     Terms `json:"terms"`
	Consented bool  `json:"consented"`
}

// ClientTerms godoc
// @Summary  Действующие условия работы (заказчик)
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} termsResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — версия правил ещё не опубликована"
// @Router   /me/orders/terms [get]
func (h *Handler) ClientTerms(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	terms, err := h.svc.CurrentTerms(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	consented, err := h.svc.repo.HasConsent(r.Context(), uid, terms.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, termsResp{Terms: terms, Consented: consented})
}

// ClientConsent godoc
// @Summary  Принять условия работы (заказчик)
// @Description Согласие фиксируется с конкретной версией: правила меняются —
// @Description старые заказы остаются на своей.
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} Terms
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — версия правил ещё не опубликована"
// @Router   /me/orders/terms/consent [post]
func (h *Handler) ClientConsent(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	terms, err := h.svc.Consent(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, terms)
}

type limitResp struct {
	// Month — месяц, на который посчитан лимит, ГГГГ-ММ.
	Month string `json:"month"`
	// Allowed — верхняя граница: сколько креаторов можно взять на этот
	// месяц. MinAllowed — нижняя, для подписи «доступно 2–3». Взять
	// меньше нижней границы не запрещено.
	Allowed    int `json:"allowed"`
	MinAllowed int `json:"min_allowed"`
	// CompletedMonths — сколько оплаченных месяцев будет закрыто к его
	// началу. Отдаётся, чтобы «доступен один креатор» не выглядело
	// произволом: видно, что месяцев работы пока ноль.
	CompletedMonths int `json:"completed_months"`
	// FirstMonth — это первый месяц клиента. В нём берут одного креатора:
	// клиент проверяет формат на небольшой сумме, платформа — что клиент
	// платит и даёт обратную связь.
	FirstMonth bool `json:"first_month"`
}

// ClientLimit godoc
// @Summary  Сколько креаторов доступно на месяц (заказчик)
// @Description Первый месяц — один креатор: клиент проверяет формат на
// @Description небольшой сумме, платформа проверяет, что клиент платит и
// @Description адекватно даёт обратную связь. Дальше 2–3.
// @Description Месяц важен: клиент берёт команду с конкретного месяца, и
// @Description к декабрю у него может быть закрыто больше месяцев, чем к
// @Description сентябрю — лимит на сегодня показывал бы не то число, по
// @Description которому потом создастся заказ.
// @Description Клиент видит ограничение ДО подбора, а не при попытке
// @Description добавить второго: запрет посреди работы читается как поломка.
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Param    month query string false "ГГГГ-ММ, по умолчанию текущий"
// @Success  200 {object} limitResp
// @Failure      400  {object}  errorResponse  "bad_month — месяц не в формате ГГГГ-ММ"
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/orders/limit [get]
func (h *Handler) ClientLimit(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	// Месяц выбирает клиент: он берёт команду не «вообще», а с конкретного
	// месяца, и к декабрю у него может быть закрыто больше месяцев, чем
	// к сентябрю. Без параметра — текущий.
	month := time.Now().UTC()
	if raw := r.URL.Query().Get("month"); raw != "" {
		parsed, err := time.Parse(monthLayout, raw)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month",
				"Месяц должен быть в формате ГГГГ-ММ.")
			return
		}
		month = parsed
	}
	allowed, err := h.svc.AllowedCreators(r.Context(), uid, month)
	if err != nil {
		writeErr(w, err)
		return
	}
	completed, err := h.svc.CompletedMonths(r.Context(), uid, month)
	if err != nil {
		writeErr(w, err)
		return
	}
	// В первый месяц вилки нет — ровно один. Дальше 2–3.
	minAllowed := FirstMonthCreators
	if completed > 0 {
		minAllowed = MinCreators
	}
	httpx.WriteJSON(w, http.StatusOK, limitResp{
		Month:           month.Format(monthLayout),
		Allowed:         allowed,
		MinAllowed:      minAllowed,
		CompletedMonths: completed,
		FirstMonth:      completed == 0,
	})
}

type createOrderReq struct {
	StartMonth  string `json:"start_month"`
	Needed      int    `json:"needed"`
	VideosCount int    `json:"videos_count"`
	// CreatorIDs — кого заказчик отметил. Порядка в списке больше нет:
	// очередь приглашений ушла, приглашение уходит всем известным
	// креаторам, а отмеченные получают его с пометкой «хотят особенно».
	CreatorIDs []uuid.UUID `json:"creator_ids"`
	// Brief — первый шаг воронки. Необязателен: заявка без брифа лучше
	// формы, которую бросили на полпути, а дописать его можно потом.
	Brief OrderBrief `json:"brief"`
	// Ceiling — потолок в копейках, который заказчику показали на баре.
	// Идёт в сообщение менеджеру: разговор начинается с той суммы,
	// которую человек видел, а не с пересчитанной на сервере.
	Ceiling int64 `json:"ceiling"`
}

// briefReq — правка брифа после отправки. Половина заказчиков
// вспоминает про референсы уже после «Отправить».
type briefReq struct {
	OrderBrief
}

type briefResp struct {
	Brief OrderBrief `json:"brief"`
}

// ClientCreateOrder godoc
// @Summary  Создать заказ на подбор (заказчик)
// @Tags     client-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body createOrderReq true "состав заказа"
// @Success  201 {object} CreateResult
// @Failure      400  {object}  errorResponse  "bad_json; bad_month; invalid_input — объём, месяц в прошлом, пустой id; not_enough_candidates — в подборке меньше, чем нужно взять; duplicate_candidate"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      403  {object}  errorResponse  "no_consent — не приняты условия работы"
// @Failure      409  {object}  errorResponse  "not_a_creator — в пакет берутся только блогеры и авторы UGC"
// @Description Заявка заводит ПРОЕКТ сразу: он виден в кабинете, и в нём работает
// @Description переписка, пока менеджер считает. project_id приходит в ответе.
// @Router   /me/orders [post]
func (h *Handler) ClientCreateOrder(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	var req createOrderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	month, err := time.Parse(monthLayout, req.StartMonth)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month",
			"Месяц старта должен быть в формате ГГГГ-ММ.")
		return
	}
	// Needed не спрашиваем у клиента как «сколько мест»: мест больше
	// нет. Сколько отметили — столько и отметили, и это же число идёт в
	// потолок цены.
	needed := req.Needed
	if needed == 0 {
		needed = len(req.CreatorIDs)
	}
	res, err := h.svc.Create(r.Context(), CreateOrderInput{
		ClientUserID: uid, StartMonth: month, Needed: needed,
		VideosCount: req.VideosCount, CreatorIDs: req.CreatorIDs,
		Brief: req.Brief, Ceiling: req.Ceiling,
	}, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

type ordersListResp struct {
	Items []Order `json:"items"`
}

// ClientListOrders godoc
// @Summary  Мои заказы на подбор (заказчик)
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} ordersListResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/orders [get]
func (h *Handler) ClientListOrders(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	items, err := h.svc.ListByClient(r.Context(), uid, 20)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ordersListResp{Items: items})
}

// clientOrder — id заказа из пути с проверкой, что он принадлежит этому
// заказчику. Чужой заказ прячется за 404: подтверждать его существование
// постороннему незачем.
func (h *Handler) clientOrder(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return uuid.Nil, false
	}
	orderID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заказа.")
		return uuid.Nil, false
	}
	owns, err := h.svc.OwnedByClient(r.Context(), orderID, uid)
	if err != nil {
		writeErr(w, err)
		return uuid.Nil, false
	}
	if !owns {
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Заказ не найден.")
		return uuid.Nil, false
	}
	return orderID, true
}

// ClientGetOrder godoc
// @Summary  Заказ на подбор (заказчик)
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_id — неверный id заказа"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не ваш или не существует"
// @Router   /me/orders/{id} [get]
func (h *Handler) ClientGetOrder(w http.ResponseWriter, r *http.Request) {
	orderID, ok := h.clientOrder(w, r)
	if !ok {
		return
	}
	o, err := h.svc.Get(r.Context(), orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

// ClientInvite godoc
// @Summary  Отправить приглашения (заказчик)
// @Description Приглашения уходят первым по приоритету и только на свободные
// @Description места. Откажется или промолчит — подключится следующий.
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не ваш"
// @Failure      409  {object}  errorResponse  "wrong_status — приглашения уже отправлены"
// @Router   /me/orders/{id}/invite [post]
func (h *Handler) ClientInvite(w http.ResponseWriter, r *http.Request) {
	orderID, ok := h.clientOrder(w, r)
	if !ok {
		return
	}
	o, err := h.svc.SendInvitations(r.Context(), orderID, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

// ClientCancelOrder godoc
// @Summary  Распустить состав (заказчик)
// @Description Приглашения отзываются, приглашённые получают уведомление.
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не ваш"
// @Failure      409  {object}  errorResponse  "wrong_status — заказ уже оплачен или отменён"
// @Router   /me/orders/{id}/cancel [post]
func (h *Handler) ClientCancelOrder(w http.ResponseWriter, r *http.Request) {
	orderID, ok := h.clientOrder(w, r)
	if !ok {
		return
	}
	o, err := h.svc.Cancel(r.Context(), orderID, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

// ---- креатор ----

type invitationsResp struct {
	Items []Invitation `json:"items"`
}

// CreatorInvitations godoc
// @Summary  Мои приглашения (креатор)
// @Tags     creator-orders
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} invitationsResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/creator/invitations [get]
func (h *Handler) CreatorInvitations(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	items, err := h.svc.InvitationsFor(r.Context(), uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, invitationsResp{Items: items})
}

// respondReq — ответ креатора на приглашение.
//
// Два способа в одном теле, и это не неряшливость. Старый — accept:
// именное приглашение с очередью и сроком ответа; он остаётся, пока
// живы заказы, собранные по старой логике. Новый — mode: отклик на
// рассылку, где отвечают работой, а не галочкой «согласен».
type respondReq struct {
	// Accept — старый путь: согласие или отказ по именному приглашению.
	Accept bool `json:"accept"`
	// Mode — новый путь: attach | upload | from_portfolio | decline.
	// Пусто — значит пришли по старому.
	Mode string `json:"mode"`
	// FileURL — ссылка на загруженный ролик (attach, upload).
	FileURL string `json:"file_url"`
	// PortfolioItems — что показать из уже загруженного
	// (from_portfolio).
	PortfolioItems []uuid.UUID `json:"portfolio_items"`
	// Note — пара слов менеджеру. Необязательно.
	Note string `json:"note"`
}

type responsesResp struct {
	Items []Response `json:"items"`
}

// CreatorRespond godoc
// @Summary  Ответить на приглашение (креатор)
// @Description Отказ не штраф и на место в выдаче не влияет. При отказе
// @Description приглашение немедленно уходит следующему по приоритету.
// @Tags     creator-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    order_id path string true "order id"
// @Param    body body respondReq true "согласие или отказ"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_json; bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      409  {object}  errorResponse  "not_invited — активного приглашения нет: сгорело, отозвано или адресовано другому"
// @Router   /me/creator/invitations/{order_id}/respond [post]
func (h *Handler) CreatorRespond(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	orderID, err := pathUUID(r, "order_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заказа.")
		return
	}
	var req respondReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	// Новый путь: отклик на рассылку. Он не «занимает место» и не
	// требует именного приглашения — рассылка ушла всем, и ответить
	// может кто угодно, включая тех, кого заказчик не отмечал.
	if req.Mode != "" {
		resp, err := h.svc.SaveResponse(r.Context(), ResponseInput{
			OrderID:        orderID,
			CreatorUserID:  uid,
			Mode:           ResponseMode(req.Mode),
			FileURL:        req.FileURL,
			PortfolioItems: req.PortfolioItems,
			Note:           req.Note,
		}, time.Now())
		if err != nil {
			writeErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
		return
	}

	// Права проверяются самим запросом: обновляется строка именно этого
	// креатора и только в статусе invited. Чужое приглашение или
	// сгоревшее дают ErrNotInvited, а не доступ.
	o, err := h.svc.Respond(r.Context(), orderID, uid, req.Accept, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

type availabilityReq struct {
	Month     string `json:"month"`
	Available *bool  `json:"available"`
}

// CreatorSetAvailability godoc
// @Summary  Отметить занятость в месяце (креатор)
// @Description Клиент видит занятость ещё при расстановке приоритета —
// @Description иначе первым в списке окажется тот, кто взять не может.
// @Tags     creator-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body availabilityReq true "занятость по месяцам"
// @Success  204
// @Failure      400  {object}  errorResponse  "bad_json; bad_month; invalid_input — не указано, свободны вы или нет"
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/creator/availability [put]
func (h *Handler) CreatorSetAvailability(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	var req availabilityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	month, err := time.Parse(monthLayout, req.Month)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month",
			"Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	if req.Available == nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input",
			"Не указано, свободны вы в этом месяце или нет.")
		return
	}
	if err := h.svc.SetAvailability(r.Context(), uid, month, *req.Available); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// myAvailabilityResp — своя занятость по месяцам.
type myAvailabilityResp struct {
	Items []Availability `json:"items"`
}

// CreatorGetAvailability godoc
// @Summary  Моя занятость по месяцам (креатор)
// @Description Отдаёт только те месяцы, которые вы отмечали. Не отмеченный
// @Description месяц в выдачу не попадает: «не отмечал» и «занят» — разные
// @Description вещи, и склеивать их в false нельзя.
// @Tags     creator-orders
// @Produce  json
// @Security BearerAuth
// @Param    months query int false "сколько месяцев вперёд, 1-24, по умолчанию 12"
// @Success  200 {object} myAvailabilityResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/creator/availability [get]
func (h *Handler) CreatorGetAvailability(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	months, _ := strconv.Atoi(r.URL.Query().Get("months"))
	items, err := h.svc.MyAvailability(r.Context(), uid, months)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, myAvailabilityResp{Items: items})
}

// ---- менеджер ----

// ManagerNeedingAttention godoc
// @Summary  Заказы, где нужен менеджер
// @Description Резерв кончился, а состав не собран — дальше автоматика
// @Description сделать ничего не может.
// @Tags     manager-orders
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} ordersListResp
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /manager/orders [get]
func (h *Handler) ManagerNeedingAttention(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.NeedingAttention(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ordersListResp{Items: items})
}

// ManagerProjectOrder godoc
// @Summary  Заказ, из которого вырос проект (менеджер)
// @Description Экран проекта показывает состав, но не показывает, как он
// @Description собирался: клиент присылал ПРИОРИТЕТ, а не список, и место
// @Description освобождается отказом или молчанием. Менеджеру нужна вся
// @Description очередь целиком, включая отказавшихся, — иначе непонятно,
// @Description почему в проекте четвёртый по счёту.
// @Description
// @Description Отдельной ручкой, а не фильтром /manager/orders: тот список
// @Description отдаёт только застрявшие заказы, а у проекта заказ давно
// @Description оплачен и туда не попадает.
// @Description Проект, заведённый руками, заказа не имеет — 404 здесь
// @Description означает «его и не было», а не сбой.
// @Tags     manager-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "project id"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — проект вырос не из заказа"
// @Router   /manager/projects/{id}/order [get]
func (h *Handler) ManagerProjectOrder(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id проекта.")
		return
	}
	o, err := h.svc.ByProject(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

// ManagerInvite godoc
// @Summary  Позвать следующих по приоритету (менеджер)
// @Description Обычно очередь двигается сама: отказ и сгоревшее
// @Description приглашение сразу отдают место следующему. Ручка нужна там,
// @Description где автоматике нечего было двигать — например, заказ остался
// @Description черновиком и приглашения не ушли вовсе.
// @Description Приглашение уходит только на реально свободное место
// @Description (needed − accepted − invited); если звать некого, это 409, а
// @Description не тихий успех.
// @Tags     manager-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не найден"
// @Failure      409  {object}  errorResponse  "wrong_status — заказ собран, оплачен или отменён; no_free_slot — свободных мест нет или резерв кончился"
// @Router   /manager/orders/{id}/invite [post]
func (h *Handler) ManagerInvite(w http.ResponseWriter, r *http.Request) {
	orderID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заказа.")
		return
	}
	o, err := h.svc.InviteNext(r.Context(), orderID, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

type addCandidatesReq struct {
	CreatorIDs []uuid.UUID `json:"creator_ids"`
}

// ManagerAddCandidates godoc
// @Summary  Добрать людей в подборку (менеджер)
// @Description Приоритеты продолжают существующие. Если место свободно,
// @Description приглашение уходит немедленно.
// @Tags     manager-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Param    body body addCandidatesReq true "кого добавить в резерв"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_json; bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не найден"
// @Failure      409  {object}  errorResponse  "wrong_status — заказ уже укомплектован, оплачен или отменён"
// @Router   /manager/orders/{id}/candidates [post]
func (h *Handler) ManagerAddCandidates(w http.ResponseWriter, r *http.Request) {
	orderID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заказа.")
		return
	}
	var req addCandidatesReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	o, err := h.svc.AddCandidates(r.Context(), orderID, req.CreatorIDs, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

// ManagerMarkPaid godoc
// @Summary  Отметить оплату (менеджер)
// @Description Платежей в системе нет, отметка ручная. Проект после этого
// @Description менеджер создаёт сам — авто-создания нет намеренно.
// @Tags     manager-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} Order
// @Failure      400  {object}  errorResponse  "bad_id"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не найден"
// @Failure      409  {object}  errorResponse  "wrong_status — оплатить можно только укомплектованный заказ"
// @Router   /manager/orders/{id}/paid [post]
func (h *Handler) ManagerMarkPaid(w http.ResponseWriter, r *http.Request) {
	orderID, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заказа.")
		return
	}
	o, err := h.svc.MarkPaid(r.Context(), orderID, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

type availabilityQueryReq struct {
	Month      string      `json:"month"`
	CreatorIDs []uuid.UUID `json:"creator_ids"`
}

type availabilityQueryResp struct {
	Month string      `json:"month"`
	Busy  []uuid.UUID `json:"busy"`
}

// ClientAvailability godoc
// @Summary  Кто из выбранных занят в месяце (заказчик)
// @Description Занятость нужна на карточке в выдаче: иначе первым в списке
// @Description окажется тот, кто взять не может, и заказ провисит трое суток
// @Description впустую.
// @Description
// @Description Отдельной ручкой, а не полем в поиске: поиск живёт в
// @Description OpenSearch и обновляется через outbox, а занятость меняется
// @Description каждый день и от месяца к месяцу. Держать её в индексе значило
// @Description бы переиндексировать каталог на каждую отметку креатора.
// @Description Фильтр по категориям при этом отдельной ручки не требует —
// @Description у /search он уже есть: ?category=blogger,ugc.
// @Tags     client-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body availabilityQueryReq true "месяцы, на которые смотрим"
// @Success  200 {object} availabilityQueryResp
// @Failure      400  {object}  errorResponse  "bad_json; bad_month — месяц не в формате ГГГГ-ММ; too_many_ids — больше 200 человек за раз"
// @Failure      401  {object}  errorResponse  "no_user"
// @Router   /me/orders/availability [post]
func (h *Handler) ClientAvailability(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFrom(r.Context()); !ok {
		writeNoUser(w)
		return
	}
	var req availabilityQueryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	month, err := time.Parse(monthLayout, req.Month)
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_month",
			"Месяц должен быть в формате ГГГГ-ММ.")
		return
	}
	// Потолок на размер списка: выдача постраничная, и спрашивать
	// занятость всего каталога незачем.
	if len(req.CreatorIDs) > 200 {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "too_many_ids",
			"За один раз можно спросить не больше 200 человек.")
		return
	}
	busyMap, err := h.svc.BusyCreators(r.Context(), req.CreatorIDs, month)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Порядок сохраняем как пришёл: фронту удобнее сопоставлять с выдачей.
	busy := make([]uuid.UUID, 0, len(busyMap))
	for _, id := range req.CreatorIDs {
		if busyMap[id] {
			busy = append(busy, id)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, availabilityQueryResp{
		Month: month.Format(monthLayout), Busy: busy,
	})
}

// errorResponse — тело ошибки, как его отдаёт httpx.
//
// В соседних доменах этот тип объявлен как одно поле `error`, но
// фактически WriteErrMsg кладёт ещё и `message` — человеческий текст,
// который показывает интерфейс. Описываем как есть: фронт живёт в
// отдельном репозитории и читает swagger, а не наш код.
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type reorderReq struct {
	// CreatorIDs — новый порядок неприглашённых кандидатов, целиком.
	// Именно порядок, а не состав: добавить или убрать человека здесь
	// нельзя, для этого есть свои ручки со своими проверками.
	CreatorIDs []uuid.UUID `json:"creator_ids"`
}

// ClientReorderPriority godoc
// @Summary  Переставить приоритет в заказе (клиент)
// @Description Приглашения уходят по порядку, и передумать после «отправить» —
// @Description нормальная просьба. Переставить можно только тех, кого ещё не
// @Description звали: у приглашённого уже тикает срок ответа. Занятые места
// @Description достаются тем же людям в новом порядке, поэтому перестановка
// @Description не задевает приглашённых, стоящих между ними.
// @Tags     client-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path string     true "order id"
// @Param    body body reorderReq true "новый порядок"
// @Success  200  {object} Order
// @Failure      400  {object}  errorResponse  "bad_json; bad_id; invalid_input — пустой список; priority_set_mismatch — прислан не тот набор людей"
// @Failure      401  {object}  errorResponse  "no_user"
// @Failure      404  {object}  errorResponse  "not_found — заказ не найден или он не ваш"
// @Failure      409  {object}  errorResponse  "priority_locked — заказ уже собран, оплачен или отменён"
// @Router   /me/orders/{id}/priority [put]
func (h *Handler) ClientReorderPriority(w http.ResponseWriter, r *http.Request) {
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
	var req reorderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	o, err := h.svc.ReorderReserve(r.Context(), orderID, uid, req.CreatorIDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, o)
}

// ClientBrief godoc
// @Summary  Бриф заявки (заказчик)
// @Tags     client-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} briefResp
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — заявка не ваша"
// @Router   /me/orders/{id}/brief [get]
func (h *Handler) ClientBrief(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заявки.")
		return
	}
	b, err := h.svc.Brief(r.Context(), id, uid)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, briefResp{Brief: b})
}

// ClientSaveBrief godoc
// @Summary  Дописать бриф заявки (заказчик)
// @Description Бриф правят ПОСЛЕ отправки: половина заказчиков вспоминает про
// @Description референсы уже потом. Текст переписывается целиком — стёртая
// @Description строка должна исчезать и из задания креатора.
// @Tags     client-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Param    body body briefReq true "бриф"
// @Success  200 {object} briefResp
// @Failure  400 {object} errorResponse "bad_id, bad_json"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — заявка не ваша"
// @Router   /me/orders/{id}/brief [patch]
func (h *Handler) ClientSaveBrief(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заявки.")
		return
	}
	var req briefReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	b, err := h.svc.SaveBrief(r.Context(), id, uid, req.OrderBrief)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, briefResp{Brief: b})
}

// ManagerRemoveCandidate godoc
// @Summary  Убрать человека из заявки (менеджер)
// @Description Только пока он не согласился: согласившийся уже в составе проекта,
// @Description и его выводят оттуда, а не отсюда.
// @Tags     manager-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Param    creator_id path string true "creator user id"
// @Success  200 {object} Order
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found — такого в заявке нет"
// @Failure  409 {object} errorResponse "wrong_status — человек уже согласился"
// @Router   /manager/orders/{id}/candidates/{creator_id} [delete]
func (h *Handler) ManagerRemoveCandidate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заявки.")
		return
	}
	creatorID, err := pathUUID(r, "creator_id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id креатора.")
		return
	}
	out, err := h.svc.RemoveCandidate(r.Context(), id, creatorID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ManagerResponses godoc
// @Summary  Кто откликнулся на заявку (менеджер)
// @Description Главный экран шага «собрать состав»: приглашение ушло всем
// @Description известным креаторам, и здесь видно, кто ответил работой.
// @Description Отмеченные заказчиком — сверху.
// @Tags     manager-orders
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Success  200 {object} responsesResp
// @Failure  400 {object} errorResponse "bad_id"
// @Failure  401 {object} errorResponse "no_user"
// @Router   /manager/orders/{id}/responses [get]
func (h *Handler) ManagerResponses(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заявки.")
		return
	}
	items, err := h.svc.Responses(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, responsesResp{Items: items})
}

// finalizeReq — что утверждает менеджер.
type finalizeReq struct {
	// CreatorIDs — состав. Может быть пустым: «утверждаю объём, людей
	// добавлю позже» — это тоже решение.
	CreatorIDs []uuid.UUID `json:"creator_ids"`
	// MonthlyPlan — сколько роликов в месяц по договорённости. Ноль —
	// не трогать: значит, о числе не договаривались.
	MonthlyPlan int `json:"monthly_plan"`
}

// ManagerFinalize godoc
// @Summary  Утвердить состав и объём заявки (менеджер)
// @Description Проект, чеклист и условия оплаты уже есть — заявка их не
// @Description создаёт. Здесь только то, что решается по телефону: кого
// @Description берём и сколько роликов в месяце. Добавленные получают
// @Description задание проекта — договор, ТЗ и чеклист, — поэтому
// @Description материалы должны быть на месте ДО финализации.
// @Tags     manager-orders
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "order id"
// @Param    body body finalizeReq true "состав и объём"
// @Success  200 {object} FinalizeResult
// @Failure  400 {object} errorResponse "bad_id; bad_json; invalid_input; duplicate_candidate"
// @Failure  401 {object} errorResponse "no_user"
// @Failure  404 {object} errorResponse "not_found"
// @Failure  409 {object} errorResponse "wrong_status — заявка отменена; no_project — у заявки нет проекта"
// @Router   /manager/orders/{id}/finalize [post]
func (h *Handler) ManagerFinalize(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserIDFrom(r.Context())
	if !ok {
		writeNoUser(w)
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_id", "Неверный id заявки.")
		return
	}
	var req finalizeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Не удалось разобрать тело запроса.")
		return
	}
	out, err := h.svc.Finalize(r.Context(), id, req.CreatorIDs, req.MonthlyPlan, uid, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
