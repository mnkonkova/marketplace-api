package projects

import (
	"time"

	"github.com/google/uuid"
)

// Общий проект — третий вид из бизнес-требований и самый короткий путь:
// клиент сам выбирает исполнителя, ставит один срок, задача уходит
// исполнителю в бот. Ни воронки, ни выкладок, ни статистики, ни менеджера.
//
// Отдельной таблицы у него нет: вид задан projects.kind, исполнитель —
// specialist_user_id, срок — due_date. Своё здесь только одно — журнал
// сдач general_deliveries.

// Решения по сданной работе.
const (
	DeliveryPending  = "pending"
	DeliveryAccepted = "accepted"
	DeliveryRework   = "rework"
)

// GeneralProject — карточка общего проекта. Одна и та же и для клиента,
// и для исполнителя: скрывать в ней друг от друга нечего — они вдвоём и
// есть весь проект.
type GeneralProject struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	// Brief — что нужно сделать. Лежит в projects.notes.
	Brief  string `json:"brief"`
	Status string `json:"status"`
	// DueDate — единственный срок проекта.
	DueDate time.Time `json:"due_date"`
	// Overdue — срок прошёл, а работа не принята. Считается на сервере,
	// чтобы клиент и исполнитель видели одно и то же независимо от
	// часового пояса браузера.
	Overdue bool `json:"overdue"`
	Budget  *int `json:"budget,omitempty"`

	ClientID   uuid.UUID `json:"client_user_id"`
	ClientName string    `json:"client_name,omitempty"`
	// SpecialistID — исполнитель, которого выбрал клиент.
	SpecialistID   uuid.UUID `json:"specialist_user_id"`
	SpecialistName string    `json:"specialist_name,omitempty"`

	// RevisionsIncluded/Used — сколько раз работу можно вернуть на
	// доработку и сколько раз уже возвращали.
	RevisionsIncluded int `json:"revisions_included"`
	RevisionsUsed     int `json:"revisions_used"`

	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// Deliveries — журнал сдач, от первой к последней. Пусто, пока
	// исполнитель ничего не сдал.
	Deliveries []Delivery `json:"deliveries,omitempty"`
}

// Delivery — одна сдача работы.
type Delivery struct {
	ID uuid.UUID `json:"id"`
	// Attempt — номер попытки, 1 для первой сдачи.
	Attempt     int       `json:"attempt"`
	SubmittedBy uuid.UUID `json:"submitted_by"`
	SubmittedAt time.Time `json:"submitted_at"`
	Note        string    `json:"note,omitempty"`
	// Decision — pending, пока клиент не ответил.
	Decision     string     `json:"decision"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	ReworkReason string     `json:"rework_reason,omitempty"`
	// Materials — что приложено именно к этой сдаче.
	Materials []Material `json:"materials,omitempty"`
}

// Material — файл или ссылка. Ложится в общий для всех видов проекта
// project_materials; delivery_id связывает его с конкретной сдачей.
type Material struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind"`
	Title string    `json:"title"`
	URL   string    `json:"url"`
}

// MaterialInput — то же на входе.
type MaterialInput struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// CreateGeneralInput — «сделай это к такому-то числу».
type CreateGeneralInput struct {
	ClientID     uuid.UUID
	SpecialistID uuid.UUID
	Title        string
	Brief        string
	DueDate      time.Time
	Budget       *int
}

// DeliverInput — сдача работы исполнителем.
type DeliverInput struct {
	ProjectID uuid.UUID
	UserID    uuid.UUID
	Note      string
	Materials []MaterialInput
}
