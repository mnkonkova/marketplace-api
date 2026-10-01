package publications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Виды напоминаний. Значение попадает в notification_log.kind и в
// event_type для n8n, поэтому меняется вместе с настройками воркфлоу.
const (
	// ReminderDueTomorrow — накануне, за сутки до срока. Уходит креатору
	// лично и только если менеджер включил колокольчик напротив него:
	// «завтра срок» — ещё рабочее напоминание, в отличие от «сегодня
	// срок», когда снимать и монтировать уже поздно.
	ReminderDueTomorrow = "publication_due_tomorrow"
	// ReminderDueToday — утро дня дедлайна. Уходит креатору лично.
	ReminderDueToday = "publication_due_today"
	// ReminderOverdue — следующий день после просрочки и дальше раз в
	// сутки, пока выкладка не закрыта.
	ReminderOverdue = "publication_overdue"
	// ReminderIncomplete — ролик вышел, но собраны не все площадки.
	// Отдельный текст: человеку, который уже поработал, «вы просрочили»
	// писать неправильно.
	ReminderIncomplete = "publication_incomplete"
	// ReminderManual — менеджер напомнил руками, не дожидаясь расписания.
	ReminderManual = "publication_manual"
	// ReminderNoViews — ролик вышел, а просмотров нет.
	//
	// Единственный вид, который говорит не про сроки, а про результат.
	// Ноль на третий день почти никогда не означает «никто не
	// посмотрел»: чаще это не тот адрес, теневой бан, удалённое видео
	// или аккаунт, с которого площадка не раздаёт. Всё это чинится,
	// пока свежо, и молчать об этом — значит узнать в конце периода.
	ReminderNoViews = "publication_no_views"
	// ReminderManagerDigest — сводка в общий чат менеджеров. Получателя-
	// человека нет, поэтому в журнале user_id = NULL.
	ReminderManagerDigest = "manager_digest"
)

// Reminder — одно напоминание, готовое к отправке.
type Reminder struct {
	Kind string `json:"kind"`
	// Views — сколько просмотров набрал ролик. Заполняется только у
	// ReminderNoViews: остальные виды про сроки, и цифра там ни при чём.
	Views         int64     `json:"views,omitempty"`
	ProjectID     uuid.UUID `json:"project_id"`
	PublicationID uuid.UUID `json:"publication_id"`
	// CreatorUserID — кому поручена выкладка. nil у проекта без
	// креаторов: ролик принадлежит проекту, и адресата у поштучного
	// письма нет вовсе. Указатель, а не нулевой uuid: нулевой uuid
	// уехал бы в notification_log получателем и слепил бы там в одну
	// строку все проекты разом.
	CreatorUserID    *uuid.UUID `json:"creator_user_id,omitempty"`
	DueDate          time.Time  `json:"due_date"`
	DaysOverdue      int        `json:"days_overdue"`
	MissingPlatforms []string   `json:"missing_platforms,omitempty"`
	ProjectTitle     string     `json:"project_title"`

	// autoping — выключатели проекта. Не экспортируется и не уезжает в
	// payload: это настройка отправителя, получателю она ни к чему.
	autoping ReminderPrefs
}

// ProjectDigest — строка сводки для общего чата менеджеров: что горит и
// по кому.
type ProjectDigest struct {
	ProjectID    uuid.UUID   `json:"project_id"`
	ProjectTitle string      `json:"project_title"`
	ManagerID    *uuid.UUID  `json:"manager_id,omitempty"`
	DueToday     int         `json:"due_today"`
	Overdue      int         `json:"overdue"`
	Incomplete   int         `json:"incomplete"`
	Creators     []uuid.UUID `json:"creators"`
}

// RunStats — что сделал один проход планировщика.
type RunStats struct {
	Considered int `json:"considered"`
	Sent       int `json:"sent"`
	// Skipped — напоминание уже уходило сегодня. Это норма, а не ошибка:
	// именно так выглядит защита от повторной отправки при перезапуске.
	Skipped int `json:"skipped"`
	Digests int `json:"digests"`
	// PlanEndings — скольким менеджерам сказали, что расписание
	// кончается. Считается отдельно от сводки: это не «что горит
	// сегодня», а предупреждение на две недели вперёд.
	PlanEndings int `json:"plan_endings"`
	Failures    int `json:"failures"`
}

