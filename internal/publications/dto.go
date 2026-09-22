package publications

import (
	"time"

	"github.com/google/uuid"
)

// Status — жизненный цикл выкладки (enum publication_status в 00032).
//
//	planned         → дата стоит, ссылок нет
//	partial         → пришла часть площадок
//	done            → пришли все пять
//	closed_manually → менеджер закрыл неполную выкладку с причиной
//	cancelled       → выкладка отменена
type Status string

const (
	StatusPlanned        Status = "planned"
	StatusPartial        Status = "partial"
	StatusDone           Status = "done"
	StatusClosedManually Status = "closed_manually"
	StatusCancelled      Status = "cancelled"
)

// IsOpen — по выкладке ещё ждут ссылок. Только такие попадают в напоминания.
func (s Status) IsOpen() bool { return s == StatusPlanned || s == StatusPartial }

// Publication — выкладка: кто, когда и что сдал.
type Publication struct {
	ID            uuid.UUID `json:"id"`
	ProjectID     uuid.UUID `json:"project_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	// CreatorName — человеческое имя вместо uuid. Считается в выдаче, а не
	// хранится: имя живёт в профиле и меняется там.
	CreatorName string `json:"creator_name,omitempty"`
	// Title — название ролика. Пишет креатор в момент сдачи: он
	// единственный, кто знает, что снял. У запланированной выкладки
	// названия ещё нет, и это нормально — тему задаёт дата.
	Title        string     `json:"title,omitempty"`
	DueDate      time.Time  `json:"due_date"`
	DraftDueDate *time.Time `json:"draft_due_date,omitempty"`
	Status       Status     `json:"status"`
	ClosedBy     *uuid.UUID `json:"closed_by,omitempty"`
	CloseReason  string     `json:"close_reason,omitempty"`
	BatchID      *uuid.UUID `json:"batch_id,omitempty"`
	// SelfAdded — выкладку завёл себе сам креатор, а не менеджер.
	//
	// Менеджеру это видно в списке и в карточке, чтобы он не искал в
	// своих пачках ролик, которого туда не ставил. Считается признак не
	// для отчётности: самодобавленные не участвуют в знаменателе
	// недосдачи — иначе кнопка «добрать до ступени» уменьшала бы оклад
	// тому, кто её нажал.
	SelfAdded bool      `json:"self_added,omitempty" extensions:"x-omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Links []SubmittedLink `json:"links"`

	// Views/Likes/Comments — сумма по всем площадкам выкладки, по
	// последнему снимку каждой ссылки. Порог «миллион просмотров» считается
	// по ролику целиком, а не по отдельной площадке, поэтому суммой этой
	// пользуются и уведомления, и интерфейс — собирать её на фронте из
	// videos_table значило бы держать вторую реализацию того же правила.
	Views    int64 `json:"views"`
	Likes    int64 `json:"likes"`
	Comments int64 `json:"comments"`
	// Shares — репосты по всем площадкам выкладки. Пусто, если хоть одна
	// площадка их не отдаёт: ноль означал бы «репостов нет».
	Shares *int64 `json:"shares,omitempty"`
	// ERPercent — вовлечённость: (лайки + комментарии + репосты) ÷
	// просмотры, в процентах.
	ERPercent *float64 `json:"er_percent,omitempty"`
	// ERWithoutShares — посчитана без репостов: их не отдала хотя бы одна
	// площадка либо ролик собирали до того, как мы начали их писать.
	// Прошлое не пересчитывается, поэтому у старых роликов признак
	// останется навсегда — и это честнее скачка на графике.
	ERWithoutShares bool `json:"er_without_shares,omitempty"`
	// StatsCollectedAt — самый свежий сбор среди площадок выкладки.
	StatsCollectedAt *time.Time `json:"stats_collected_at,omitempty"`
	// PublishedAt — когда ролик вышел: самое раннее известное среди
	// площадок выкладки. Площадки выкладывают не одновременно, и «вышел»
	// — это первая из них; на этой дате будет стоять возраст ролика и
	// правило «зрелый» (14 дней).
	//
	// nil означает «не знаем»: ни одна площадка даты не отдала или ролик
	// ещё не собирали. Подставлять сюда дату сдачи ссылок нельзя — сдают
	// и через неделю после выхода.
	PublishedAt *time.Time `json:"published_at,omitempty"`

	// Overdue — вычисляется, а не хранится: просрочка зависит от текущей
	// даты и от того, не висит ли просьба о переносе. Хранить такое поле
	// значит держать фоновую задачу, которая его переписывает каждую ночь.
	Overdue bool `json:"overdue"`
	// PendingDateRequest — непринятая просьба о переносе. Пока она есть,
	// выкладка не считается просроченной и пинги по ней не идут.
	PendingDateRequest *DateRequest `json:"pending_date_request,omitempty"`
}

