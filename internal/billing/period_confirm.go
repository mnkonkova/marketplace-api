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

// Подтверждение конца периода менеджером.
//
// Границу считает автомат — месяц от первой выкладки, — и он остаётся
// главным путём: проект, в котором менеджер ничего не трогал, обязан
// считаться сам. Но план знает человек: последняя выкладка периода
// может стоять не в тот день, в который месяц кончается по арифметике.
// Поэтому подтверждённая дата СИЛЬНЕЕ вычисленной.
//
// Подтверждают только открытый период: под подытогом уже стоит счёт, и
// двигать его границу задним числом — это переписывать историю.

// ErrPeriodLocked — период подытожен, границу трогать нельзя.
var ErrPeriodLocked = errors.New("период подытожен: границу больше не двигают")

// ErrBadPeriodEnd — дата конца не годится.
var ErrBadPeriodEnd = errors.New("конец периода не может быть раньше его начала")

// ConfirmPeriodEnd — менеджер подтверждает, каким числом кончается период.
//
// Если дата та же, что стояла, — это просто отметка «я проверил»: она
// гасит тревогу в «Где горит» и больше ничего не меняет. Если другая —
// граница переезжает, и вместе с ней едет вся цепочка дальше: начало
// следующего периода, его конец, отсечка.
//
// Периоды после этого пересобираются заново, поэтому подытоженных среди
// них быть не должно: подытог замораживает счёт, и сдвинуть его границу
// значит разойтись с тем, что уже выставлено.
func (r *Repo) ConfirmPeriodEnd(
	ctx context.Context, periodID uuid.UUID, endsOn time.Time, actor uuid.UUID, now time.Time,
) (ProjectPeriod, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := scanPeriod(tx.QueryRow(ctx,
		`SELECT `+periodScanCols+` FROM project_periods WHERE id = $1 FOR UPDATE`, periodID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectPeriod{}, ErrNoPeriods
	}
	if err != nil {
		return ProjectPeriod{}, fmt.Errorf("lock period row: %w", err)
	}
	if p.IsLocked() {
		return ProjectPeriod{}, ErrPeriodLocked
	}

	day := dayOf(endsOn)
	if day.Before(dayOf(p.StartsOn)) {
		return ProjectPeriod{}, ErrBadPeriodEnd
	}

	// Периоды после этого нельзя сдвигать, если хоть один уже подытожен.
	var lockedAfter int
	if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM project_periods
WHERE project_id = $1 AND seq > $2 AND status = $3`,
		p.ProjectID, p.Seq, PeriodLocked).Scan(&lockedAfter); err != nil {
		return ProjectPeriod{}, fmt.Errorf("count locked after: %w", err)
	}
	moved := !day.Equal(dayOf(p.EndsOn))
	if moved && lockedAfter > 0 {
		return ProjectPeriod{}, ErrPeriodLocked
	}

	if _, err := tx.Exec(ctx, `
UPDATE project_periods
SET ends_on = $2, ends_on_confirmed_at = $3, ends_on_confirmed_by = $4, updated_at = now()
WHERE id = $1`, periodID, day, now, actor); err != nil {
		return ProjectPeriod{}, fmt.Errorf("confirm period end: %w", err)
	}

	// Хвост цепочки пересобираем от новой границы. Удаляем, а не
	// правим: число периодов после сдвига может измениться, и «подвинуть
	// каждый» пришлось бы писать тем же кодом, что их и создаёт.
	if moved {
		if _, err := tx.Exec(ctx,
			`DELETE FROM project_periods WHERE project_id = $1 AND seq > $2`,
			p.ProjectID, p.Seq); err != nil {
			return ProjectPeriod{}, fmt.Errorf("drop periods after: %w", err)
		}
	}

	if err := audit.Write(ctx, tx, actor, audit.ActionPeriodConfirmEnd,
		audit.ObjectProject, p.ProjectID.String(), map[string]any{
			"period_seq": p.Seq,
			"was":        dayOf(p.EndsOn).Format("2006-01-02"),
			"now":        day.Format("2006-01-02"),
			"moved":      moved,
		}); err != nil {
		return ProjectPeriod{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectPeriod{}, fmt.Errorf("commit: %w", err)
	}

	// Пересобрать хвост: EnsurePeriods доведёт цепочку до сегодняшнего
	// дня, уже от подтверждённой границы.
	if moved {
		if err := r.EnsurePeriods(ctx, p.ProjectID, now); err != nil {
			return ProjectPeriod{}, err
		}
	}
	return r.PeriodBySeq(ctx, p.ProjectID, p.Seq)
}

// PeriodNeedsConfirm — период, конец которого ждёт подтверждения.
type PeriodNeedsConfirm struct {
	PeriodID     uuid.UUID  `json:"period_id"`
	ProjectID    uuid.UUID  `json:"project_id"`
	ProjectTitle string     `json:"project_title"`
	ManagerID    *uuid.UUID `json:"manager_id,omitempty"`
	Seq          int        `json:"seq"`
	StartsOn     time.Time  `json:"starts_on"`
	// EndsOn — что предлагаем подтвердить: вычисленная граница.
	EndsOn time.Time `json:"ends_on"`
	// LastDueDate — последняя выкладка, стоящая в плане внутри периода.
	// Ради неё подтверждение и существует: если она раньше границы,
	// период кончается работой, а не календарём.
	LastDueDate *time.Time `json:"last_due_date,omitempty"`
}

// PeriodsNeedingConfirm — по каким периодам спросить менеджера.
//
// Спрашиваем, когда план уже проставлен (есть хоть одна выкладка в
// границах периода) и никто ещё не подтверждал. Подытоженные не
// спрашиваем вовсе: поздно.
func (r *Repo) PeriodsNeedingConfirm(ctx context.Context, now time.Time) ([]PeriodNeedsConfirm, error) {
	rows, err := r.db.Query(ctx, `
SELECT pp.id, pp.project_id, COALESCE(pr.title, ''), pr.assigned_to_user_id,
       pp.seq, pp.starts_on, pp.ends_on,
       (SELECT MAX(p.due_date) FROM project_publications p
         WHERE p.project_id = pp.project_id AND p.status <> 'cancelled'
           AND p.due_date BETWEEN pp.starts_on AND pp.ends_on)
FROM project_periods pp
JOIN projects pr ON pr.id = pp.project_id
WHERE pp.status = $1
  AND pp.ends_on_confirmed_at IS NULL
  AND pr.kind = 'creators_turnkey'
  AND pr.status = 'active'
  AND pr.is_test = FALSE
  AND EXISTS (
      SELECT 1 FROM project_publications p
      WHERE p.project_id = pp.project_id AND p.status <> 'cancelled'
        AND p.due_date BETWEEN pp.starts_on AND pp.ends_on)
ORDER BY pp.ends_on, pp.seq`, PeriodOpen)
	if err != nil {
		return nil, fmt.Errorf("periods needing confirm: %w", err)
	}
	defer rows.Close()

	out := make([]PeriodNeedsConfirm, 0, 8)
	for rows.Next() {
		var c PeriodNeedsConfirm
		if err := rows.Scan(&c.PeriodID, &c.ProjectID, &c.ProjectTitle, &c.ManagerID,
			&c.Seq, &c.StartsOn, &c.EndsOn, &c.LastDueDate); err != nil {
			return nil, fmt.Errorf("scan period confirm: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConfirmPeriodEnd — подтверждение конца периода менеджером.
func (s *Service) ConfirmPeriodEnd(
	ctx context.Context, periodID uuid.UUID, endsOn time.Time, actor uuid.UUID, now time.Time,
) (ProjectPeriod, error) {
	return s.repo.ConfirmPeriodEnd(ctx, periodID, endsOn, actor, now)
}

// PeriodsNeedingConfirm — по каким периодам спросить менеджера.
func (s *Service) PeriodsNeedingConfirm(ctx context.Context, now time.Time) ([]PeriodNeedsConfirm, error) {
	return s.repo.PeriodsNeedingConfirm(ctx, now)
}