// DueReminders — что нужно отправить на дату today.
//
// Просроченным считается только то, по чему НЕ висит непринятая просьба
// о переносе: писать человеку, который уже предупредил, — верный способ
// научить его не предупреждать.
func (r *Repo) DueReminders(ctx context.Context, today time.Time) ([]Reminder, error) {
	day := truncateDay(today)
	const q = `
SELECT p.id, p.project_id, p.creator_user_id, p.due_date, p.status::text,
       COALESCE(pr.title, ''),
       COALESCE(array_agg(l.platform) FILTER (WHERE l.platform IS NOT NULL), '{}'),
       -- Выключатели автопинга. Нет строки — всё включено (см. 00036).
       COALESCE(rp.due_today, TRUE), COALESCE(rp.overdue, TRUE),
       COALESCE(rp.incomplete, TRUE), COALESCE(rp.manager_digest, TRUE),
       -- «Накануне» — исключение из правила «нет строки, значит
       -- включено»: вид напоминания новый, и включают его поимённо.
       -- Колокольчик напротив креатора сильнее настройки проекта.
       COALESCE(cp.day_before, rp.day_before, FALSE)
FROM project_publications p
JOIN projects pr ON pr.id = p.project_id
LEFT JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN project_reminder_prefs rp ON rp.project_id = p.project_id
LEFT JOIN project_creator_reminder_prefs cp
       ON cp.project_id = p.project_id AND cp.creator_user_id = p.creator_user_id
WHERE p.status IN ('planned', 'partial')
  AND p.due_date <= $1::date + 1
  AND p.due_date >= $1 - $2::int
  -- Оба вида с выкладками: у проекта без креаторов просрочки такие же
  -- настоящие, просто писать о них некому лично — они складываются в
  -- дневную сводку в общий чат менеджеров (см. RunReminders).
  AND pr.kind IN ('creators_turnkey', 'brand_turnkey')
  AND NOT EXISTS (
      SELECT 1 FROM publication_date_requests dr
      WHERE dr.publication_id = p.id AND dr.status = 'pending'
  )
GROUP BY p.id, p.project_id, p.creator_user_id, p.due_date, p.status, pr.title,
         rp.due_today, rp.overdue, rp.incomplete, rp.manager_digest,
         cp.day_before, rp.day_before
ORDER BY p.due_date`
	rows, err := r.db.Query(ctx, q, day, OverdueHorizonDays)
	if err != nil {
		return nil, fmt.Errorf("list due reminders: %w", err)
	}
	defer rows.Close()

	out := make([]Reminder, 0, 16)
	for rows.Next() {
		var (
			rem      Reminder
			status   string
			platform []string
		)
		if err := rows.Scan(&rem.PublicationID, &rem.ProjectID, &rem.CreatorUserID,
			&rem.DueDate, &status, &rem.ProjectTitle, &platform,
			&rem.autoping.DueToday, &rem.autoping.Overdue,
			&rem.autoping.Incomplete, &rem.autoping.ManagerDigest,
			&rem.autoping.DayBefore); err != nil {
			return nil, fmt.Errorf("scan due reminder: %w", err)
		}
		rem.autoping.ProjectID = rem.ProjectID
		rem.DaysOverdue = int(day.Sub(truncateDay(rem.DueDate)).Hours() / 24)
		rem.MissingPlatforms = missingFrom(platform)
		rem.Kind = classify(status, rem.DaysOverdue, len(platform))
		out = append(out, rem)
	}
	return out, rows.Err()
}

// classify — какой именно текст уходит человеку.
//
// daysOverdue отрицателен, когда срок ещё впереди: −1 — это «завтра».
func classify(status string, daysOverdue, haveLinks int) string {
	switch {
	// Ролик вышел, но площадок меньше пяти — отдельная ветка, даже если
	// срок уже прошёл: человек работу сделал, просто не дослал ссылки.
	case status == string(StatusPartial) && haveLinks > 0:
		return ReminderIncomplete
	case daysOverdue > 0:
		return ReminderOverdue
	case daysOverdue < 0:
		return ReminderDueTomorrow
	default:
		return ReminderDueToday
	}
}

func missingFrom(have []string) []string {
	set := make(map[string]bool, len(have))
	for _, p := range have {
		set[p] = true
	}
	missing := make([]string, 0, len(AllPlatforms))
	for _, p := range AllPlatforms {
		if !set[p] {
			missing = append(missing, p)
		}
	}
	return missing
}

