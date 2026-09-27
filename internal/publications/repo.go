package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketpclce/internal/outbox"
	"marketpclce/internal/projects"
)

var (
	ErrNotFound = errors.New("publication not found")
	// ErrForbidden — сдавать ролик может только тот креатор, на кого
	// заведена выкладка. Проверка на бэке, а не только в UI.
	ErrForbidden = errors.New("publication belongs to another creator")
	// ErrNotCreatorsProject — выкладок у этого вида проекта не бывает.
	// У продакшна воронка, у общего проекта один срок; план выкладок
	// есть только у «креаторов под ключ» и «бренда под ключ».
	ErrNotCreatorsProject = errors.New("project kind has no publications")
	// ErrNoCreator — действие требует креатора, а у выкладки его нет:
	// это ролик проекта без людей. Не ErrForbidden и не ErrNotFound —
	// оба врут о причине: выкладка существует и видна, просто сдавать,
	// проверять и напоминать по ней некому.
	ErrNoCreator = errors.New("publication has no creator")
	// ErrCrewNotAllowed — в проект без креаторов пытаются добавить
	// состав. Пустой состав здесь не «ещё никого не добавили», а
	// «не будет никогда».
	ErrCrewNotAllowed = errors.New("project kind has no crew")
	// ErrCreatorNotInProject — креатора нет в составе проекта (или его
	// оттуда убрали).
	ErrCreatorNotInProject = errors.New("creator is not in project")
	// ErrPublicationClosed — по отменённой или закрытой руками выкладке
	// досылать ссылки нельзя.
	ErrPublicationClosed = errors.New("publication is closed")
	// ErrChecklistIncomplete — не отмечены обязательные пункты.
	ErrChecklistIncomplete = errors.New("required checklist items are not checked")
	// ErrAlreadyRequested — по выкладке уже висит непринятая просьба
	// о переносе.
	ErrAlreadyRequested = errors.New("pending date request already exists")
	ErrNothingToCreate  = errors.New("nothing to create: no creators or no dates")
	// ErrNotACreator — в состав проекта пытаются добавить того, кто не
	// может быть креатором: заказчика, менеджера, админа или
	// деактивированного пользователя. Без этой проверки такой человек
	// получал бы доступ к выкладкам проекта наравне с исполнителями.
	ErrNotACreator = errors.New("user cannot be a project creator")
	// ErrDayTaken — на этот день у креатора уже есть выкладка. В базе это
	// ловит publications_creator_day_uniq; отдельная ошибка нужна, чтобы
	// человеку ответили «на эту дату у вас уже есть выкладка», а не
	// пятисоткой из драйвера.
	ErrDayTaken = errors.New("creator already has a publication on that day")
	// ErrPublicationStarted — по выкладке уже сдавали ссылки: двигать её
	// дату или снимать её нельзя. Работа креатора не исчезает из-за
	// правки календаря, а «перенос» ролика, который уже вышел, переписал
	// бы историю периода задним числом.
	ErrPublicationStarted = errors.New("publication already has links")
	// ErrPeriodLocked — день попадает в подытоженный период. Такой период
	// заморожен вместе со срезом и суммами: добавить в него ролик значит
	// поменять то, по чему уже выставлен счёт.
	ErrPeriodLocked = errors.New("period is already locked")
	// ErrLinkRemoveDenied — креатор пытается снять ссылку, а не заменить.
	// Пустой адрес означает «этой площадки не было» — решение о работе, а
	// не о ссылке, и принимает его менеджер.
	ErrLinkRemoveDenied = errors.New("creator cannot remove a link")
)