// SubmittedLink — сданная ссылка на одну площадку.
type SubmittedLink struct {
	ID              uuid.UUID  `json:"id"`
	PublicationID   uuid.UUID  `json:"publication_id"`
	Platform        string     `json:"platform"`
	URL             string     `json:"url"`
	URLCanonical    string     `json:"url_canonical"`
	ExternalMediaID string     `json:"external_media_id,omitempty"`
	SubmittedAt     time.Time  `json:"submitted_at"`
	LastCollectedAt *time.Time `json:"last_collected_at,omitempty"`
	// PublishedAt — когда ролик вышел на этой площадке, по данным
	// сборщика. nil, пока площадка даты не отдала.
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// MissingPlatforms — каких площадок ещё нет. Именно этот список показывается
// креатору и уходит в отдельное напоминание «вышло, но не все площадки».
func (p Publication) MissingPlatforms() []string {
	have := make(map[string]bool, len(p.Links))
	for _, l := range p.Links {
		have[l.Platform] = true
	}
	missing := make([]string, 0, len(AllPlatforms))
	for _, pl := range AllPlatforms {
		if !have[pl] {
			missing = append(missing, pl)
		}
	}
	return missing
}

// DateRequest — просьба креатора перенести дедлайн. Дедлайн ставит менеджер,
// креатор его не меняет — только просит (требование К3).
type DateRequest struct {
	ID            uuid.UUID  `json:"id"`
	PublicationID uuid.UUID  `json:"publication_id"`
	RequestedDate time.Time  `json:"requested_date"`
	Reason        string     `json:"reason"`
	Status        string     `json:"status"`
	DecidedBy     *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

const (
	DateRequestPending  = "pending"
	DateRequestApproved = "approved"
	DateRequestRejected = "rejected"
)

// ChecklistItem — пункт чеклиста в снимке проекта.
type ChecklistItem struct {
	ID         uuid.UUID `json:"id"`
	ProjectID  uuid.UUID `json:"project_id"`
	Text       string    `json:"text"`
	Platform   *string   `json:"platform,omitempty"`
	IsRequired bool      `json:"is_required"`
	SortOrder  int       `json:"sort_order"`
	// AddedForProject — пункт завёл менеджер под этот проект, а не
	// скопирован из библиотеки. Обновление шаблона такие пункты не
	// трогает, и на экране они отличаются: иначе менеджер не знает, что
	// уцелеет при обновлении версии.
	AddedForProject bool `json:"added_for_project"`
}

// AppliesTo — пункт относится к этой площадке. Общий пункт (Platform == nil)
// относится ко всем.
func (c ChecklistItem) AppliesTo(platform string) bool {
	return c.Platform == nil || *c.Platform == platform
}

// CreateBatchInput — массовая простановка дат. Менеджер выбирает креаторов
// и дни, а не заводит выкладки по одной: 30 штук по одной — недопустимо
// (требование М2).
type CreateBatchInput struct {
	ProjectID      uuid.UUID
	CreatorUserIDs []uuid.UUID
	Dates          []time.Time
	// DraftLeadDays — за сколько дней до публикации сдать черновик.
	// Учитывается, только если у проекта draft_required.
	DraftLeadDays int
	CreatedBy     uuid.UUID
}

// BatchResult — что получилось из пачки.
type BatchResult struct {
	BatchID uuid.UUID     `json:"batch_id"`
	Created int           `json:"created"`
	Items   []Publication `json:"items"`
}

// SubmitLinksInput — креатор сдаёт ролик ссылками. Можно сдать не все
// площадки сразу и дослать остальные позже.
type SubmitLinksInput struct {
	PublicationID uuid.UUID
	// ActorUserID — кто сдаёт. Должен совпадать с креатором выкладки.
	ActorUserID uuid.UUID
	URLs        []string
	// Title — название ролика. Пустое не стирает уже записанное: креатор
	// досылает площадки по одной, и второй заход без поля не должен
	// обнулять то, что он вписал в первый.
	Title string
	// CheckedItemIDs — отмеченные пункты чеклиста. Обязательные пункты,
	// которых здесь нет, блокируют сдачу.
	CheckedItemIDs []uuid.UUID
}

// CloseManuallyInput — менеджер закрывает неполную выкладку.
type CloseManuallyInput struct {
	PublicationID uuid.UUID
	ManagerUserID uuid.UUID
	Reason        string
}
