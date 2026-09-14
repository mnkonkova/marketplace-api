package projects

import (
	"time"

	"github.com/google/uuid"
)

// PartyContact — контакты одного участника проекта (клиента или спеца).
// Все поля опциональные: показываем только заполненное. email берём из
// users.email, phone/telegram — из client_profiles либо specialist_profiles
// (см. manager_repo.LoadProjectParties).
type PartyContact struct {
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Telegram    string `json:"telegram,omitempty"`
}

// ProjectManagerView — карточка проекта для канбана менеджера. Кроме самого
// Project, тащит: текущую стадию (для расчёта колонки канбана), display_status,
// прогресс и кого ждём. Намеренно без полного дерева шагов — лёгкая выдача.
type ProjectManagerView struct {
	Project
	ClientDisplayName string        `json:"client_display_name,omitempty"`
	Client            *PartyContact `json:"client,omitempty"`
	Specialist        *PartyContact `json:"specialist,omitempty"`
	// ManagerDisplayName — ответственный менеджер. Без него по списку не
	// ответить на вторую половину вопроса «что не двигалось» — «и кто за
	// это отвечает»: assigned_to_user_id сам по себе ничего не говорит.
	ManagerDisplayName string               `json:"manager_display_name,omitempty"`
	DisplayStatus      ProjectDisplayStatus `json:"display_status"`
	Progress           float64              `json:"progress"`

	// Прогресс числом, а не только процентом.
	//
	// «62%» в списке не отвечает на вопрос, с которым в него смотрят:
	// сколько роликов ещё выложить и сколько шагов осталось пройти.
	// Единица измерения у видов проектов разная, поэтому рядом с числами
	// лежит ProgressUnit — иначе «3 из 5» читалось бы как что угодно.
	ProgressDone  int `json:"progress_done"`
	ProgressTotal int `json:"progress_total"`
	// ProgressUnit — publications у креаторов, steps у продакшна.
	// Пусто означает, что мерить нечего: ни выкладок, ни шагов.
	ProgressUnit string `json:"progress_unit,omitempty"`

	// CurrentStageID/Title — куда положить карточку в канбане.
	CurrentStageID    *uuid.UUID `json:"current_stage_id,omitempty"`
	CurrentStageName  string     `json:"current_stage_name,omitempty"`
	CurrentStageOrder int        `json:"current_stage_order"`

	CurrentStepID     *uuid.UUID `json:"current_step_id,omitempty"`
	CurrentStepTitle  string     `json:"current_step_title,omitempty"`
	CurrentStepOwner  string     `json:"current_step_owner,omitempty"`
	CurrentStepStatus StepStatus `json:"current_step_status,omitempty"`
}

// Порядок админского списка.
//
// Таблица в админке просит updated_asc — «самые давно обновлённые
// сверху»: её открывают, чтобы увидеть, что неделю не двигалось. Сама
// ручка без параметра остаётся на updated_desc: тем же списком живут
// канбан и сводка, и менять им порядок молча нельзя.
const (
	AdminSortUpdatedAsc  = "updated_asc"
	AdminSortUpdatedDesc = "updated_desc"
	AdminSortCreatedAsc  = "created_asc"
	AdminSortCreatedDesc = "created_desc"
)

// AdminStatusUnfinished — единственное неточное значение фильтра status:
// «в работе», то есть четыре незавершённых статуса разом.
//
// Нужно, чтобы цифра «Проекты N» в меню равнялась числу строк, которые
// человек увидит, кликнув. Счётчик считает именно этот набор, а выразить
// его в списке было нечем: точное значение задаёт ровно один статус, а
// пустое означает «всё, кроме отменённых» — вместе с завершёнными.
// Фильтровать на клиенте нельзя: поедут пагинация и total.
const AdminStatusUnfinished = "unfinished"

// unfinishedStatusesSQL — что считается «в работе». Тот же набор, что у
// нагрузки менеджеров, у проверки при снятии роли и у счётчика
// nav_counts.projects_active в сводке: разойдись они — и два экрана
// рядом показывали бы разные числа про одно и то же.
const unfinishedStatusesSQL = `('draft','active','on_hold','dispute')`

// AdminListParams — поиск, фильтры, сортировка и страница для
// GET /admin/projects. Все поля опциональны; Limit clamp'ится 1..1000.
type AdminListParams struct {
	// Q — ILIKE по названию проекта и по клиенту (имя в профиле, имя на
	// проекте у клиента без аккаунта, почта). Короче 2 символов — игнор.
	Q string
	// Status — точный статус проекта, либо AdminStatusUnfinished — «в
	// работе» (четыре незавершённых статуса). Пусто = всё, кроме
	// cancelled, то есть вместе с завершёнными.
	Status string
	// ManagerID — ответственный менеджер. Unassigned — проекты вообще без
	// ответственного; отдельным флагом, потому что «никто» нельзя выразить
	// значением UUID.
	ManagerID  *uuid.UUID
	Unassigned bool
	// IncludeTest — показывать проекты с is_test. По умолчанию скрыты.
	IncludeTest bool
	// Kind — вид проекта: creators_turnkey | production_turnkey | general.
	// Пусто = все. Вид определяет и колонки таблицы, и смысл прогресса,
	// поэтому смешанный список админ почти всегда сужает до одного вида.
	Kind   string
	Sort   string
	Limit  int
	Offset int
}

// ProgressUnit* — в чём измерен прогресс проекта (см. ProjectManagerView).
const (
	ProgressUnitPublications = "publications"
	ProgressUnitSteps        = "steps"
)

// AdminListResult — страница админского списка. Total — количество под
// текущими фильтрами без limit/offset, для пагинатора на фронте.
type AdminListResult struct {
	Items  []ProjectManagerView `json:"items"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

// ProjectFullView — детальная страница проекта в кабинете менеджера/админа.
// Включает все шаги (включая скрытые от клиента), стадии с их display_status,
// и события (в Ф7 — лента активности).
type ProjectFullView struct {
	Project
	Client     *PartyContact `json:"client,omitempty"`
	Specialist *PartyContact `json:"specialist,omitempty"`
	// ProposedSpecialist — кого клиент выбрал в брифе (lead_recipient_specialist_id);
	// пока не подтверждён менеджером (Specialist=nil). Фронт показывает блок
	// «Клиент выбрал …» c кнопками Approve/Reject.
	ProposedSpecialist *PartyContact        `json:"proposed_specialist,omitempty"`
	DisplayStatus      ProjectDisplayStatus `json:"display_status"`
	Progress           float64              `json:"progress"`
	CurrentStepID      *uuid.UUID           `json:"current_step_id,omitempty"`
	CurrentStepTitle   string               `json:"current_step_title,omitempty"`
	CurrentStepOwner   string               `json:"current_step_owner,omitempty"`
	CurrentStepStatus  StepStatus           `json:"current_step_status,omitempty"`
	Stages             []StageView          `json:"stages"`
}

// ManagerPatchInput — поля, которые менеджер правит inline на карточке.
// UpdatedAt — optimistic-lock версия projects.updated_at.
type ManagerPatchInput struct {
	Title     *string    `json:"title,omitempty"`
	Budget    *int       `json:"budget,omitempty"`
	Notes     *string    `json:"notes,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}
