package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/audit"
)

// Месяц проекта: идёт или зафиксирован.
//
// Раньше месяца как сущности не было — был period_month в строке
// начисления. Утверждение начисления замораживало деньги, но не
// просмотры: числа, из которых выросла сумма, продолжали меняться, и
// через полгода рядом с прежней суммой стояли другие просмотры.
// Объяснить это было нечем.
//
// Фиксация закрывает месяц целиком: суммы пересчитываются в последний
// раз, а просмотры сохраняются срезом — по каждой выкладке и каждой
// площадке. Срез нужен не для красоты: оценка ролика, доли и процентили
// считаются по площадкам, и пересобрать их потом из живых данных значит
// получить другие числа.

// Состояния месяца. Текстом, а не enum'ом: следующим состоянием будет
// перенос недобора гарантии, и добавить его в CHECK дешевле, чем
// пересоздавать тип.
const (
	// MonthOpen — месяц идёт: всё считается на лету и помечается
	// «предварительно».
	MonthOpen = "open"
	// MonthLocked — зафиксирован: суммы и просмотры больше не меняются.
	MonthLocked = "locked"
)

// DefaultMonthLockDelay — через сколько после конца месяца он
// фиксируется сам.
//
// Четырнадцать дней — правило площадки: за две недели ролик набирает
// основную массу просмотров, дальше счёт почти не меняется. Раньше это
// правило нигде не исполнялось: оно держалось на том, что менеджер
// вовремя нажал «Пересчитать».
const DefaultMonthLockDelay = 14 * 24 * time.Hour

// ErrMonthLocked — попытка изменить зафиксированный месяц.
var ErrMonthLocked = errors.New("месяц зафиксирован")

// ProjectMonth — состояние месяца проекта.
type ProjectMonth struct {
	ProjectID   uuid.UUID `json:"project_id"`
	PeriodMonth time.Time `json:"period_month"`
	// Status — open | locked.
	Status   string     `json:"status"`
	LockedAt *time.Time `json:"locked_at,omitempty"`
	// LockedBy — кто зафиксировал. nil = фоновая задача.
	LockedBy *uuid.UUID `json:"locked_by,omitempty"`
	// SnapshotAsOf — на какую дату сняты числа месяца. Это отсечка
	// (конец месяца плюс две недели), а не момент фиксации: воркер мог
	// опоздать, и числа всё равно должны быть теми, что были на отсечку.
	SnapshotAsOf *time.Time `json:"snapshot_as_of,omitempty"`
	// SnapshotApprox — числам не на что опереться: поденный ряд к моменту
	// фиксации уже схлопнули, и восстановить просмотры на отсечку
	// неоткуда.
	//
	// Не путать с «предварительно» (Accrual.IsPreview): то про месяц,
	// который ещё идёт, это — про месяц, который зафиксирован, но
	// опирается на пустоту. Могут стоять одновременно, и в интерфейсе их
	// нельзя схлопывать в одну плашку.
	SnapshotApprox bool `json:"snapshot_approx,omitempty"`
}

// IsLocked — месяц зафиксирован.
func (m ProjectMonth) IsLocked() bool { return m.Status == MonthLocked }

// Month — состояние месяца. Строки может не быть вовсе: месяц, которого
// никто не трогал, идёт — заводить под это запись незачем.
func (r *Repo) Month(ctx context.Context, projectID uuid.UUID, month time.Time) (ProjectMonth, error) {
	period := firstOfMonth(month)
	out := ProjectMonth{ProjectID: projectID, PeriodMonth: period, Status: MonthOpen}
	err := r.db.QueryRow(ctx, `
SELECT status, locked_at, locked_by, snapshot_as_of, snapshot_approx
FROM project_months WHERE project_id = $1 AND period_month = $2`,
		projectID, period).Scan(&out.Status, &out.LockedAt, &out.LockedBy,
		&out.SnapshotAsOf, &out.SnapshotApprox)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return ProjectMonth{}, fmt.Errorf("load project month: %w", err)
	}
	return out, nil
}