// Send — записывает напоминание в журнал и кладёт событие в outbox
// в одной транзакции.
//
// Возвращает false, если такое напоминание сегодня уже уходило. Дедуп
// держится уникальным индексом в БД, а не памятью процесса: перезапуск
// воркера память обнуляет, а индекс — нет. Событие пишется ТОЛЬКО когда
// строка журнала действительно вставилась, иначе повторный проход
// насыпал бы в очередь дубли при пустом журнале.
func (r *Repo) Send(ctx context.Context, rem Reminder, recipient *uuid.UUID, sentDate time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	subject := rem.PublicationID
	if rem.Kind == ReminderManagerDigest {
		subject = rem.ProjectID
	}

	tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING`, recipient, rem.Kind, subject, truncateDay(sentDate))
	if err != nil {
		return false, fmt.Errorf("log notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, rem.ProjectID.String(),
		"project."+rem.Kind, rem); err != nil {
		return false, fmt.Errorf("emit reminder: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// SendDigest — сводка в общий чат менеджеров. Получателя-человека нет,
// поэтому в журнале user_id = NULL, а дедуп идёт по проекту и дате.
func (r *Repo) SendDigest(ctx context.Context, d ProjectDigest, sentDate time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES (NULL, $1, $2, $3)
ON CONFLICT DO NOTHING`, ReminderManagerDigest, d.ProjectID, truncateDay(sentDate))
	if err != nil {
		return false, fmt.Errorf("log digest: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, d.ProjectID.String(),
		"project."+ReminderManagerDigest, d); err != nil {
		return false, fmt.Errorf("emit digest: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// GetPublicationForReminder — данные для ручного напоминания менеджера.
func (r *Repo) GetPublicationForReminder(ctx context.Context, pubID uuid.UUID) (Reminder, error) {
	const q = `
SELECT p.id, p.project_id, p.creator_user_id, p.due_date, COALESCE(pr.title, ''),
       COALESCE(array_agg(l.platform) FILTER (WHERE l.platform IS NOT NULL), '{}')
FROM project_publications p
JOIN projects pr ON pr.id = p.project_id
LEFT JOIN publication_links l ON l.publication_id = p.id
WHERE p.id = $1
GROUP BY p.id, p.project_id, p.creator_user_id, p.due_date, pr.title`
	var rem Reminder
	var platform []string
	err := r.db.QueryRow(ctx, q, pubID).Scan(&rem.PublicationID, &rem.ProjectID,
		&rem.CreatorUserID, &rem.DueDate, &rem.ProjectTitle, &platform)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reminder{}, ErrNotFound
	}
	if err != nil {
		return Reminder{}, fmt.Errorf("get publication for reminder: %w", err)
	}
	rem.Kind = ReminderManual
	rem.MissingPlatforms = missingFrom(platform)
	return rem, nil
}

// Digests — по каким проектам сегодня есть о чём писать в общий чат.
func (r *Repo) Digests(ctx context.Context, reminders []Reminder) ([]ProjectDigest, error) {
	if len(reminders) == 0 {
		return nil, nil
	}
	byProject := make(map[uuid.UUID]*ProjectDigest, 4)
	order := make([]uuid.UUID, 0, 4)
	seenCreator := make(map[string]bool, len(reminders))

	for _, rem := range reminders {
		// Сводка по проекту выключена — не заводим по нему строку вовсе.
		// Проверка до создания записи, иначе в выдачу попадёт пустая
		// сводка с нулями и уедет менеджерам как «ничего не горит».
		if !rem.autoping.ManagerDigest {
			continue
		}
		// «Завтра срок» — ещё не событие для чата менеджеров: ничего не
		// горит, человек в сроке. Проверка до создания записи, иначе по
		// проекту, где на завтра стоит план и больше ничего, уедет
		// пустая сводка с нулями — то есть «ничего не горит» словами.
		if rem.Kind == ReminderDueTomorrow {
			continue
		}
		d, ok := byProject[rem.ProjectID]
		if !ok {
			d = &ProjectDigest{ProjectID: rem.ProjectID, ProjectTitle: rem.ProjectTitle}
			byProject[rem.ProjectID] = d
			order = append(order, rem.ProjectID)
		}
		switch rem.Kind {
		case ReminderOverdue:
			d.Overdue++
		case ReminderIncomplete:
			d.Incomplete++
		default:
			d.DueToday++
		}
		// «Кто виноват» бывает только там, где есть кому. У проекта без
		// креаторов список остаётся пустым, и сводка говорит «по каким
		// дням», а не «по кому».
		if rem.CreatorUserID != nil {
			key := rem.ProjectID.String() + rem.CreatorUserID.String()
			if !seenCreator[key] {
				seenCreator[key] = true
				d.Creators = append(d.Creators, *rem.CreatorUserID)
			}
		}
	}

	// Назначенный менеджер — чтобы в сводке было видно, чей это проект.
	ids := make([]uuid.UUID, 0, len(order))
	ids = append(ids, order...)
	rows, err := r.db.Query(ctx,
		`SELECT id, assigned_to_user_id FROM projects WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load project managers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid uuid.UUID
		var mid *uuid.UUID
		if err := rows.Scan(&pid, &mid); err != nil {
			return nil, fmt.Errorf("scan project manager: %w", err)
		}
		if d := byProject[pid]; d != nil {
			d.ManagerID = mid
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ProjectDigest, 0, len(order))
	for _, pid := range order {
		out = append(out, *byProject[pid])
	}
	return out, nil
}

// RunReminders — один проход планировщика: разослать напоминания за день
// и положить сводку менеджерам.
//
// Идемпотентен по построению: повторный запуск в тот же день ничего не
// отправит второй раз, а только увеличит Skipped.
func (s *Service) RunReminders(ctx context.Context, now time.Time) (RunStats, error) {
	var st RunStats

	reminders, err := s.repo.DueReminders(ctx, now)
	if err != nil {
		return st, err
	}
	st.Considered = len(reminders)

	for _, rem := range reminders {
		// Выключенный автопинг — не «отложить», а «не слать»: копить
		// молчание и вывалить его при включении было бы хуже, чем
		// не писать вовсе. В сводку менеджерам выкладка при этом
		// попадёт: то, что креатору не пишет бот, менеджеру знать надо.
		if !rem.autoping.enabled(rem.Kind) {
			remindersSuppressedTotal.Inc()
			st.Skipped++
			continue
		}
		// Поштучное письмо без адресата не бывает. У проекта без
		// креаторов такие напоминания не отправляются вовсе — ни в
		// личку (некому), ни в чат (шестьдесят «просрочка» за одно
		// утро по одному проекту). Их единственный след — строка в
		// дневной сводке ниже, и в неё выкладка попадает отсюда же.
		if rem.CreatorUserID == nil {
			st.Skipped++
			continue
		}
		sent, err := s.repo.Send(ctx, rem, rem.CreatorUserID, now)
		if err != nil {
			// Один упавший креатор не должен останавливать рассылку по
			// остальным — считаем и идём дальше.
			st.Failures++
			continue
		}
		if sent {
			remindersSentTotal.WithLabelValues(rem.Kind).Inc()
			st.Sent++
		} else {
			remindersSuppressedTotal.Inc()
			st.Skipped++
		}
	}

	// Ролики, которые вышли и не пошли. Отдельным проходом, не из
	// DueReminders: та выбирает ОТКРЫТЫЕ выкладки по срокам, а здесь
	// речь про уже вышедшие и про результат, а не про дату.
	lowViews, err := s.repo.LowViewsReminders(ctx, now)
	if err != nil {
		return st, err
	}
	st.Considered += len(lowViews)
	for _, rem := range lowViews {
		if !rem.autoping.enabled(rem.Kind) {
			remindersSuppressedTotal.Inc()
			st.Skipped++
			continue
		}
		// Поштучное письмо без адресата не бывает — то же правило, что
		// у остальных видов. У проекта без креаторов такой ролик
		// принадлежит проекту, и сказать о нём некому лично.
		if rem.CreatorUserID == nil {
			st.Skipped++
			continue
		}
		sent, err := s.repo.Send(ctx, rem, rem.CreatorUserID, now)
		if err != nil {
			st.Failures++
			continue
		}
		if sent {
			remindersSentTotal.WithLabelValues(rem.Kind).Inc()
			st.Sent++
		} else {
			remindersSuppressedTotal.Inc()
			st.Skipped++
		}
	}

	// План выкладок кончается — отдельным проходом, не из reminders:
	// там выборка идёт по выкладкам, а здесь предмет — проект, и он
	// попадает в неё как раз тогда, когда выкладок не осталось.
	endings, err := s.repo.DuePlanEndings(ctx, now)
	if err != nil {
		return st, err
	}
	for _, e := range endings {
		sent, err := s.repo.SendPlanEnding(ctx, e, now)
		if err != nil {
			st.Failures++
			continue
		}
		if sent {
			remindersSentTotal.WithLabelValues(ReminderPlanEnding).Inc()
			st.PlanEndings++
		}
	}

	digests, err := s.repo.Digests(ctx, reminders)
	if err != nil {
		return st, err
	}
	for _, d := range digests {
		sent, err := s.repo.SendDigest(ctx, d, now)
		if err != nil {
			st.Failures++
			continue
		}
		if sent {
			remindersSentTotal.WithLabelValues(ReminderManagerDigest).Inc()
			st.Digests++
		}
	}
	return st, nil
}

// RemindNow — менеджер напоминает руками, не дожидаясь утра.
//
// Своим kind: иначе ручное напоминание молча гасилось бы дедупом уже
// ушедшего автоматического. Но и оно логируется — чтобы кнопку нельзя
// было нажать двадцать раз подряд.
func (s *Service) RemindNow(ctx context.Context, pubID uuid.UUID, now time.Time) (bool, error) {
	rem, err := s.repo.GetPublicationForReminder(ctx, pubID)
	if err != nil {
		return false, err
	}
	// Напомнить некому: у выкладки проекта без креаторов нет владельца,
	// и кнопка напоминала бы менеджеру о самом себе. Отказываем явно, а
	// не отправляем в никуда.
	if rem.CreatorUserID == nil {
		return false, ErrNoCreator
	}
	return s.repo.Send(ctx, rem, rem.CreatorUserID, now)
}

// DefaultReminderHour — с какого часа местного времени можно слать
// напоминания. Требование М3 говорит «утром в день дедлайна»: без этого
// порога первый же проход после полуночи разбудил бы креатора в 00:05.
const DefaultReminderHour = 9

// ReminderWindowHours — ширина окна рассылки в часах.
//
// Окно, а не «всё время после девяти»: при часовом тике открытая сверху
// проверка запускала полный проход пятнадцать раз в сутки. Четырнадцать
// из них не отправляли ничего — дедуп гасил всё, что уже ушло, — но
// каждый исправно вычитывал все открытые выкладки и открывал транзакцию
// на каждую. Три часа дают запас на перезапуск воркера и не превращают
// рассылку в фоновый шум.
const ReminderWindowHours = 3

// ReminderWindowOpen — наступило ли время рассылки. Проверка отдельной
// функцией, чтобы её можно было проверить тестом, не двигая часы.
//
// Если воркер лежал дольше окна и поднялся вечером, дневная рассылка
// пропускается. Это осознанный размен: дедуп по дате всё равно не даст
// отправить её дважды, а напоминание о дедлайне, пришедшее в полночь,
// пользы не приносит — оно придёт утром вместе со следующим.
func ReminderWindowOpen(now time.Time, afterHour int) bool {
	h := now.Hour()
	return h >= afterHour && h < afterHour+ReminderWindowHours
}

// OverdueHorizonDays — сколько дней после дедлайна продолжать напоминать.
//
// Выкладка остаётся planned, пока её кто-то не закроет, поэтому без
// горизонта выборка растёт вечно: через полгода это сотни строк, которые
// вычитываются на каждом проходе и по которым бесконечно шлются
// напоминания. Если за месяц никто не отреагировал, ещё одно сообщение
// ничего не изменит — это уже вопрос к менеджеру, а не к рассылке.
const OverdueHorizonDays = 30

// ---- «ролик вышел, а просмотров нет» ----

// LowViewsAfterDays — через сколько дней после выхода спрашиваем про
// просмотры.
//
// Двое суток: за первые часы площадка часто ещё не раздала, и ноль там
// ничего не значит. К исходу вторых — уже значит: живой ролик к этому
// времени набирает хоть что-то, а пустой столбик говорит не про
// качество, а про то, что до ленты он не дошёл. Раньше спрашивать —
// дёргать людей по роликам, которые просто не успели разойтись.
const LowViewsAfterDays = 2

// LowViewsThreshold — ниже скольки просмотров считаем, что не пошло.
//
// Сто: это не планка качества, а граница «что-то не так». Ролик, который
// за двое суток не набрал и сотни, почти всегда не дошёл до ленты — не
// тот адрес, теневой бан, закрытый аккаунт.
const LowViewsThreshold = 100

// LowViewsReminders — вышедшие ролики, по которым просмотров почти нет.
//
// Главное правило здесь — НЕ ПУТАТЬ «никто не посмотрел» с «мы не
// измерили». Первое — повод написать человеку, второе — наша поломка, и
// пинг по ней был бы ложной тревогой с обвинением невиновного. Поэтому
// в выборку попадают только ссылки, по которым снимок РЕАЛЬНО есть; у
// кого цифр нет вовсе, тот отсюда исключён целиком (его отдельно видно
// по crm_links_never_collected и подписи в отчёте).
//
// Шлётся один раз на выкладку: дедуп в notification_log идёт по дате, а
// здесь нужен «когда-либо» — иначе каждое утро приходило бы «у тебя всё
// ещё ноль», а это не новость, это нытьё.
func (r *Repo) LowViewsReminders(ctx context.Context, today time.Time) ([]Reminder, error) {
	day := truncateDay(today)
	const q = `
WITH measured AS (
    -- Последний снимок каждой ссылки. Итог — по последнему, а не сумма
    -- по дням: просмотры накопительные, и сумма завысила бы кратно.
    SELECT DISTINCT ON (d.link_id) d.link_id, d.views
    FROM video_stat_daily d
    ORDER BY d.link_id, d.stat_date DESC
),
pubs AS (
    SELECT p.id, p.project_id, p.creator_user_id, p.due_date, pr.title,
           MIN(l.submitted_at) AS first_submitted,
           COUNT(*) FILTER (WHERE m.link_id IS NOT NULL) AS measured_links,
           COALESCE(SUM(m.views), 0) AS views,
           COALESCE(rp.low_views, TRUE) AS pref_low_views,
           COALESCE(rp.manager_digest, TRUE) AS pref_digest,
           COALESCE(rp.due_today, TRUE) AS pref_due_today,
           COALESCE(rp.overdue, TRUE) AS pref_overdue,
           COALESCE(rp.incomplete, TRUE) AS pref_incomplete,
           COALESCE(cp.day_before, rp.day_before, FALSE) AS pref_day_before
    FROM project_publications p
    JOIN projects pr ON pr.id = p.project_id
    JOIN publication_links l ON l.publication_id = p.id
    LEFT JOIN measured m ON m.link_id = l.id
    LEFT JOIN project_reminder_prefs rp ON rp.project_id = p.project_id
    LEFT JOIN project_creator_reminder_prefs cp
           ON cp.project_id = p.project_id AND cp.creator_user_id = p.creator_user_id
    WHERE p.status <> 'cancelled'
      AND pr.kind IN ('creators_turnkey', 'brand_turnkey')
    GROUP BY p.id, p.project_id, p.creator_user_id, p.due_date, pr.title,
             rp.low_views, rp.manager_digest, rp.due_today, rp.overdue,
             rp.incomplete, cp.day_before, rp.day_before
)
SELECT id, project_id, creator_user_id, due_date, title, views,
       pref_due_today, pref_overdue, pref_incomplete, pref_digest,
       pref_day_before, pref_low_views
FROM pubs
WHERE measured_links > 0
  AND views < $2
  AND first_submitted <= $1::timestamptz - make_interval(days => $3)
  -- Один раз на выкладку, а не каждое утро: «у тебя всё ещё ноль» —
  -- это не новость.
  AND NOT EXISTS (
      SELECT 1 FROM notification_log n
      WHERE n.kind = $4 AND n.subject_id = pubs.id
  )
ORDER BY first_submitted`
	rows, err := r.db.Query(ctx, q, day, LowViewsThreshold, LowViewsAfterDays, ReminderNoViews)
	if err != nil {
		return nil, fmt.Errorf("list low views reminders: %w", err)
	}
	defer rows.Close()

	out := make([]Reminder, 0, 8)
	for rows.Next() {
		var rem Reminder
		if err := rows.Scan(&rem.PublicationID, &rem.ProjectID, &rem.CreatorUserID,
			&rem.DueDate, &rem.ProjectTitle, &rem.Views,
			&rem.autoping.DueToday, &rem.autoping.Overdue, &rem.autoping.Incomplete,
			&rem.autoping.ManagerDigest, &rem.autoping.DayBefore,
			&rem.autoping.LowViews); err != nil {
			return nil, fmt.Errorf("scan low views reminder: %w", err)
		}
		rem.autoping.ProjectID = rem.ProjectID
		rem.Kind = ReminderNoViews
		out = append(out, rem)
	}
	return out, rows.Err()
}