// PublicationBelongsTo — его ли это выкладка. Проверка отдельным
// запросом, а не условием в UPDATE: «не ваше» и «нет такой» должны
// отвечать одинаково, и различить их надо ДО правки.
func (r *Repo) PublicationBelongsTo(ctx context.Context, pubID, creatorID uuid.UUID) (bool, error) {
	var mine bool
	if err := r.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_publications
    WHERE id = $1 AND creator_user_id = $2
)`, pubID, creatorID).Scan(&mine); err != nil {
		return false, fmt.Errorf("check publication owner: %w", err)
	}
	return mine, nil
}

type Repo struct{ db *pgxpool.Pool }

func NewRepo(db *pgxpool.Pool) *Repo { return &Repo{db: db} }

// ---- состав проекта ----

// AddCreator — добавить креатора в проект. Повторный вызов возвращает
// человека в состав (снимает removed_at), а не падает: менеджер может
// убрать и вернуть того же человека.
func (r *Repo) AddCreator(ctx context.Context, projectID, creatorID, addedBy uuid.UUID) error {
	if err := r.assertCanBeCreator(ctx, creatorID); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
INSERT INTO project_creators (project_id, creator_user_id, added_by)
VALUES ($1, $2, $3)
ON CONFLICT (project_id, creator_user_id)
DO UPDATE SET removed_at = NULL, added_at = now(), added_by = EXCLUDED.added_by`
	if _, err := tx.Exec(ctx, q, projectID, creatorID, addedBy); err != nil {
		return fmt.Errorf("add creator: %w", err)
	}
	// Задание проекта человек получает в момент, когда его в проект
	// добавили, а не когда он сам догадается открыть вкладку.
	if err := notifyNewCreator(ctx, tx, projectID, creatorID, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// RemoveCreator — убрать креатора из проекта мягко. Сданные выкладки и его
// цифры в отчёте остаются: они часть истории проекта.
func (r *Repo) RemoveCreator(ctx context.Context, projectID, creatorID uuid.UUID) error {
	const q = `
UPDATE project_creators SET removed_at = now()
WHERE project_id = $1 AND creator_user_id = $2 AND removed_at IS NULL`
	tag, err := r.db.Exec(ctx, q, projectID, creatorID)
	if err != nil {
		return fmt.Errorf("remove creator: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCreatorNotInProject
	}
	return nil
}

// ListCreators — действующий состав проекта.
func (r *Repo) ListCreators(ctx context.Context, projectID uuid.UUID) ([]uuid.UUID, error) {
	const q = `
SELECT creator_user_id FROM project_creators
WHERE project_id = $1 AND removed_at IS NULL
ORDER BY added_at`
	rows, err := r.db.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("list creators: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, 4)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan creator: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- выкладки ----

// CreateBatch — массовая простановка дат: креаторы × даты. Всё в одной
// транзакции и с общим created_batch_id, чтобы ошибочную пачку можно было
// снять одним действием (см. CancelBatch).
func (r *Repo) CreateBatch(ctx context.Context, in CreateBatchInput) (BatchResult, error) {
	if len(in.Dates) == 0 {
		return BatchResult{}, ErrNothingToCreate
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BatchResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	plan, err := r.assertProjectHasPublications(ctx, tx, in.ProjectID)
	if err != nil {
		return BatchResult{}, err
	}
	draftRequired := plan.DraftRequired

	// Где креаторов нет, там пачка — это просто даты, и пустой список
	// людей норма. Непустой при этом отказываем, а не игнорируем тихо:
	// менеджер, приславший креаторов в проект без креаторов, ошибся
	// проектом, и молчаливое «поняли по-своему» он заметит только в
	// отчёте.
	if !plan.HasCrew() {
		if len(in.CreatorUserIDs) > 0 {
			return BatchResult{}, ErrCrewNotAllowed
		}
	} else {
		if len(in.CreatorUserIDs) == 0 {
			return BatchResult{}, ErrNothingToCreate
		}
		// Состав проекта читаем один раз: проверять каждого креатора
		// отдельным запросом на пачке из 60 выкладок — 60 лишних
		// round-trip'ов.
		members, err := txCreatorSet(ctx, tx, in.ProjectID)
		if err != nil {
			return BatchResult{}, err
		}
		for _, cid := range in.CreatorUserIDs {
			if !members[cid] {
				return BatchResult{}, fmt.Errorf("%w: %s", ErrCreatorNotInProject, cid)
			}
		}
	}

	batchID := uuid.New()

	// Один INSERT на всю пачку вместо запроса на каждую пару
	// «креатор × дата»: потолок пачки — 500 выкладок, и столько же
	// round-trip'ов держали бы транзакцию открытой заметно дольше
	// нужного. Соседний txCreatorSet сделан тем же приёмом и по той же
	// причине.
	//
	// ON CONFLICT DO NOTHING поверх publications_creator_day_uniq:
	// повторная отправка формы (двойной клик, ретрай) не создаёт вторую
	// пачку на те же даты, а тихо пропускает уже существующие.
	//
	// У проекта без креаторов произведение вырождается в одни даты, и
	// столбец креаторов уходит в базу массивом NULL'ов: выкладка
	// принадлежит проекту, а не человеку.
	owners := in.CreatorUserIDs
	if !plan.HasCrew() {
		owners = []uuid.UUID{uuid.Nil}
	}
	total := len(owners) * len(in.Dates)
	// pgtype.UUID, а не *uuid.UUID: драйвер кодирует срез указателей
	// поэлементно через driver.Valuer, и метод-значение UUID.Value на
	// nil-указателе паникует. Явный Valid=false — единственный способ
	// положить NULL в массив uuid[].
	creatorArg := make([]pgtype.UUID, 0, total)
	dueArg := make([]time.Time, 0, total)
	draftArg := make([]*time.Time, 0, total)
	for i := range owners {
		owner := pgtype.UUID{Bytes: owners[i], Valid: plan.HasCrew()}
		for _, d := range in.Dates {
			day := truncateDay(d)
			var draft *time.Time
			if draftRequired && in.DraftLeadDays > 0 {
				dd := day.AddDate(0, 0, -in.DraftLeadDays)
				draft = &dd
			}
			creatorArg = append(creatorArg, owner)
			dueArg = append(dueArg, day)
			draftArg = append(draftArg, draft)
		}
	}

	const ins = `
INSERT INTO project_publications
    (project_id, creator_user_id, due_date, draft_due_date, created_batch_id, created_by)
SELECT $1, t.c, t.d, t.dr, $5, $6
FROM unnest($2::uuid[], $3::date[], $4::date[]) AS t(c, d, dr)
ON CONFLICT DO NOTHING
RETURNING id, project_id, creator_user_id, due_date, draft_due_date, status,
          created_batch_id, created_at, updated_at`

	rows, err := tx.Query(ctx, ins, in.ProjectID, creatorArg, dueArg, draftArg,
		batchID, in.CreatedBy)
	if err != nil {
		return BatchResult{}, fmt.Errorf("insert publications: %w", err)
	}
	items := make([]Publication, 0, total)
	for rows.Next() {
		var p Publication
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.CreatorUserID, &p.DueDate,
			&p.DraftDueDate, &p.Status, &p.BatchID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rows.Close()
			return BatchResult{}, fmt.Errorf("scan created publication: %w", err)
		}
		// Пустой список ссылок — это [], а не null: поле объявлено
		// массивом, и nil-срез уходит в JSON как null, на котором
		// потребитель, читающий links.length, падает. Ставим руками, а не
		// зовём hydrate: ON CONFLICT DO NOTHING возвращает только что
		// вставленные строки, а у них ни ссылок, ни просьб о переносе, ни
		// статистики нет по определению — три запроса вернули бы пусто.
		p.Links = []SubmittedLink{}
		items = append(items, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return BatchResult{}, fmt.Errorf("iter created publications: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, in.ProjectID.String(),
		outbox.EventPublicationsCreated, map[string]any{
			"project_id": in.ProjectID,
			"batch_id":   batchID,
			"count":      len(items),
			"created_by": in.CreatedBy,
		}); err != nil {
		return BatchResult{}, fmt.Errorf("emit publications_created: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return BatchResult{}, fmt.Errorf("commit: %w", err)
	}
	return BatchResult{BatchID: batchID, Created: len(items), Items: items}, nil
}

// AddSelfPublication — креатор заводит себе выкладку сам.
//
// Отдельный метод, а не флаг у CreateBatch: у пачки менеджера другие
// правила (несколько креаторов, черновики, ON CONFLICT DO NOTHING), и
// смешать их значило бы однажды дать креатору чужого креатора в
// CreatorUserIDs.
//
// Черновик не ставится: срок черновика — это договорённость с
// менеджером о работе, которую он поручил. Ролик, добавленный
// креатором, никто не поручал.
func (r *Repo) AddSelfPublication(ctx context.Context, projectID, creatorID uuid.UUID, day time.Time) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	plan, err := r.assertProjectHasPublications(ctx, tx, projectID)
	if err != nil {
		return Publication{}, err
	}
	// Кабинета креатора у проекта без креаторов нет, но ручка обязана
	// отказывать явно: пустой ответ читался бы как «проект есть, просто
	// пуст».
	if !plan.HasCrew() {
		return Publication{}, ErrCrewNotAllowed
	}
	members, err := txCreatorSet(ctx, tx, projectID)
	if err != nil {
		return Publication{}, err
	}
	if !members[creatorID] {
		// Не «нет доступа», а «нет такого проекта»: подтверждать чужому
		// человеку существование проекта незачем. Хендлер отвечает 404.
		return Publication{}, ErrForbidden
	}

	// Подытоженный период трогать нельзя. Проверка читает таблицу
	// периодов напрямую — да, это чужой домен, но правило про выкладки, и
	// живёт оно там, где выкладка заводится. Сегодняшний день в
	// подытоженный период попасть не может (отсечка — конец периода плюс
	// две недели), и всё же проверяем: период могли закрыть руками
	// раньше, а молча дописать ролик в замороженный подытог — худший из
	// возможных исходов.
	var locked bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_periods
    WHERE project_id = $1 AND status = 'locked' AND $2::date BETWEEN starts_on AND ends_on
)`, projectID, day).Scan(&locked); err != nil {
		return Publication{}, fmt.Errorf("check locked period: %w", err)
	}
	if locked {
		return Publication{}, ErrPeriodLocked
	}

	var p Publication
	err = tx.QueryRow(ctx, `
INSERT INTO project_publications
    (project_id, creator_user_id, due_date, created_by, self_added)
VALUES ($1, $2, $3::date, $2, TRUE)
RETURNING id, project_id, creator_user_id, title, due_date, draft_due_date, status,
          created_batch_id, self_added, created_at, updated_at`,
		projectID, creatorID, day).Scan(
		&p.ID, &p.ProjectID, &p.CreatorUserID, &p.Title, &p.DueDate, &p.DraftDueDate,
		&p.Status, &p.BatchID, &p.SelfAdded, &p.CreatedAt, &p.UpdatedAt)
	if isUniqueViolation(err) {
		// publications_creator_day_uniq: на этот день у него уже
		// что-то стоит. Ограничение не обходим — отвечаем понятно.
		return Publication{}, ErrDayTaken
	}
	if err != nil {
		return Publication{}, fmt.Errorf("insert self publication: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	// Ссылок у только что заведённой выкладки нет по определению, но в
	// JSON должен уйти [], а не null (см. hydrate). Перечитывать строку
	// через Get после COMMIT нельзя: выкладка уже создана, а ошибка
	// чтения превратилась бы в 500 — креатор нажал бы «Добавить» второй
	// раз и упёрся в 409 на дату, которой на экране не видел.
	p.Links = []SubmittedLink{}
	return p, nil
}

// ManagerAddPublication — одна дата одному креатору, рукой менеджера.
//
// Повторяет правила пачки (состав, подытоженный период, уникальность
// дня), но пишет строку без батч-идентификатора: это правка плана, а не
// его простановка, и «отменить пачку» её задеть не должно.
func (r *Repo) ManagerAddPublication(ctx context.Context, in AddPublicationInput) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	plan, err := r.assertProjectHasPublications(ctx, tx, in.ProjectID)
	if err != nil {
		return Publication{}, err
	}
	// Владелец выкладки есть ровно там, где есть состав. У проекта без
	// креаторов вставляем NULL, и переданный креатор — ошибка проектом,
	// а не пожелание, которое можно молча проигнорировать.
	var owner *uuid.UUID
	if plan.HasCrew() {
		members, err := txCreatorSet(ctx, tx, in.ProjectID)
		if err != nil {
			return Publication{}, err
		}
		if !members[in.CreatorUserID] {
			return Publication{}, ErrCreatorNotInProject
		}
		owner = &in.CreatorUserID
	} else if in.CreatorUserID != uuid.Nil {
		return Publication{}, ErrCrewNotAllowed
	}
	if err := assertPeriodOpen(ctx, tx, in.ProjectID, in.Day); err != nil {
		return Publication{}, err
	}

	var draft *time.Time
	if plan.DraftRequired && in.DraftLeadDays > 0 {
		dd := in.Day.AddDate(0, 0, -in.DraftLeadDays)
		draft = &dd
	}

	var p Publication
	err = tx.QueryRow(ctx, `
INSERT INTO project_publications
    (project_id, creator_user_id, due_date, draft_due_date, created_by)
VALUES ($1, $2, $3::date, $4::date, $5)
RETURNING id, project_id, creator_user_id, title, due_date, draft_due_date, status,
          created_batch_id, self_added, created_at, updated_at`,
		in.ProjectID, owner, in.Day, draft, in.ManagerUserID).Scan(
		&p.ID, &p.ProjectID, &p.CreatorUserID, &p.Title, &p.DueDate, &p.DraftDueDate,
		&p.Status, &p.BatchID, &p.SelfAdded, &p.CreatedAt, &p.UpdatedAt)
	if isUniqueViolation(err) {
		return Publication{}, ErrDayTaken
	}
	if err != nil {
		return Publication{}, fmt.Errorf("insert publication: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, in.ProjectID.String(),
		outbox.EventPublicationsCreated, map[string]any{
			"project_id":      in.ProjectID,
			"publication_id":  p.ID,
			"creator_user_id": owner,
			"due_date":        in.Day.Format("2006-01-02"),
			"count":           1,
			"created_by":      in.ManagerUserID,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_created: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	p.Links = []SubmittedLink{}
	return p, nil
}

// MoveDueDate — перенести дату плановой выкладки.
//
// Только по плановой и только пока по ней ничего не сдано: ссылки
// значат, что ролик уже вышел, и дата у него не намерение, а факт.
//
// Открытая просьба о переносе закрывается тем же действием: менеджер на
// неё ответил. Совпала с тем, что он поставил, — «одобрено», поставил
// другое — «отклонено», и в обоих случаях у креатора в кабинете висит
// решение, а не вечное «ждём менеджера».
func (r *Repo) MoveDueDate(ctx context.Context, in MoveDueDateInput) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, err := lockPublicationForEdit(ctx, tx, in.PublicationID)
	if err != nil {
		return Publication{}, err
	}

	var status string
	// Указателем: у ролика проекта без креаторов владельца нет, и скан
	// NULL в uuid.UUID — не компиляторная, а рантайм-ошибка.
	var creatorID *uuid.UUID
	var oldDay time.Time
	var hasLinks bool
	if err := tx.QueryRow(ctx, `
SELECT p.status, p.creator_user_id, p.due_date,
       EXISTS (SELECT 1 FROM publication_links l WHERE l.publication_id = p.id)
FROM project_publications p WHERE p.id = $1`, in.PublicationID).Scan(
		&status, &creatorID, &oldDay, &hasLinks); err != nil {
		return Publication{}, fmt.Errorf("read publication: %w", err)
	}
	if Status(status) != StatusPlanned || hasLinks {
		return Publication{}, ErrPublicationStarted
	}
	// Оба конца переноса: из подытоженного периода не уносят и в
	// подытоженный не приносят — в обоих случаях менялся бы уже
	// замороженный расчёт.
	if err := assertPeriodOpen(ctx, tx, projectID, oldDay); err != nil {
		return Publication{}, err
	}
	if err := assertPeriodOpen(ctx, tx, projectID, in.Day); err != nil {
		return Publication{}, err
	}

	if _, err := tx.Exec(ctx, `
UPDATE project_publications
SET due_date = $2::date,
    draft_due_date = CASE
        WHEN draft_due_date IS NULL THEN NULL
        ELSE $2::date - (due_date - draft_due_date)
    END,
    updated_at = now()
WHERE id = $1`, in.PublicationID, in.Day); err != nil {
		if isUniqueViolation(err) {
			return Publication{}, ErrDayTaken
		}
		return Publication{}, fmt.Errorf("move due date: %w", err)
	}

	// Просьба креатора, если она висела, получает ответ.
	if _, err := tx.Exec(ctx, `
UPDATE publication_date_requests
SET status = CASE WHEN requested_date = $2::date THEN 'approved' ELSE 'rejected' END,
    decided_by = $3, decided_at = now()
WHERE publication_id = $1 AND status = 'pending'`,
		in.PublicationID, in.Day, in.ManagerUserID); err != nil {
		return Publication{}, fmt.Errorf("close date request: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		outbox.EventPublicationMoved, map[string]any{
			"project_id":      projectID,
			"publication_id":  in.PublicationID,
			"creator_user_id": creatorID,
			"from":            oldDay.Format("2006-01-02"),
			"to":              in.Day.Format("2006-01-02"),
			"moved_by":        in.ManagerUserID,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_moved: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, in.PublicationID)
}

// CancelPublication — снять одну плановую выкладку.
//
// Правило то же, что у отмены пачки: сданное не снимается. Причина
// пишется в close_reason — по ней потом видно, почему в плане дыра.
func (r *Repo) CancelPublication(ctx context.Context, pubID, actor uuid.UUID, reason string) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, err := lockPublicationForEdit(ctx, tx, pubID)
	if err != nil {
		return Publication{}, err
	}

	tag, err := tx.Exec(ctx, `
UPDATE project_publications
SET status = 'cancelled', closed_by = $2, close_reason = $3, updated_at = now()
WHERE id = $1 AND status = 'planned'
  AND NOT EXISTS (SELECT 1 FROM publication_links l WHERE l.publication_id = project_publications.id)`,
		pubID, actor, reason)
	if err != nil {
		return Publication{}, fmt.Errorf("cancel publication: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Publication{}, ErrPublicationStarted
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		outbox.EventPublicationCancelled, map[string]any{
			"project_id":     projectID,
			"publication_id": pubID,
			"reason":         reason,
			"cancelled_by":   actor,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_cancelled: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, pubID)
}

// assertPeriodOpen — дата не попадает в подытоженный период.
//
// Одна проверка на три места (завести, перенести, снять): подытоженный
// период — это замороженный расчёт, и любая правка плана внутри него
// меняет суммы, которые уже назвали обеим сторонам.
func assertPeriodOpen(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, day time.Time) error {
	var locked bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM project_periods
    WHERE project_id = $1 AND status = 'locked' AND $2::date BETWEEN starts_on AND ends_on
)`, projectID, day).Scan(&locked); err != nil {
		return fmt.Errorf("check locked period: %w", err)
	}
	if locked {
		return ErrPeriodLocked
	}
	return nil
}

// CancelBatch — снять пачку целиком. Отменяем только те выкладки, по
// которым ещё ничего не сдано: если креатор уже прислал ссылки, стирать
// его работу из-за ошибки менеджера нельзя.
func (r *Repo) CancelBatch(ctx context.Context, projectID, batchID uuid.UUID) (int, error) {
	const q = `
UPDATE project_publications
SET status = 'cancelled', updated_at = now()
WHERE project_id = $1 AND created_batch_id = $2
  AND status = 'planned'
  AND NOT EXISTS (SELECT 1 FROM publication_links l WHERE l.publication_id = project_publications.id)`
	tag, err := r.db.Exec(ctx, q, projectID, batchID)
	if err != nil {
		return 0, fmt.Errorf("cancel batch: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Get — выкладка со ссылками и открытой просьбой о переносе.
func (r *Repo) Get(ctx context.Context, id uuid.UUID) (Publication, error) {
	const q = `
SELECT id, project_id, creator_user_id, title, due_date, draft_due_date, status,
       closed_by, COALESCE(close_reason, ''), created_batch_id, self_added, created_at, updated_at
FROM project_publications WHERE id = $1`
	var p Publication
	err := r.db.QueryRow(ctx, q, id).Scan(
		&p.ID, &p.ProjectID, &p.CreatorUserID, &p.Title, &p.DueDate, &p.DraftDueDate, &p.Status,
		&p.ClosedBy, &p.CloseReason, &p.BatchID, &p.SelfAdded, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, fmt.Errorf("get publication: %w", err)
	}
	if err := r.hydrate(ctx, []*Publication{&p}); err != nil {
		return Publication{}, err
	}
	return p, nil
}

// ListByProject — все выкладки проекта. Это выдача менеджера: он видит всех.
func (r *Repo) ListByProject(ctx context.Context, projectID uuid.UUID) ([]Publication, error) {
	return r.list(ctx, `
SELECT id, project_id, creator_user_id, title, due_date, draft_due_date, status,
       closed_by, COALESCE(close_reason, ''), created_batch_id, self_added, created_at, updated_at
FROM project_publications
WHERE project_id = $1
ORDER BY due_date, created_at`, projectID)
}

// ListByCreator — выкладки одного креатора в проекте.
//
// Это ровно тот срез, который видит креатор: чужие выкладки в выдачу не
// попадают в принципе, а не прячутся на фронте (требование К1).
func (r *Repo) ListByCreator(ctx context.Context, projectID, creatorID uuid.UUID) ([]Publication, error) {
	return r.list(ctx, `
SELECT id, project_id, creator_user_id, title, due_date, draft_due_date, status,
       closed_by, COALESCE(close_reason, ''), created_batch_id, self_added, created_at, updated_at
FROM project_publications
WHERE project_id = $1 AND creator_user_id = $2
ORDER BY due_date, created_at`, projectID, creatorID)
}

func (r *Repo) list(ctx context.Context, q string, args ...any) ([]Publication, error) {
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list publications: %w", err)
	}
	defer rows.Close()
	out := make([]Publication, 0, 16)
	for rows.Next() {
		var p Publication
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.CreatorUserID, &p.Title, &p.DueDate,
			&p.DraftDueDate, &p.Status, &p.ClosedBy, &p.CloseReason, &p.BatchID,
			&p.SelfAdded, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan publication: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refs := make([]*Publication, len(out))
	for i := range out {
		refs[i] = &out[i]
	}
	if err := r.hydrate(ctx, refs); err != nil {
		return nil, err
	}
	return out, nil
}

// hydrate — догружает ссылки и открытые просьбы о переносе одним запросом
// на весь список, а не по запросу на выкладку.
func (r *Repo) hydrate(ctx context.Context, ps []*Publication) error {
	if len(ps) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(ps))
	byID := make(map[uuid.UUID]*Publication, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
		byID[p.ID] = p
	}

	// Пустой список ссылок — это [], а не null. Go отдаёт nil-срез как
	// JSON null, и потребитель, ждущий массив, падает на первой же
	// невыложенной выкладке: у неё ссылок нет по определению.
	for _, p := range ps {
		if p.Links == nil {
			p.Links = []SubmittedLink{}
		}
	}

	linkRows, err := r.db.Query(ctx, `
SELECT id, publication_id, platform, url, url_canonical,
       COALESCE(external_media_id, ''), submitted_at, last_collected_at, published_at
FROM publication_links
WHERE publication_id = ANY($1)
ORDER BY submitted_at`, ids)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var l SubmittedLink
		if err := linkRows.Scan(&l.ID, &l.PublicationID, &l.Platform, &l.URL,
			&l.URLCanonical, &l.ExternalMediaID, &l.SubmittedAt, &l.LastCollectedAt,
			&l.PublishedAt); err != nil {
			return fmt.Errorf("scan link: %w", err)
		}
		if p := byID[l.PublicationID]; p != nil {
			p.Links = append(p.Links, l)
			// «Когда ролик вышел» — самое раннее среди площадок.
			// Считаем здесь, а не в SQL: строки уже в руках, а второй
			// запрос ради минимума был бы запросом ради минимума.
			if l.PublishedAt != nil && (p.PublishedAt == nil || l.PublishedAt.Before(*p.PublishedAt)) {
				p.PublishedAt = l.PublishedAt
			}
		}
	}
	if err := linkRows.Err(); err != nil {
		return err
	}

	reqRows, err := r.db.Query(ctx, `
SELECT id, publication_id, requested_date, reason, status, decided_by, decided_at, created_at
FROM publication_date_requests
WHERE publication_id = ANY($1) AND status = 'pending'`, ids)
	if err != nil {
		return fmt.Errorf("list date requests: %w", err)
	}
	defer reqRows.Close()
	for reqRows.Next() {
		var d DateRequest
		if err := reqRows.Scan(&d.ID, &d.PublicationID, &d.RequestedDate, &d.Reason,
			&d.Status, &d.DecidedBy, &d.DecidedAt, &d.CreatedAt); err != nil {
			return fmt.Errorf("scan date request: %w", err)
		}
		if p := byID[d.PublicationID]; p != nil {
			req := d
			p.PendingDateRequest = &req
		}
	}
	if err := reqRows.Err(); err != nil {
		return err
	}

	// Проверка ролика: решение менеджера и вердикты по пунктам. Едет
	// вместе с выкладкой, потому что спрашивают о ней там же, где
	// смотрят список: «что с моим роликом» — это один экран, а не
	// отдельный заход.
	reviewRows, err := r.db.Query(ctx, `
SELECT v.publication_id, v.status, v.comment, v.round, v.decided_by, v.decided_at,
       COALESCE(u.display_name, '')
FROM publication_reviews v
LEFT JOIN users u ON u.id = v.decided_by
WHERE v.publication_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("list reviews: %w", err)
	}
	defer reviewRows.Close()
	for reviewRows.Next() {
		var pubID uuid.UUID
		var rv PublicationReview
		if err := reviewRows.Scan(&pubID, &rv.Status, &rv.Comment, &rv.Round,
			&rv.DecidedBy, &rv.DecidedAt, &rv.DecidedByName); err != nil {
			return fmt.Errorf("scan review: %w", err)
		}
		rv.Marks = []ReviewMark{}
		if p := byID[pubID]; p != nil {
			v := rv
			p.Review = &v
		}
	}
	if err := reviewRows.Err(); err != nil {
		return err
	}

	markRows, err := r.db.Query(ctx, `
SELECT publication_id, item_id, passed FROM publication_review_marks
WHERE publication_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("list review marks: %w", err)
	}
	defer markRows.Close()
	for markRows.Next() {
		var pubID uuid.UUID
		var m ReviewMark
		if err := markRows.Scan(&pubID, &m.ItemID, &m.Passed); err != nil {
			return fmt.Errorf("scan review mark: %w", err)
		}
		if p := byID[pubID]; p != nil && p.Review != nil {
			p.Review.Marks = append(p.Review.Marks, m)
		}
	}
	if err := markRows.Err(); err != nil {
		return err
	}

	// Цифры по выкладке — сумма последних снимков её площадок. Один
	// запрос на пачку: LATERAL берёт свежую строку статистики по каждой
	// ссылке, снаружи всё складывается по выкладке.
	// Репосты складываем только если они известны по ВСЕМ площадкам
	// выкладки: bool_or по «репостов нет» отвечает, знаем ли мы их
	// целиком. Сумма известных вперемешку с неизвестными — занижение,
	// которое ничем не отличить от настоящего числа.
	statRows, err := r.db.Query(ctx, `
SELECT l.publication_id,
       COALESCE(SUM(cur.views), 0), COALESCE(SUM(cur.likes), 0),
       COALESCE(SUM(cur.comments), 0),
       COALESCE(SUM(cur.shares), 0), bool_or(cur.shares IS NULL),
       MAX(cur.collected_at)
FROM publication_links l
LEFT JOIN LATERAL (
    SELECT views, likes, comments, shares, collected_at
    FROM video_stat_daily d WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC LIMIT 1
) cur ON TRUE
WHERE l.publication_id = ANY($1)
GROUP BY l.publication_id`, ids)
	if err != nil {
		return fmt.Errorf("list publication stats: %w", err)
	}
	defer statRows.Close()
	for statRows.Next() {
		var (
			pubID                  uuid.UUID
			views, likes, comments int64
			shares                 int64
			sharesMissing          *bool
			collectedAt            *time.Time
		)
		if err := statRows.Scan(&pubID, &views, &likes, &comments,
			&shares, &sharesMissing, &collectedAt); err != nil {
			return fmt.Errorf("scan publication stats: %w", err)
		}
		p := byID[pubID]
		if p == nil {
			continue
		}
		p.Views, p.Likes, p.Comments, p.StatsCollectedAt = views, likes, comments, collectedAt
		if sharesMissing == nil || !*sharesMissing {
			v := shares
			p.Shares = &v
		}
		p.ERPercent, p.ERWithoutShares = erPercent(likes, comments, p.Shares, views)
	}
	if err := statRows.Err(); err != nil {
		return err
	}

	// Имена — четвёртым запросом на всю пачку, там же где ссылки и переносы.
	// Без них в интерфейсе остаётся «Креатор 3f2a91b8».
	//
	// У выкладки без креатора имени нет и быть не может — это ролик
	// проекта. Пустая строка здесь правильнее любой подстановки:
	// «Проект» или «Бренд» в колонке «Креатор» читалось бы как имя
	// человека, которого нет.
	creatorIDs := make([]uuid.UUID, 0, len(ps))
	for _, p := range ps {
		if p.CreatorUserID != nil {
			creatorIDs = append(creatorIDs, *p.CreatorUserID)
		}
	}
	names, err := r.resolveNames(ctx, creatorIDs)
	if err != nil {
		return err
	}

	today := truncateDay(time.Now())
	for _, p := range ps {
		if p.CreatorUserID != nil {
			p.CreatorName = names[*p.CreatorUserID]
		}
		p.Overdue = p.Status.IsOpen() &&
			p.PendingDateRequest == nil &&
			truncateDay(p.DueDate).Before(today)
	}
	return nil
}

// SubmitLinks — креатор сдаёт ссылки. Пришли все пять — выкладка закрыта,
// одна-четыре — сдана частично.
//
// parsed приходит уже разобранным (см. ParseLink): репозиторий не занимается
// разбором ссылок, а сервис — SQL.
func (r *Repo) SubmitLinks(ctx context.Context, in SubmitLinksInput, parsed []Link) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID uuid.UUID
	var creatorID *uuid.UUID
	var status Status
	err = tx.QueryRow(ctx, `
SELECT project_id, creator_user_id, status FROM project_publications
WHERE id = $1 FOR UPDATE`, in.PublicationID).Scan(&projectID, &creatorID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, fmt.Errorf("lock publication: %w", err)
	}
	// Креаторская сдача по ролику без креатора обязана отказывать, а не
	// падать: ссылки такому проекту вставляет менеджер (ManagerEditLink).
	if creatorID == nil || *creatorID != in.ActorUserID {
		return Publication{}, ErrForbidden
	}
	if status == StatusCancelled || status == StatusClosedManually {
		return Publication{}, ErrPublicationClosed
	}

	platforms := make([]string, 0, len(parsed))
	for _, l := range parsed {
		platforms = append(platforms, l.Platform)
	}
	if err := assertChecklist(ctx, tx, projectID, platforms, in.CheckedItemIDs); err != nil {
		return Publication{}, err
	}

	upsert := `
INSERT INTO publication_links
    (publication_id, platform, url, url_canonical, external_media_id,
     submitted_at, collect_interval_days, next_collect_at)
VALUES ($1, $2, $3, $4, NULLIF($5, ''), now(), 1, now())
ON CONFLICT (publication_id, platform) DO UPDATE
SET url = EXCLUDED.url,
    url_canonical = EXCLUDED.url_canonical,
    external_media_id = EXCLUDED.external_media_id,
    submitted_at = now(),
    -- Переcданная ссылка — это другой ролик: сбрасываем расписание, чтобы
    -- он попал в ближайший обход, а не ждал своего интервала. Кроме
    -- ссылок, уже снятых с обхода: период такого ролика подытожен, и
    -- новые просмотры по нему записывать некуда (см. ParkedAt).
    collect_interval_days = 1,
    next_collect_at = ` + keepParked("publication_links.next_collect_at", "now()")

	for _, l := range parsed {
		if _, err := tx.Exec(ctx, upsert, in.PublicationID, l.Platform, l.Raw, l.Canonical, l.MediaID); err != nil {
			return Publication{}, fmt.Errorf("upsert link %s: %w", l.Platform, err)
		}
	}

	// Отметки чеклиста — одним запросом: в шаблоне бывает два десятка
	// пунктов, и столько же round-trip'ов внутри транзакции сдачи ничем
	// не оправданы.
	if len(in.CheckedItemIDs) > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO publication_checklist_marks (publication_id, item_id, checked_by)
SELECT $1, item, $3 FROM unnest($2::uuid[]) AS item
ON CONFLICT (publication_id, item_id) DO NOTHING`,
			in.PublicationID, in.CheckedItemIDs, in.ActorUserID); err != nil {
			return Publication{}, fmt.Errorf("mark checklist items: %w", err)
		}
	}

	// Считаем ДВА числа, и это не дублирование.
	//
	// have — сколько ссылок сдано всего, оно едет в событие. required —
	// сколько сдано из обязательной пятёрки, и закрывает выкладку
	// только оно. Сейчас эти числа совпадают: принимаются ровно пять
	// площадок, и UNIQUE (publication_id, platform) не даст сдать одну
	// дважды. Но «пять ссылок» и «все пять площадок» — разные
	// утверждения, и закрывать ролик по первому значит поставить
	// правило на совпадение, которое переживёт ровно до шестой
	// площадки в списке.
	var have, required int
	if err := tx.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE platform = ANY($2))
FROM publication_links WHERE publication_id = $1`,
		in.PublicationID, AllPlatforms).Scan(&have, &required); err != nil {
		return Publication{}, fmt.Errorf("count links: %w", err)
	}
	newStatus := StatusPartial
	if required >= len(AllPlatforms) {
		newStatus = StatusDone
	}
	// COALESCE(NULLIF(...)) — пустое название не затирает записанное:
	// площадки досылают по одной, и во втором заходе поля может не быть.
	if _, err := tx.Exec(ctx, `
UPDATE project_publications
SET status = $2, title = COALESCE(NULLIF($3, ''), title), updated_at = now()
WHERE id = $1`, in.PublicationID, newStatus, in.Title); err != nil {
		return Publication{}, fmt.Errorf("update status: %w", err)
	}

	// Ролик пересдали — прежнее решение по нему больше не про него.
	if err := reopenReview(ctx, tx, in.PublicationID); err != nil {
		return Publication{}, err
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		outbox.EventPublicationSubmitted, map[string]any{
			"project_id":     projectID,
			"publication_id": in.PublicationID,
			"creator_id":     in.ActorUserID,
			"platforms":      platforms,
			"status":         string(newStatus),
			"links_total":    have,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_submitted: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, in.PublicationID)
}

// CloseManually — менеджер закрывает неполную выкладку. Причина обязательна:
// это исключение из правила «закрыто на пяти ссылках», и у исключения должен
// быть автор и объяснение.

// ---- правка ссылки менеджером ----

// ManagerEditLink — заменить ссылку площадки на новую.
//
// Старые замеры удаляются вместе со ссылкой, если это ДРУГОЙ ролик: ряд
// video_stat_daily привязан к link_id, и оставить его — значит склеить
// историю двух разных видео в одну линию. Та же ссылка, поправленная
// косметически (мобильный домен, рекламный хвост), даёт тот же
// url_canonical — тогда замеры остаются на месте.
func (r *Repo) ManagerEditLink(ctx context.Context, in ManagerEditLinkInput, l Link) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, err := lockPublicationForEdit(ctx, tx, in.PublicationID)
	if err != nil {
		return Publication{}, err
	}

	// Подытоженный период правку ссылки не принимает — ни от креатора,
	// ни от менеджера. Срез снят, счёт выставлен, деньги посчитаны;
	// подмена ролика задним числом поменяла бы то, по чему уже
	// рассчитались, и никто бы этого не увидел. Ролик, исчезнувший с
	// площадки в закрытом периоде, — разговор с клиентом, а не правка
	// строки.
	if err := assertPublicationPeriodOpen(ctx, tx, in.PublicationID); err != nil {
		return Publication{}, err
	}

	// Тот же ролик или другой — решает канонический адрес: он и есть то,
	// по чему ходит сборщик.
	var prevCanonical string
	err = tx.QueryRow(ctx, `
SELECT url_canonical FROM publication_links
WHERE publication_id = $1 AND platform = $2`, in.PublicationID, in.Platform).Scan(&prevCanonical)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, fmt.Errorf("read link: %w", err)
	}
	statsReset := prevCanonical != "" && prevCanonical != l.Canonical

	upsert := `
INSERT INTO publication_links
    (publication_id, platform, url, url_canonical, external_media_id,
     submitted_at, collect_interval_days, next_collect_at)
VALUES ($1, $2, $3, $4, NULLIF($5, ''), now(), 1, now())
ON CONFLICT (publication_id, platform) DO UPDATE
SET url = EXCLUDED.url,
    url_canonical = EXCLUDED.url_canonical,
    external_media_id = EXCLUDED.external_media_id,
    submitted_at = now(),
    -- Снятую с обхода ссылку правка адреса в очередь не возвращает:
    -- её период подытожен, числа заморожены срезом (см. ParkedAt).
    collect_interval_days = 1,
    next_collect_at = ` + keepParked("publication_links.next_collect_at", "now()") + `
RETURNING id`
	// Замеры сносим ДО правки самой ссылки, хотя логически это следствие.
	//
	// Порядок блокировок обязан совпадать со сборщиком: SaveStats берёт
	// сначала video_stat_daily (upsert замера), потом publication_links
	// (перенос расписания). Обратный порядок здесь давал классическую
	// взаимную блокировку — Postgres ловил 40P01 и убивал одну из
	// транзакций: либо 500 у менеджера, либо оборванная пачка сбора.
	// Окно шире, чем кажется: тик сбора идёт до двадцати пачек подряд.
	if statsReset {
		if _, err := tx.Exec(ctx, `
DELETE FROM video_stat_daily WHERE link_id IN (
    SELECT id FROM publication_links WHERE publication_id = $1 AND platform = $2
)`, in.PublicationID, in.Platform); err != nil {
			return Publication{}, fmt.Errorf("reset stats: %w", err)
		}
	}

	var linkID uuid.UUID
	if err := tx.QueryRow(ctx, upsert,
		in.PublicationID, in.Platform, l.Raw, l.Canonical, l.MediaID).Scan(&linkID); err != nil {
		return Publication{}, fmt.Errorf("upsert link %s: %w", in.Platform, err)
	}

	if statsReset {
		// Ссылка ведёт на другой ролик — значит, проверяли не его.
		if err := reopenReview(ctx, tx, in.PublicationID); err != nil {
			return Publication{}, err
		}
	}

	if err := refreshPublicationStatus(ctx, tx, in.PublicationID); err != nil {
		return Publication{}, err
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		outbox.EventPublicationLinkEdited, map[string]any{
			"project_id":     projectID,
			"publication_id": in.PublicationID,
			"platform":       in.Platform,
			"url":            l.Raw,
			"edited_by":      in.ManagerUserID,
			"stats_reset":    statsReset,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_link_edited: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, in.PublicationID)
}

// ManagerRemoveLink — снять ссылку с площадки.
//
// Выкладка возвращается в «неполную»: закрытой по ошибочной ссылке она
// оставаться не может, иначе счёт заказчику включает ролик, которого нет.
func (r *Repo) ManagerRemoveLink(ctx context.Context, in ManagerEditLinkInput) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, err := lockPublicationForEdit(ctx, tx, in.PublicationID)
	if err != nil {
		return Publication{}, err
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM publication_links WHERE publication_id = $1 AND platform = $2`,
		in.PublicationID, in.Platform)
	if err != nil {
		return Publication{}, fmt.Errorf("delete link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Publication{}, ErrNotFound
	}

	if err := refreshPublicationStatus(ctx, tx, in.PublicationID); err != nil {
		return Publication{}, err
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		outbox.EventPublicationLinkEdited, map[string]any{
			"project_id":     projectID,
			"publication_id": in.PublicationID,
			"platform":       in.Platform,
			"url":            "",
			"edited_by":      in.ManagerUserID,
			"stats_reset":    true,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_link_edited: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, in.PublicationID)
}

// lockPublicationForEdit — общий пролог правки: блокирует выкладку и не
// даёт трогать отменённую и закрытую руками.
// assertPublicationPeriodOpen — период, которому принадлежит срок этой
// выкладки, ещё не подытожен.
func assertPublicationPeriodOpen(ctx context.Context, tx pgx.Tx, pubID uuid.UUID) error {
	var projectID uuid.UUID
	var day time.Time
	if err := tx.QueryRow(ctx,
		`SELECT project_id, due_date FROM project_publications WHERE id = $1`,
		pubID).Scan(&projectID, &day); err != nil {
		return fmt.Errorf("read publication day: %w", err)
	}
	return assertPeriodOpen(ctx, tx, projectID, day)
}

func lockPublicationForEdit(ctx context.Context, tx pgx.Tx, pubID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	var status Status
	err := tx.QueryRow(ctx, `
SELECT project_id, status FROM project_publications WHERE id = $1 FOR UPDATE`,
		pubID).Scan(&projectID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock publication: %w", err)
	}
	if status == StatusCancelled || status == StatusClosedManually {
		return uuid.Nil, ErrPublicationClosed
	}
	return projectID, nil
}

// refreshPublicationStatus — пересчитать статус по числу сданных ссылок.
// Пять из пяти — done, меньше — partial, ни одной — planned: выкладка,
// с которой сняли последнюю ссылку, снова ждёт сдачи.
func refreshPublicationStatus(ctx context.Context, tx pgx.Tx, pubID uuid.UUID) error {
	var have int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM publication_links WHERE publication_id = $1`, pubID).Scan(&have); err != nil {
		return fmt.Errorf("count links: %w", err)
	}
	status := StatusPlanned
	switch {
	case have >= len(AllPlatforms):
		status = StatusDone
	case have > 0:
		status = StatusPartial
	}
	if _, err := tx.Exec(ctx, `
UPDATE project_publications SET status = $2, updated_at = now() WHERE id = $1`,
		pubID, status); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

func (r *Repo) CloseManually(ctx context.Context, in CloseManuallyInput) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID uuid.UUID
	var status Status
	err = tx.QueryRow(ctx, `
SELECT project_id, status FROM project_publications WHERE id = $1 FOR UPDATE`,
		in.PublicationID).Scan(&projectID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, fmt.Errorf("lock publication: %w", err)
	}
	if status == StatusCancelled || status == StatusClosedManually {
		return Publication{}, ErrPublicationClosed
	}

	if _, err := tx.Exec(ctx, `
UPDATE project_publications
SET status = 'closed_manually', closed_by = $2, close_reason = $3, updated_at = now()
WHERE id = $1`, in.PublicationID, in.ManagerUserID, in.Reason); err != nil {
		return Publication{}, fmt.Errorf("close publication: %w", err)
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(),
		outbox.EventPublicationClosed, map[string]any{
			"project_id":     projectID,
			"publication_id": in.PublicationID,
			"closed_by":      in.ManagerUserID,
			"reason":         in.Reason,
		}); err != nil {
		return Publication{}, fmt.Errorf("emit publication_closed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, in.PublicationID)
}

// ---- перенос даты ----

// RequestDateChange — креатор просит перенос. Пока просьба не рассмотрена,
// выкладка не считается просроченной (см. hydrate).
func (r *Repo) RequestDateChange(ctx context.Context, pubID, actorID uuid.UUID, newDate time.Time, reason string) (DateRequest, error) {
	var creatorID *uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT creator_user_id FROM project_publications WHERE id = $1`, pubID).Scan(&creatorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DateRequest{}, ErrNotFound
	}
	if err != nil {
		return DateRequest{}, fmt.Errorf("get publication: %w", err)
	}
	// Просить перенос может только тот, кому выкладка поручена. У ролика
	// проекта без креаторов такого человека нет вовсе.
	if creatorID == nil || *creatorID != actorID {
		return DateRequest{}, ErrForbidden
	}

	var d DateRequest
	err = r.db.QueryRow(ctx, `
INSERT INTO publication_date_requests (publication_id, requested_date, reason)
VALUES ($1, $2, $3)
RETURNING id, publication_id, requested_date, reason, status, decided_by, decided_at, created_at`,
		pubID, truncateDay(newDate), reason).Scan(
		&d.ID, &d.PublicationID, &d.RequestedDate, &d.Reason, &d.Status,
		&d.DecidedBy, &d.DecidedAt, &d.CreatedAt)
	if err != nil {
		// Уникальный частичный индекс не даёт завести вторую открытую
		// просьбу по той же выкладке.
		if isUniqueViolation(err) {
			return DateRequest{}, ErrAlreadyRequested
		}
		return DateRequest{}, fmt.Errorf("insert date request: %w", err)
	}
	return d, nil
}

// DecideDateRequest — менеджер принимает или отклоняет перенос. При принятии
// дата выкладки меняется в той же транзакции: иначе просьба «одобрена», а
// дедлайн старый.
func (r *Repo) DecideDateRequest(ctx context.Context, requestID, managerID uuid.UUID, approve bool) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var pubID uuid.UUID
	var requested time.Time
	var status string
	err = tx.QueryRow(ctx, `
SELECT publication_id, requested_date, status FROM publication_date_requests
WHERE id = $1 FOR UPDATE`, requestID).Scan(&pubID, &requested, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock date request: %w", err)
	}
	if status != DateRequestPending {
		return ErrNotFound
	}

	newStatus := DateRequestRejected
	if approve {
		newStatus = DateRequestApproved
	}
	if _, err := tx.Exec(ctx, `
UPDATE publication_date_requests
SET status = $2, decided_by = $3, decided_at = now() WHERE id = $1`,
		requestID, newStatus, managerID); err != nil {
		return fmt.Errorf("update date request: %w", err)
	}
	if approve {
		if _, err := tx.Exec(ctx, `
UPDATE project_publications SET due_date = $2, updated_at = now() WHERE id = $1`,
			pubID, requested); err != nil {
			return fmt.Errorf("move due date: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// ---- чеклист ----

// SnapshotChecklist — копирует шаблон из библиотеки в проект.
//
// Приём повторяет снимок воронки (internal/projects/repo.go): правка
// библиотеки не должна менять идущие проекты. Повторный вызов заменяет
// снимок целиком — иначе после смены шаблона в проекте окажется смесь
// старых и новых пунктов.
func (r *Repo) SnapshotChecklist(ctx context.Context, projectID, templateID, actor uuid.UUID) (int, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Сносим ТОЛЬКО библиотечный снимок. Пункты, заведённые менеджером
	// под этот проект, не часть шаблона и обновление версии переживают:
	// иначе он обновляет v3 до v4 и молча теряет свои уточнения.
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_checklist_items WHERE project_id = $1 AND NOT added_for_project`,
		projectID); err != nil {
		return 0, fmt.Errorf("clear checklist snapshot: %w", err)
	}

	tag, err := tx.Exec(ctx, `
INSERT INTO project_checklist_items
    (project_id, source_item_id, text, platform, is_required, sort_order)
SELECT $1, i.id, i.text, i.platform, i.is_required, i.sort_order
FROM checklist_template_items i
WHERE i.template_id = $2
ORDER BY i.sort_order`, projectID, templateID)
	if err != nil {
		return 0, fmt.Errorf("copy checklist items: %w", err)
	}
	// Имя и версия — снимком, а не ссылкой: версия шаблона живёт колонкой
	// в его же строке и переписывается при правке библиотеки. Без копии
	// «подключён v3, вышла v4» показать нечем.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_checklist_snapshot
    (project_id, template_id, template_name, template_version, connected_by, connected_at)
-- Подключить чек-лист может не человек, а создание проекта: тогда
-- «кто подключил» — NULL, а не нулевой UUID. Нулевой не проходит
-- внешним ключом, и вся вставка откатывалась молча: проект заводился
-- без чек-листа, и выглядело это как «автоподключение не работает».
SELECT $1, t.id, t.name, t.version, NULLIF($3, '00000000-0000-0000-0000-000000000000'::uuid), now()
FROM checklist_templates t WHERE t.id = $2
ON CONFLICT (project_id) DO UPDATE SET
  template_id = EXCLUDED.template_id,
  template_name = EXCLUDED.template_name,
  template_version = EXCLUDED.template_version,
  connected_by = EXCLUDED.connected_by,
  connected_at = now()`, projectID, templateID, actor); err != nil {
		return 0, fmt.Errorf("save checklist snapshot meta: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ChecklistSnapshotMeta — какой шаблон и какой его версии подключён.
// Пусто, если чеклист в проект не подключали.
type ChecklistSnapshotMeta struct {
	TemplateID      *uuid.UUID `json:"template_id,omitempty"`
	TemplateName    string     `json:"template_name"`
	TemplateVersion int        `json:"template_version"`
	ConnectedAt     time.Time  `json:"connected_at"`
	// LatestVersion — какая версия этого шаблона в библиотеке сейчас.
	// Больше подключённой — значит вышло обновление.
	LatestVersion int `json:"latest_version,omitempty"`
}

// ChecklistMeta — шапка блока чеклиста у менеджера.
func (r *Repo) ChecklistMeta(ctx context.Context, projectID uuid.UUID) (*ChecklistSnapshotMeta, error) {
	var m ChecklistSnapshotMeta
	err := r.db.QueryRow(ctx, `
SELECT s.template_id, s.template_name, s.template_version, s.connected_at,
       COALESCE(t.version, 0)
FROM project_checklist_snapshot s
LEFT JOIN checklist_templates t ON t.id = s.template_id
WHERE s.project_id = $1`, projectID).
		Scan(&m.TemplateID, &m.TemplateName, &m.TemplateVersion, &m.ConnectedAt, &m.LatestVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load checklist meta: %w", err)
	}
	return &m, nil
}

// ProjectChecklist — снимок чеклиста проекта.
func (r *Repo) ProjectChecklist(ctx context.Context, projectID uuid.UUID) ([]ChecklistItem, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, project_id, text, platform, is_required, sort_order, added_for_project
FROM project_checklist_items WHERE project_id = $1 ORDER BY sort_order`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list checklist: %w", err)
	}
	defer rows.Close()
	out := make([]ChecklistItem, 0, 8)
	for rows.Next() {
		var c ChecklistItem
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Text, &c.Platform, &c.IsRequired,
			&c.SortOrder, &c.AddedForProject); err != nil {
			return nil, fmt.Errorf("scan checklist item: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- helpers ----

// assertChecklist — обязательные пункты, относящиеся к сдаваемым площадкам,
// должны быть отмечены. Проверка здесь, а не на фронте: иначе её обходят
// прямым запросом к API.
//
// Это же правило действует и при проверке ролика менеджером (review.go):
// список «что спрашивать» там ровно тот же, поэтому обе стороны считают
// его одной функцией — requiredChecklistFor. Две копии правила разошлись
// бы на первом же пункте с площадкой, и тогда креатору и менеджеру
// показывали бы разные требования к одному ролику.
func assertChecklist(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, platforms []string, checked []uuid.UUID) error {
	items, err := checklistItemsTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(platforms))
	for _, p := range platforms {
		set[p] = true
	}
	isChecked := make(map[uuid.UUID]bool, len(checked))
	for _, id := range checked {
		isChecked[id] = true
	}
	if missing := missingChecklistTexts(requiredChecklistFor(items, set), isChecked); len(missing) > 0 {
		return fmt.Errorf("%w: %v", ErrChecklistIncomplete, missing)
	}
	return nil
}

// checklistItemsTx — снимок чек-листа проекта внутри транзакции. Тот же
// запрос, что в ProjectChecklist, но по tx: обе стороны чек-листа
// читают его под блокировкой выкладки.
func checklistItemsTx(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) ([]ChecklistItem, error) {
	rows, err := tx.Query(ctx, `
SELECT id, project_id, text, platform, is_required, sort_order, added_for_project
FROM project_checklist_items WHERE project_id = $1 ORDER BY sort_order`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list checklist: %w", err)
	}
	defer rows.Close()
	out := make([]ChecklistItem, 0, 8)
	for rows.Next() {
		var c ChecklistItem
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Text, &c.Platform, &c.IsRequired,
			&c.SortOrder, &c.AddedForProject); err != nil {
			return nil, fmt.Errorf("scan checklist item: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// requiredChecklistFor — какие пункты спрашивают у ролика, вышедшего на
// этих площадках. Пункт площадки, которой в ролике нет, не требуют: у
// креатора это право дослать остальные позже, у менеджера — требование,
// которое невозможно выполнить.
func requiredChecklistFor(items []ChecklistItem, platforms map[string]bool) []ChecklistItem {
	out := make([]ChecklistItem, 0, len(items))
	for _, it := range items {
		if !it.IsRequired {
			continue
		}
		if it.Platform != nil && !platforms[*it.Platform] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// missingChecklistTexts — незакрытые пункты словами. Пункт без отметки
// и пункт с «нет» здесь одинаковы: оба не пройдены.
func missingChecklistTexts(required []ChecklistItem, ok map[uuid.UUID]bool) []string {
	missing := make([]string, 0, 2)
	for _, it := range required {
		if !ok[it.ID] {
			missing = append(missing, it.Text)
		}
	}
	return missing
}

// projectPlan — вид проекта и его настройка черновиков.
//
// Возвращается вместе, чтобы вызывающий не ходил в ту же строку второй
// раз: вид ему нужен, чтобы решить про креаторов, а draft_required —
// чтобы поставить срок черновика.
type projectPlan struct {
	Kind          projects.ProjectKind
	DraftRequired bool
}

// HasCrew — выкладку этого проекта поручают человеку.
func (p projectPlan) HasCrew() bool { return projects.FeaturesOf(p.Kind).HasCrew }

// assertProjectHasPublications — план выкладок бывает не у всякого вида.
//
// Раньше здесь стоял литерал `kind != "creators_turnkey"`, и он же был
// единственным вратарём выкладок. С появлением второго вида с планом
// (бренд под ключ) сравнение со строкой пришлось бы держать в голове —
// спрашиваем матрицу.
func (r *Repo) assertProjectHasPublications(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (projectPlan, error) {
	var out projectPlan
	var kind string
	err := tx.QueryRow(ctx,
		`SELECT kind::text, draft_required FROM projects WHERE id = $1`, projectID).Scan(&kind, &out.DraftRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectPlan{}, ErrNotFound
	}
	if err != nil {
		return projectPlan{}, fmt.Errorf("get project kind: %w", err)
	}
	out.Kind = projects.ProjectKind(kind)
	if !projects.FeaturesOf(out.Kind).HasPublications {
		return projectPlan{}, fmt.Errorf("%w: kind=%s", ErrNotCreatorsProject, kind)
	}
	return out, nil
}

func txCreatorSet(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := tx.Query(ctx, `
SELECT creator_user_id FROM project_creators
WHERE project_id = $1 AND removed_at IS NULL`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project creators: %w", err)
	}
	defer rows.Close()
	set := make(map[uuid.UUID]bool, 4)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project creator: %w", err)
		}
		set[id] = true
	}
	return set, rows.Err()
}

// truncateDay — дата без времени. due_date в БД типа DATE, и сравнивать
// её с time.Now() без обрезки значит считать сегодняшнюю выкладку
// просроченной с полуночи.
func truncateDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// ManagerHasAccess — проект доступен этому менеджеру.
//
// Повторяет семантику projects.GetByIDForManager, которой держится
// вся остальная CRM:
//   - managerID == uuid.Nil — это админ, ему доступно всё;
//   - назначенный на себя проект — доступен;
//   - неназначенный — виден всем менеджерам (чтобы было что взять);
//   - назначенный другому — ErrNotFound, а не «запрещено»: сам факт
//     существования чужого проекта тоже не наше дело.
//
// Без этой проверки роли мало: /manager/* пускает любого менеджера, и,
// зная чужой project_id, он получил бы и отчёт, и право менять состав.
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
	if managerID == uuid.Nil {
		return nil
	}
	if assigned == nil || *assigned == managerID {
		return nil
	}
	return ErrNotFound
}

// ProjectOfPublication — к какому проекту относится выкладка.
// Нужен там, где в пути есть только publication_id: проверять доступ
// всё равно надо, а проверять его не к чему.
func (r *Repo) ProjectOfPublication(ctx context.Context, pubID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT project_id FROM project_publications WHERE id = $1`, pubID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("project of publication: %w", err)
	}
	return projectID, nil
}

// ProjectOfDateRequest — то же для просьбы о переносе даты.
func (r *Repo) ProjectOfDateRequest(ctx context.Context, reqID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	err := r.db.QueryRow(ctx, `
SELECT p.project_id FROM publication_date_requests d
JOIN project_publications p ON p.id = d.publication_id
WHERE d.id = $1`, reqID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("project of date request: %w", err)
	}
	return projectID, nil
}

// assertCanBeCreator — этот пользователь может быть креатором проекта.
//
// Креатор снимает и выкладывает ролики, поэтому он специалист (kind
// specialist или both) и точно не менеджер, не админ и не отключённый
// аккаунт. Тот же приём, что у projects.AssertUserCanBeClient, который
// не даёт привязать проект к коллеге по CRM.
func (r *Repo) assertCanBeCreator(ctx context.Context, userID uuid.UUID) error {
	var kind string
	var isManager, isAdmin, isActive bool
	err := r.db.QueryRow(ctx,
		`SELECT kind, is_manager, is_admin, is_active FROM users WHERE id = $1`,
		userID).Scan(&kind, &isManager, &isAdmin, &isActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check creator eligibility: %w", err)
	}
	if isManager || isAdmin || !isActive {
		return ErrNotACreator
	}
	if kind != "specialist" && kind != "both" {
		return ErrNotACreator
	}
	return nil
}
