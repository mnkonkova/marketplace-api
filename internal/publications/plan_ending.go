package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// «План выкладок заканчивается».
//
// Проект с креаторами живёт расписанием: менеджер проставил даты на
// месяц вперёд, и до последней из них всё идёт само. Момент, когда даты
// кончились, не виден ниоткуда — на экране просто нет просрочек, и он
// выглядит как спокойный. Замечают его постфактум: креаторы сдали
// последний ролик, неделю никто ничего не делает, а клиент спрашивает,
// почему остановилось.
//
// Поэтому предупреждаем заранее — за две недели с небольшим: этого
// хватает, чтобы согласовать следующий месяц с заказчиком и проставить
// даты, не устраивая гонку.

const (
	// ReminderPlanEnding — менеджеру: расписание кончается.
	ReminderPlanEnding = "project_plan_ending"

	// PlanEndingLeadDays — за сколько дней до последней даты плана
	// предупреждаем.
	PlanEndingLeadDays = 15

	// PlanEndingRepeatDays — как часто повторять, пока план не продлили.
	//
	// Один выстрел ровно за 15 дней был бы точнее, но теряется: воркер
	// лежал сутки — и предупреждение пропало совсем. Раз в неделю —
	// компромисс между «не проспать» и «не превратиться в шум»: пока
	// даты не проставлены, напоминание придёт ещё два раза, а как
	// только проставлены, замолчит само.
	PlanEndingRepeatDays = 7
)

// PlanEnding — проект, у которого кончается расписание.
type PlanEnding struct {
	ProjectID    uuid.UUID  `json:"project_id"`
	ProjectTitle string     `json:"project_title"`
	ManagerID    *uuid.UUID `json:"manager_id,omitempty"`
	// LastDueDate — последняя проставленная дата выкладки.
	LastDueDate time.Time `json:"last_due_date"`
	// DaysLeft — сколько дней до неё осталось. Может быть и нулём:
	// предупреждение повторяется, пока план не продлили.
	DaysLeft int `json:"days_left"`
	// OpenLeft — сколько выкладок из оставшихся ещё не сдано. Ноль
	// означает, что работа уже кончилась, а не «вот-вот кончится».
	OpenLeft int `json:"open_left"`
}

// DuePlanEndings — по каким проектам пора предупредить.
//
// Условие «последняя дата не дальше чем через PlanEndingLeadDays» — по
// ВСЕМ неотменённым выкладкам, а не только по несданным: план кончается
// тогда, когда кончаются даты, независимо от того, сдали по ним ролики
// или нет.
//
// Повтор гасится журналом: ищем свою же запись за последние
// PlanEndingRepeatDays дней. Дедуп по дате из notification_log здесь не
// годится — он суточный, и предупреждение приходило бы каждый день.
func (r *Repo) DuePlanEndings(ctx context.Context, today time.Time) ([]PlanEnding, error) {
	day := truncateDay(today)
	rows, err := r.db.Query(ctx, `
SELECT pr.id, COALESCE(pr.title, ''), pr.assigned_to_user_id,
       MAX(p.due_date) AS last_due,
       COUNT(*) FILTER (WHERE p.status IN ('planned', 'partial')) AS open_left
FROM projects pr
JOIN project_publications p ON p.project_id = pr.id AND p.status <> 'cancelled'
WHERE pr.kind = 'creators_turnkey'
  AND pr.status = 'active'
  AND pr.is_test = FALSE
  -- Сбор по проекту остановлен — он закрывается, и расписание ему
  -- больше не нужно.
  AND (pr.collection_stops_at IS NULL OR pr.collection_stops_at > $1)
GROUP BY pr.id, pr.title, pr.assigned_to_user_id
HAVING MAX(p.due_date) <= $1::date + $2::int
   AND NOT EXISTS (
       SELECT 1 FROM notification_log n
       WHERE n.kind = $3 AND n.subject_id = pr.id
         AND n.sent_date > $1::date - $4::int
   )
ORDER BY last_due`, day, PlanEndingLeadDays, ReminderPlanEnding, PlanEndingRepeatDays)
	if err != nil {
		return nil, fmt.Errorf("list plan endings: %w", err)
	}
	defer rows.Close()

	out := make([]PlanEnding, 0, 4)
	for rows.Next() {
		var e PlanEnding
		if err := rows.Scan(&e.ProjectID, &e.ProjectTitle, &e.ManagerID,
			&e.LastDueDate, &e.OpenLeft); err != nil {
			return nil, fmt.Errorf("scan plan ending: %w", err)
		}
		e.DaysLeft = int(truncateDay(e.LastDueDate).Sub(day).Hours() / 24)
		if e.DaysLeft < 0 {
			e.DaysLeft = 0
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SendPlanEnding — записать предупреждение в журнал и положить событие.
//
// Получатель — назначенный менеджер лично; проект без менеджера уходит в
// общий чат (user_id = NULL), как и сводка: брать его некому, и это как
// раз тот случай, когда сказать надо всем сразу.
func (r *Repo) SendPlanEnding(ctx context.Context, e PlanEnding, sentDate time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING`, e.ManagerID, ReminderPlanEnding, e.ProjectID, truncateDay(sentDate))
	if err != nil {
		return false, fmt.Errorf("log plan ending: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := outbox.Emit(ctx, tx, outbox.AggregateProject, e.ProjectID.String(),
		"project."+ReminderPlanEnding, e); err != nil {
		return false, fmt.Errorf("emit plan ending: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
