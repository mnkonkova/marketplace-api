package orders

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// OrderStatus — жизненный цикл заказа на подбор.
type OrderStatus string

const (
	StatusDraft OrderStatus = "draft"
	// StatusSubmitted — заявка отправлена: заказ и проект заведены,
	// менеджер считает. Между «нажал отправить» и «оплачено» у заказа
	// раньше не было имени вовсе, и заказчик всё это время висел в
	// тишине.
	StatusSubmitted OrderStatus = "submitted"
	StatusInviting  OrderStatus = "inviting"
	StatusStaffed   OrderStatus = "staffed"
	// StatusFinalized — менеджер утвердил состав, цену и даты. Дальше
	// живёт проект, а заказ становится историей сделки.
	StatusFinalized OrderStatus = "finalized"
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
	// CandidateResponded — креатор откликнулся на рассылку: прислал файл
	// или указал свои ролики. Это НЕ «согласился»: согласие
	// подтверждает менеджер, когда добавляет человека в состав.
	CandidateResponded CandidateStatus = "responded"
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
	CreatorName string `json:"creator_name,omitempty"`
	// IsPreferred — заказчик отметил этого человека: «хочу особенно
	// его». Это НЕ приоритет очереди — очереди больше нет, приглашение
	// уходит всем сразу. Отметка идёт в текст приглашения и в список у
	// менеджера, когда он собирает состав из откликнувшихся.
	IsPreferred bool `json:"is_preferred"`
	// Priority — порядок строк, в котором заказчик их отметил. Смысла
	// очереди у него больше нет; оставлен как устойчивая сортировка,
	// чтобы список не прыгал между запросами.
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
	// CreatorIDs — кого заказчик отметил.
	//
	// Раньше это была подборка В ПОРЯДКЕ ПРИОРИТЕТА, и порядок был
	// смыслом: приглашения уходили сверху вниз по одному на свободное
	// место. Очереди больше нет — приглашение уходит всем известным
	// креаторам, а отмеченные получают его с пометкой «вас хотят
	// особенно». Порядок остался только как порядок строк на экране.
	CreatorIDs []uuid.UUID
	// Brief — ответы на пять вопросов первого шага воронки. Пустой бриф
	// не запрещён: человек может дописать его после отправки, и лучше
	// пустой бриф с заведённым проектом, чем форма, которую бросили.
	Brief OrderBrief
	// Ceiling — потолок, который заказчик УВИДЕЛ на баре, в копейках.
	//
	// Не пересчитываем его на сервере: менеджеру нужно знать не «сколько
	// вышло бы сейчас», а с каким числом в голове человек нажал
	// «Отправить». Прайс мог смениться между показом и отправкой, и
	// разговор начнётся с чужой суммы.
	//
	// Ноль допустим: старый фронт его не шлёт, и заявка от этого не
	// становится хуже — в сообщении менеджеру строки просто не будет.
	Ceiling int64
}

// OrderBrief — бриф заказчика: что снимаем, кому и каким тоном.
//
// Отдельным типом, а не набором полей заказа: заказ — это состояние
// сделки, бриф — текст, который правят. Правка текста не должна трогать
// строку, по которой считаются деньги и статусы.
type OrderBrief struct {
	// Goal — зачем снимаем: «продажи», «узнаваемость», «прогрев к
	// запуску». С этого менеджер начинает разговор.
	Goal string `json:"goal"`
	// Product — что продвигаем.
	Product string `json:"product"`
	// Audience — кому.
	Audience string `json:"audience"`
	// Tone — каким тоном: «по-дружески», «экспертно», «без юмора».
	Tone string `json:"tone"`
	// Refs — на что равняться: ссылки на ролики, которые нравятся.
	Refs string `json:"refs"`
	// Platforms — площадки. Пусто = все пять: так работает пакет по
	// умолчанию, и заставлять отмечать их ради этого незачем.
	Platforms []string `json:"platforms,omitempty"`
}

// Filled — в брифе есть хоть что-то. Нужен там, где пустой бриф
// показывать нечем: «Бриф: —» занимает место и ничего не сообщает.
func (b OrderBrief) Filled() bool {
	return b.Goal != "" || b.Product != "" || b.Audience != "" ||
		b.Tone != "" || b.Refs != "" || len(b.Platforms) > 0
}

// Text — бриф одним куском для заметок проекта и сообщения в чат.
//
// Собирается здесь, а не в вызывающем коде: мест, где бриф надо
// прочитать человеку, уже три (заметки проекта, карточка менеджера,
// сообщение в чат), и три разные склейки разошлись бы на первой же
// новой строке брифа.
func (b OrderBrief) Text() string {
	parts := make([]string, 0, 6)
	add := func(label, v string) {
		if v != "" {
			parts = append(parts, label+": "+v)
		}
	}
	add("Задача", b.Goal)
	add("Продукт", b.Product)
	add("Аудитория", b.Audience)
	add("Тон", b.Tone)
	add("Референсы", b.Refs)
	if len(b.Platforms) > 0 {
		parts = append(parts, "Площадки: "+strings.Join(b.Platforms, ", "))
	}
	return strings.Join(parts, "\n")
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
	// FeePerVideo — фикс за ВЫШЕДШИЙ РОЛИК. nil = версия старая, фикс
	// платится окладом за период.
	//
	// Это экран, на котором человек НАЖИМАЕТ «согласен», и цена здесь
	// обязана быть той, по которой ему выставят счёт. Без этого поля
	// условия показывали оклад за месяц, а счёт приходил за ролики —
	// причём числа расходились вдвое.
	FeePerVideo *int64 `json:"fee_per_video,omitempty"`
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
