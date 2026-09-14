package orders

import (
	"time"

	"github.com/google/uuid"
)

// OrderStatus — жизненный цикл заказа на подбор.
type OrderStatus string

const (
	StatusDraft     OrderStatus = "draft"
	StatusInviting  OrderStatus = "inviting"
	StatusStaffed   OrderStatus = "staffed"
	StatusPaid      OrderStatus = "paid"
	StatusCancelled OrderStatus = "cancelled"
)

// CandidateStatus — состояние человека в подборке.
type CandidateStatus string

const (
	CandidateReserve  CandidateStatus = "reserve"
	CandidateInvited  CandidateStatus = "invited"
	CandidateAccepted CandidateStatus = "accepted"
	CandidateDeclined CandidateStatus = "declined"
	CandidateExpired  CandidateStatus = "expired"
)

// Правила объёма: первый месяц — один креатор, дальше 2–3.
//
// Считается по клиенту и по ЗАВЕРШЁННЫМ оплаченным месяцам, а не по факту
// первой оплаты. Иначе ограничение обходится за минуту: клиент платит за
// одного, тут же добирает троих и уходит с четырьмя на первом же месяце.
const (
	FirstMonthCreators = 1
	// MinCreators — нижняя граница со второго месяца. Показывается
	// клиенту («доступно 2–3»), но НЕ запрещает взять одного: правило
	// написано как предложение объёма, а не как запрет работать меньше,
	// и отказывать человеку, которому нужен один ролик в месяц, было бы
	// странно. Захотите запрет — это одна строка в Create.
	MinCreators = 2
	MaxCreators = 3
)

// InviteTTL — сколько живёт приглашение. Не ответил — сгорает, место
// освобождается, приглашение уходит следующему по приоритету.
const InviteTTL = 72 * time.Hour

// ManagerPingAfter — через сколько молчания сказать менеджеру, что человек
// не отвечает. Самому креатору второй раз не пишем: его уже позвали.
const ManagerPingAfter = 24 * time.Hour

// Order — заказ на подбор креаторов.
type Order struct {
	ID             uuid.UUID   `json:"id"`
	ClientUserID   uuid.UUID   `json:"client_user_id"`
	StartMonth     time.Time   `json:"start_month"`
	Needed         int         `json:"needed"`
	VideosCount    int         `json:"videos_count"`
	Status         OrderStatus `json:"status"`
	TermsVersionID uuid.UUID   `json:"terms_version_id"`
	ProjectID      *uuid.UUID  `json:"project_id,omitempty"`
	PaidAt         *time.Time  `json:"paid_at,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`

	Candidates []Candidate `json:"candidates"`

	// Accepted — сколько уже согласилось. Считается, а не хранится.
	Accepted int `json:"accepted"`
	// NeedMore — сколько ещё не хватает до укомплектованного состава.
	NeedMore int `json:"need_more"`
	// ReserveLeft — сколько людей ещё можно позвать без участия клиента.
	// Ноль при NeedMore > 0 — это и есть момент «добрать», когда
	// подключается менеджер.
	ReserveLeft int `json:"reserve_left"`
}

// Candidate — человек в подборке.
type Candidate struct {
	OrderID       uuid.UUID `json:"order_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	// CreatorName — человеческое имя вместо uuid: та же лесенка
	// specialist → client → префикс email, что в остальной выдаче.
	CreatorName string          `json:"creator_name,omitempty"`
	Priority    int             `json:"priority"`
	Status      CandidateStatus `json:"status"`
	InvitedAt   *time.Time      `json:"invited_at,omitempty"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	RespondedAt *time.Time      `json:"responded_at,omitempty"`
}

// CreateOrderInput — что нужно, чтобы завести заказ.
type CreateOrderInput struct {
	ClientUserID uuid.UUID
	StartMonth   time.Time
	Needed       int
	VideosCount  int
	// CreatorIDs — подборка В ПОРЯДКЕ ПРИОРИТЕТА: первый в списке —
	// первый по приоритету. Клиент не «выбирает N человек», а расставляет
	// собранных по порядку, и порядок пропустить нельзя.
	CreatorIDs []uuid.UUID
}

// Terms — версия правил работы.
//
// Кроме текста, с которым клиент соглашается, здесь лежит и сам тариф.
// Раньше было только тело: карточку «Оклад 60 000, 90 ₽/1000, порог
// 1 000 000» собрать из текстового блока нельзя, а показывать её надо
// до создания заказа — то есть тогда, когда проекта ещё нет и взять
// числа больше неоткуда. Все суммы в копейках.
type Terms struct {
	ID          uuid.UUID `json:"id"`
	Version     int       `json:"version"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`

	// SalaryPerMonth — оклад за месяц; VideosFirstMonth/VideosNextMonths —
	// за какой объём он назван.
	SalaryPerMonth   int64 `json:"salary_per_month"`
	VideosFirstMonth int   `json:"videos_first_month"`
	VideosNextMonths int   `json:"videos_next_months"`
	// Ставка ступенчатая: до порога на ролик — полная, свыше — пониженная.
	RatePer1000Views     int64 `json:"rate_per_1000_views"`
	BonusViewsThreshold  int64 `json:"bonus_views_threshold"`
	RatePer1000ViewsOver int64 `json:"rate_per_1000_views_over"`
	// Переходы: порог месячный. nil в ставке = не считаем.
	ClickBonusRate      *int64 `json:"click_bonus_rate,omitempty"`
	ClickBonusThreshold int    `json:"click_bonus_threshold"`
	ClickBonusRateOver  *int64 `json:"click_bonus_rate_over,omitempty"`
}

// Availability — занятость креатора в месяце.
type Availability struct {
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	Month         time.Time `json:"month"`
	IsAvailable   bool      `json:"is_available"`
}

// firstOfMonth — первое число месяца. В БД стоит CHECK на это: иначе
// «сентябрь» и «сентябрь с 3-го» окажутся разными месяцами при сравнении
// занятости.
func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
