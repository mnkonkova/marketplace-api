package publications

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Задание креатора — материалы и чеклист — меняется по ходу проекта, и
// до сих пор об этом никто никому не говорил.
//
// Менеджер добавлял бриф, референс или пункт чеклиста, а креатор узнавал
// об этом, только если сам открывал проект. На практике он снимал по
// вчерашнему заданию и переснимал после проверки — то есть цена молчания
// была в пересъёмке.
//
// Отдельные виды на каждый случай, а не один общий: журнал уведомлений
// гасит повтор по паре «вид + день», и общий вид склеил бы правку
// материалов с правкой чеклиста в одно сообщение — пришло бы только
// первое.
const (
	// ReminderMaterialsUpdated — в проект добавили или убрали материал.
	ReminderMaterialsUpdated = "project_materials_updated"
	// ReminderChecklistUpdated — изменился чеклист сдачи.
	ReminderChecklistUpdated = "project_checklist_updated"
	// ReminderCreatorBriefed — креатора добавили в проект, и задание у
	// проекта уже есть. Без задания молчим: сообщение «вот твои
	// материалы: ноль» хуже, чем его отсутствие.
	ReminderCreatorBriefed = "project_creator_briefed"
)

// BriefUpdate — что и кому сообщаем.
type BriefUpdate struct {
	ProjectID    uuid.UUID   `json:"project_id"`
	ProjectTitle string      `json:"project_title"`
	CreatorIDs   []uuid.UUID `json:"creator_ids"`
	// Kind — вид уведомления, он же причина: по нему бот выбирает текст.
	Kind string `json:"kind"`
	// Materials/Checklist — сколько их у проекта сейчас. Числа нужны
	// самому сообщению: «в задании три материала и пять пунктов».
	Materials int `json:"materials"`
	Checklist int `json:"checklist"`
}

// notifyBrief — сказать креаторам проекта, что задание изменилось.
//
// Зовётся ВНУТРИ транзакции, которая это изменение и делает: уведомление
// об изменении, которого не случилось, — худший вид вранья, а разнести
// их по разным транзакциям значит когда-нибудь его получить.
//
// only не пуст — сообщаем только этим людям (так уходит приветствие
// новому креатору); пусто — всему действующему составу.
//
// Ошибка возвращается наружу: молча проглоченное уведомление отличить от
// отправленного потом невозможно.
func notifyBrief(
	ctx context.Context, tx pgx.Tx, projectID uuid.UUID, kind string, only []uuid.UUID, now time.Time,
) error {
	var (
		title      string
		materials  int
		checklist  int
		recipients []uuid.UUID
	)
	if err := tx.QueryRow(ctx, `
SELECT p.title,
       -- 'creators', а не 'creator'/'all': вторых в CHECK нет вовсе
       -- (migrations/00036_materials_autoping.sql), и счётчик материалов
       -- в задании всегда приезжал нулём — «материалов 0» при полном
       -- проекте материалов.
       (SELECT count(*) FROM project_materials m
         WHERE m.project_id = p.id AND m.delivery_id IS NULL
           AND m.audience = 'creators'),
       (SELECT count(*) FROM project_checklist_items c
         WHERE c.project_id = p.id)
FROM projects p WHERE p.id = $1`, projectID).Scan(&title, &materials, &checklist); err != nil {
		return fmt.Errorf("brief counts: %w", err)
	}

	if len(only) > 0 {
		recipients = only
	} else {
		rows, err := tx.Query(ctx,
			`SELECT creator_user_id FROM project_creators
             WHERE project_id = $1 AND removed_at IS NULL`, projectID)
		if err != nil {
			return fmt.Errorf("brief recipients: %w", err)
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan brief recipient: %w", err)
			}
			recipients = append(recipients, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if len(recipients) == 0 {
		// Состава ещё нет — сообщать некому. Новый креатор получит
		// задание в момент, когда его добавят.
		return nil
	}

	// Журнал гасит повтор: менеджер правит задание пачкой, и пять
	// добавленных подряд материалов — это одна новость, а не пять.
	day := truncateDay(now)
	sent := make([]uuid.UUID, 0, len(recipients))
	for _, id := range recipients {
		tag, err := tx.Exec(ctx, `
INSERT INTO notification_log (user_id, kind, subject_id, sent_date)
VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, id, kind, projectID, day)
		if err != nil {
			return fmt.Errorf("log brief update: %w", err)
		}
		if tag.RowsAffected() > 0 {
			sent = append(sent, id)
		}
	}
	if len(sent) == 0 {
		return nil
	}

	return outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(), "project."+kind,
		BriefUpdate{
			ProjectID: projectID, ProjectTitle: title, CreatorIDs: sent,
			Kind: kind, Materials: materials, Checklist: checklist,
		})
}

// notifyNewCreator — приветствие новому креатору: вот задание проекта.
//
// Молчим, если задания ещё нет: пустое приветствие занимает внимание и
// ничего не сообщает, а как только материалы появятся, о них скажет
// обычное уведомление об изменении.
func notifyNewCreator(
	ctx context.Context, tx pgx.Tx, projectID, creatorID uuid.UUID, now time.Time,
) error {
	var has bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM project_materials m
                WHERE m.project_id = $1 AND m.delivery_id IS NULL
                  AND m.audience = 'creators')
    OR EXISTS (SELECT 1 FROM project_checklist_items c WHERE c.project_id = $1)`,
		projectID).Scan(&has); err != nil {
		return fmt.Errorf("brief exists: %w", err)
	}
	if !has {
		return nil
	}
	return notifyBrief(ctx, tx, projectID, ReminderCreatorBriefed, []uuid.UUID{creatorID}, now)
}
