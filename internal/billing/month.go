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
}

// IsLocked — месяц зафиксирован.
func (m ProjectMonth) IsLocked() bool { return m.Status == MonthLocked }

// Month — состояние месяца. Строки может не быть вовсе: месяц, которого
// никто не трогал, идёт — заводить под это запись незачем.
func (r *Repo) Month(ctx context.Context, projectID uuid.UUID, month time.Time) (ProjectMonth, error) {
	period := firstOfMonth(month)
	out := ProjectMonth{ProjectID: projectID, PeriodMonth: period, Status: MonthOpen}
	err := r.db.QueryRow(ctx, `
SELECT status, locked_at, locked_by
FROM project_months WHERE project_id = $1 AND period_month = $2`,
		projectID, period).Scan(&out.Status, &out.LockedAt, &out.LockedBy)
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
func (r *Repo) LockMonth(ctx context.Context, projectID uuid.UUID, month time.Time, actor *uuid.UUID, now time.Time) (ProjectMonth, error) {
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

	if err := snapshotMonth(ctx, tx, projectID, period); err != nil {
		return ProjectMonth{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE project_months
SET status = $3, locked_at = $4, locked_by = $5, updated_at = now()
WHERE project_id = $1 AND period_month = $2`,
		projectID, period, MonthLocked, now, actor); err != nil {
		return ProjectMonth{}, fmt.Errorf("mark month locked: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectMonth{}, fmt.Errorf("commit: %w", err)
	}
	return r.Month(ctx, projectID, period)
}

// snapshotMonth — срез месяца: какие выкладки в нём считались и сколько
// просмотров было у каждой площадки на этот момент.
//
// Границы месяца те же, что у расчёта начислений (periodFacts): выкладка
// принадлежит месяцу по due_date, отменённые не в счёт. Иначе срез и
// расчёт разошлись бы в составе.
func snapshotMonth(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, period time.Time) error {
	to := period.AddDate(0, 1, 0)

	// Пересобираем срез с нуля: на случай расфиксации и повторной
	// фиксации старые строки не должны смешаться с новыми.
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_month_views WHERE project_id = $1 AND period_month = $2`,
		projectID, period); err != nil {
		return fmt.Errorf("clear views snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM project_month_publications WHERE project_id = $1 AND period_month = $2`,
		projectID, period); err != nil {
		return fmt.Errorf("clear publications snapshot: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO project_month_publications
    (project_id, period_month, publication_id, creator_user_id, status, due_date)
SELECT $1, $2, p.id, p.creator_user_id, p.status::text, p.due_date
FROM project_publications p
WHERE p.project_id = $1
  AND p.due_date >= $2 AND p.due_date < $3
  AND p.status <> 'cancelled'`, projectID, period, to); err != nil {
		return fmt.Errorf("snapshot publications: %w", err)
	}

	// Просмотры накопительные, поэтому берём ПОСЛЕДНИЙ снимок каждой
	// ссылки, а не сумму по дням: сумма завысила бы в разы.
	if _, err := tx.Exec(ctx, `
INSERT INTO project_month_views
    (project_id, period_month, publication_id, platform, link_id, views, likes, comments, published_at)
SELECT $1, $2, p.id, l.platform, l.id,
       COALESCE(v.views, 0), v.likes, v.comments, l.published_at
FROM project_publications p
JOIN publication_links l ON l.publication_id = p.id
LEFT JOIN LATERAL (
    SELECT views, likes, comments
    FROM video_stat_daily d
    WHERE d.link_id = l.id
    ORDER BY d.stat_date DESC
    LIMIT 1
) v ON TRUE
WHERE p.project_id = $1
  AND p.due_date >= $2 AND p.due_date < $3
  AND p.status <> 'cancelled'`, projectID, period, to); err != nil {
		return fmt.Errorf("snapshot views: %w", err)
	}
	return nil
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

// LockMonth — зафиксировать месяц: пересчитать в последний раз и снять
// срез просмотров.
//
// Пересчёт перед фиксацией обязателен: иначе зафиксируется месяц с
// суммами, посчитанными неизвестно когда, рядом со свежим срезом
// просмотров. Утверждённые и выплаченные строки пересчёт по-прежнему не
// трогает — SaveAccrual их пропускает.
//
// actor nil = фоновая задача.
func (s *Service) LockMonth(ctx context.Context, projectID uuid.UUID, month time.Time, actor *uuid.UUID, now time.Time) (ProjectMonth, error) {
	current, err := s.repo.Month(ctx, projectID, month)
	if err != nil {
		return ProjectMonth{}, err
	}
	if current.IsLocked() {
		// Идемпотентность: фоновая задача и кнопка менеджера легко
		// приходят к одному месяцу вдвоём.
		return current, nil
	}
	if _, err := s.Recalculate(ctx, projectID, month); err != nil {
		return ProjectMonth{}, err
	}
	return s.repo.LockMonth(ctx, projectID, month, actor, now)
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
		if _, lerr := s.LockMonth(ctx, m.ProjectID, m.PeriodMonth, nil, now); lerr != nil {
			failed++
			continue
		}
		locked++
	}
	return locked, failed, nil
}