// LockMonth — зафиксировать месяц: срез просмотров плюс отметка.
//
// Идемпотентна: повторный вызов на зафиксированном месяце ничего не
// меняет и ошибкой не считается. Так и должно быть — фоновая задача и
// кнопка менеджера легко приходят к одному месяцу вдвоём.
//
// Всё одной транзакцией: срез без отметки означал бы месяц, который
// считается живым по данным, снятым неизвестно когда.
func (r *Repo) LockMonth(ctx context.Context, projectID uuid.UUID, month time.Time, actor *uuid.UUID, asOf, now time.Time) (ProjectMonth, error) {
	period := firstOfMonth(month)
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ProjectMonth{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Блокируем строку месяца (или создаём её открытой): два
	// одновременных вызова не должны писать срез дважды.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_months (project_id, period_month) VALUES ($1, $2)
ON CONFLICT (project_id, period_month) DO NOTHING`, projectID, period); err != nil {
		return ProjectMonth{}, fmt.Errorf("ensure month row: %w", err)
	}
	var status string
	if err := tx.QueryRow(ctx, `
SELECT status FROM project_months
WHERE project_id = $1 AND period_month = $2 FOR UPDATE`, projectID, period).Scan(&status); err != nil {
		return ProjectMonth{}, fmt.Errorf("lock month row: %w", err)
	}
	if status == MonthLocked {
		// Уже зафиксирован — выходим молча, срез не переписываем.
		if err := tx.Commit(ctx); err != nil {
			return ProjectMonth{}, fmt.Errorf("commit: %w", err)
		}
		return r.Month(ctx, projectID, period)
	}

	approx, err := snapshotMonth(ctx, tx, projectID, period, asOf)
	if err != nil {
		return ProjectMonth{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE project_months
SET status = $3, locked_at = $4, locked_by = $5,
    snapshot_as_of = $6::date, snapshot_approx = $7, updated_at = now()
WHERE project_id = $1 AND period_month = $2`,
		projectID, period, MonthLocked, now, actor, asOf, approx); err != nil {
		return ProjectMonth{}, fmt.Errorf("mark month locked: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectMonth{}, fmt.Errorf("commit: %w", err)
	}
	return r.Month(ctx, projectID, period)
}

// snapshotMonth — срез месяца: какие выкладки в нём считались и сколько
// просмотров было у каждой площадки НА ОТСЕЧКУ.
//
// Отсечка, а не «последний снимок»: поденный ряд накопительный, и
// просмотры на любую дату достаются из него штатно — последняя строка,
// не позже отсечки. Брать последний снимок значило бы запомнить
// просмотры, набранные уже после неё, стоило воркеру опоздать на сутки.
//
// Границы месяца те же, что у расчёта начислений (periodFacts): выкладка
// принадлежит месяцу по due_date, отменённые не в счёт. Иначе срез и
// расчёт разошлись бы в составе.
//
// Возвращает признак приблизительности: поденный ряд схлопнут, и
// восстанавливать нечего.
func snapshotMonth(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, period time.Time, asOf time.Time) (bool, error) {
	to := period.AddDate(0, 1, 0)

	// Пересобираем срез с нуля: на случай расфиксации и повторной
	// фиксации старые строки не должны смешаться с новыми.
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_month_views WHERE project_id = $1 AND period_month = $2`,
		projectID, period); err != nil {
		return false, fmt.Errorf("clear views snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_month_publications WHERE project_id = $1 AND period_month = $2`,
		projectID, period); err != nil {
		return false, fmt.Errorf("clear publications snapshot: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_month_publications
    (project_id, period_month, publication_id, creator_user_id, status, due_date)
SELECT $1, $2, p.id, p.creator_user_id, p.status::text, p.due_date
FROM project_publications p
WHERE p.project_id = $1
  AND p.due_date >= $2 AND p.due_date < $3
  AND p.status <> 'cancelled'`, projectID, period, to); err != nil {
		return false, fmt.Errorf("snapshot publications: %w", err)
	}

	// Просмотры накопительные: берём последнюю строку ряда, не позже
	// отсечки, а не сумму по дням (сумма завысила бы в разы) и не
	// последнюю вообще (она может быть уже после отсечки).
	//
	// Строки на отсечку нет — пишем NULL, а не сегодняшнее число.
	// Пустое честнее завышенного: ноль неотличим от честного нуля
	// просмотров, а «данных не было» — это другое.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_month_views
    (project_id, period_month, publication_id, platform, link_id,
     views, likes, comments, stat_date, published_at)
SELECT $1, $2, p.id, l.platform, l.id,
       v.views, v.likes, v.comments, v.stat_date, l.published_at
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments, stat_date
    FROM video_stat_daily d
    WHERE d.link_id = l.id AND d.stat_date <= $4::date
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE
WHERE p.project_id = $1
  AND p.due_date >= $2 AND p.due_date < $3
  AND p.status <> 'cancelled'`, projectID, period, to, asOf); err != nil {
		return false, fmt.Errorf("snapshot views: %w", err)
	}

	// Приблизительный срез — это когда числа не просто отсутствуют, а
	// УНИЧТОЖЕНЫ: поденный ряд проекта схлопнут (есть итоговый снимок), и
	// на отсечку не нашлось ничего. Просто «ещё не собрали» — не
	// приблизительность, а честное отсутствие данных.
	var approx bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM project_stat_summary s WHERE s.project_id = $1)
   AND EXISTS (
       SELECT 1 FROM project_month_views v
       WHERE v.project_id = $1 AND v.period_month = $2 AND v.stat_date IS NULL
   )`, projectID, period).Scan(&approx); err != nil {
		return false, fmt.Errorf("detect approximate snapshot: %w", err)
	}
	return approx, nil
}

// UnlockMonth — вернуть месяц в работу.
//
// Только админ и только со следом в журнале: месяц, который нельзя
// переоткрыть, — это тупик, а переоткрытый молча — это разъехавшиеся
// числа без объяснения. Срез при этом удаляется: он перестал быть
// правдой, и оставлять его рядом с живым расчётом значит держать два
// ответа на один вопрос.
func (r *Repo) UnlockMonth(ctx context.Context, projectID uuid.UUID, month time.Time, actor uuid.UUID, reason string) error {
	period := firstOfMonth(month)
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx, `
SELECT status FROM project_months
WHERE project_id = $1 AND period_month = $2 FOR UPDATE`, projectID, period).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status == MonthOpen) {
		// Месяц и так идёт — расфиксировать нечего. Не ошибка: кнопку
		// могли нажать дважды.
		if err == nil {
			return tx.Commit(ctx)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock month row: %w", err)
	}

	var views int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM project_month_views WHERE project_id = $1 AND period_month = $2`,
		projectID, period).Scan(&views); err != nil {
		return fmt.Errorf("count snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE project_months
SET status = $3, locked_at = NULL, locked_by = NULL, updated_at = now()
WHERE project_id = $1 AND period_month = $2`, projectID, period, MonthOpen); err != nil {
		return fmt.Errorf("unlock month: %w", err)
	}
	// Срез уходит вместе с фиксацией: внешние ключи на project_months
	// стоят с ON DELETE CASCADE, но здесь строка месяца остаётся, так
	// что чистим явно.
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_month_views WHERE project_id = $1 AND period_month = $2`,
		projectID, period); err != nil {
		return fmt.Errorf("drop views snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_month_publications WHERE project_id = $1 AND period_month = $2`,
		projectID, period); err != nil {
		return fmt.Errorf("drop publications snapshot: %w", err)
	}
	if err := audit.Write(ctx, tx, actor, audit.ActionMonthUnlock,
		audit.ObjectProject, projectID.String(), map[string]any{
			"period_month":  period.Format("2006-01"),
			"snapshot_rows": views,
			"reason":        reason,
		}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DueForLock — месяцы, которым пора закрыться: месяц кончился больше
// delay назад, а фиксации нет.
//
// Список месяцев берём из самих выкладок: месяц проекта существует
// ровно тогда, когда в нём что-то стояло. Отдельного «календаря
// проекта» нет, и заводить его ради этого запроса незачем.
func (r *Repo) DueForLock(ctx context.Context, now time.Time, delay time.Duration, limit int) ([]ProjectMonth, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, `
SELECT p.project_id, date_trunc('month', p.due_date)::date AS period
FROM project_publications p
LEFT JOIN project_months m
       ON m.project_id = p.project_id
      AND m.period_month = date_trunc('month', p.due_date)::date
WHERE p.status <> 'cancelled'
  AND COALESCE(m.status, 'open') = 'open'
  -- Конец месяца плюс отсрочка: date_trunc + 1 месяц даёт первое число
  -- следующего, от него и считаем.
  AND (date_trunc('month', p.due_date) + interval '1 month' + $2::interval) <= $1
GROUP BY p.project_id, period
ORDER BY period
LIMIT $3`, now, delay, limit)
	if err != nil {
		return nil, fmt.Errorf("months due for lock: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectMonth, 0, limit)
	for rows.Next() {
		var m ProjectMonth
		if err := rows.Scan(&m.ProjectID, &m.PeriodMonth); err != nil {
			return nil, fmt.Errorf("scan month due: %w", err)
		}
		m.Status = MonthOpen
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- сервис ----

// Month — состояние месяца проекта.
func (s *Service) Month(ctx context.Context, projectID uuid.UUID, month time.Time) (ProjectMonth, error) {
	return s.repo.Month(ctx, projectID, month)
}

// LockMonth — зафиксировать месяц: снять срез просмотров на отсечку и
// пересчитать суммы уже по нему.
//
// Порядок именно такой. Пересчёт идёт ПОСЛЕ фиксации, потому что после
// неё monthFacts читает числа из среза: так сохранённые суммы и срез
// заведомо об одном и том же. Пересчитай до — и суммы посчитались бы по
// живым просмотрам, то есть по числам, которых на отсечку не было.
//
// Утверждённые и выплаченные строки пересчёт по-прежнему не трогает —
// SaveAccrual их пропускает.
//
// asOf — отсечка: конец месяца плюс отсрочка у фоновой фиксации,
// сегодняшний день у ручной. now — когда фиксируем на самом деле; эти
// два времени расходятся ровно тогда, когда воркер опоздал.
//
// actor nil = фоновая задача.
func (s *Service) LockMonth(ctx context.Context, projectID uuid.UUID, month time.Time, actor *uuid.UUID, asOf, now time.Time) (ProjectMonth, error) {
	current, err := s.repo.Month(ctx, projectID, month)
	if err != nil {
		return ProjectMonth{}, err
	}
	if current.IsLocked() {
		// Идемпотентность: фоновая задача и кнопка менеджера легко
		// приходят к одному месяцу вдвоём.
		return current, nil
	}
	m, err := s.repo.LockMonth(ctx, projectID, month, actor, asOf, now)
	if err != nil {
		return ProjectMonth{}, err
	}
	if m.SnapshotApprox {
		// Опереться не на что: ряд схлопнут, срез пуст. Пересчитать
		// сейчас — значит переписать сохранённые суммы нулями, то есть
		// потерять последнее, что от месяца осталось. Оставляем как есть
		// и честно помечаем месяц приблизительным.
		return m, nil
	}
	if _, err := s.Recalculate(ctx, projectID, month); err != nil {
		return m, err
	}
	return m, nil
}

// UnlockMonth — вернуть месяц в работу (только админ, со следом в журнале).
func (s *Service) UnlockMonth(ctx context.Context, projectID uuid.UUID, month time.Time, actor uuid.UUID, reason string) error {
	return s.repo.UnlockMonth(ctx, projectID, month, actor, reason)
}

// LockDueMonths — фоновая фиксация: закрыть все месяцы, которым пора.
//
// Возвращает, сколько закрыто. Ошибка на одном месяце не должна ронять
// весь проход: остальные проекты в этом не виноваты, а следующий тик
// попробует снова.
func (s *Service) LockDueMonths(ctx context.Context, now time.Time, delay time.Duration) (locked int, failed int, err error) {
	if delay <= 0 {
		delay = DefaultMonthLockDelay
	}
	due, err := s.repo.DueForLock(ctx, now, delay, 100)
	if err != nil {
		return 0, 0, err
	}
	for _, m := range due {
		// Отсечка считается от самого месяца, а не от «сейчас»: опоздание
		// воркера не должно менять числа, которые месяц запомнит.
		asOf := m.PeriodMonth.AddDate(0, 1, 0).Add(delay)
		if _, lerr := s.LockMonth(ctx, m.ProjectID, m.PeriodMonth, nil, asOf, now); lerr != nil {
			failed++
			continue
		}
		locked++
	}
	return locked, failed, nil
}
