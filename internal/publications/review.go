package publications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Проверка ролика менеджером.
//
// Чек-лист до сих пор был односторонним: креатор сам отмечал пункты при
// сдаче, и на этом всё. Кто посмотрел ролик и что с ним не так — жило в
// переписке, то есть нигде. Здесь появляется вторая сторона: по каждому
// пункту стоит «да» или «нет» именем менеджера, а решение по ролику
// («принят» / «возвращён с замечанием») одинаково видно обоим.
//
// Отметки креатора не переписываются и не заменяются: «я сделал» и «я
// проверил» — разные утверждения, и вся польза проверки в том, что они
// расходятся.

const (
	ReviewInReview = "in_review"
	ReviewReturned = "returned"
	ReviewAccepted = "accepted"
)

// Решения, которые менеджер отправляет с панели проверки.
const (
	// DecisionSave — сохранить ход проверки, не вынося решения. Ролик
	// смотрят частями, и отметки не должны теряться между заходами.
	DecisionSave   = ""
	DecisionReturn = "return"
	DecisionAccept = "accept"
)

var (
	// ErrNothingToReview — проверять нечего: ссылок нет. Отдельная
	// ошибка, потому что это не поломка, а естественное состояние
	// запланированной выкладки.
	ErrNothingToReview = errors.New("по выкладке нет ни одной ссылки")
	// ErrReviewBlocked — принять нельзя: обязательный пункт не пройден.
	// Правило то же, что у креатора при сдаче, и держит его сервер:
	// кнопку на фронте можно погасить, а можно и забыть.
	ErrReviewBlocked = errors.New("не пройдены обязательные пункты")
	// ErrForeignChecklistItem — вердикт по пункту чужого проекта.
	ErrForeignChecklistItem = errors.New("пункт не из чек-листа этого проекта")
)

// ReviewMark — вердикт менеджера по одному пункту чек-листа.
type ReviewMark struct {
	ItemID uuid.UUID `json:"item_id"`
	// Passed — «да» или «нет». Отсутствие строки означает третье
	// состояние — «ещё не смотрел», и оно не равно «нет».
	Passed bool `json:"passed"`
}

// PublicationReview — состояние проверки ролика.
type PublicationReview struct {
	Status string `json:"status"`
	// Comment — замечание последнего решения. При возврате обязательно:
	// «вернули молча» креатор всё равно придёт выяснять словами.
	Comment string `json:"comment,omitempty"`
	// Round — какая это по счёту сдача. Больше единицы значит, что ролик
	// уже возвращали.
	Round         int        `json:"round"`
	DecidedBy     *uuid.UUID `json:"decided_by,omitempty"`
	DecidedByName string     `json:"decided_by_name,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	// Marks — вердикты по пунктам; пункта без строки менеджер ещё не
	// касался.
	Marks []ReviewMark `json:"marks"`
}

// Failed — пункты, по которым стоит «нет».
func (rv PublicationReview) Failed() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rv.Marks))
	for _, m := range rv.Marks {
		if !m.Passed {
			out = append(out, m.ItemID)
		}
	}
	return out
}

// ReviewInput — то, что отправляет панель проверки: весь набор вердиктов
// целиком плюс решение.
type ReviewInput struct {
	PublicationID uuid.UUID
	ManagerUserID uuid.UUID
	// Marks — полный набор проставленных вердиктов. Именно полный:
	// снятая отметка приходит отсутствием строки, а не отдельным
	// «удалить».
	Marks    []ReviewMark
	Comment  string
	Decision string
}

// Review — сохранить ход проверки или вынести решение.
func (s *Service) Review(ctx context.Context, in ReviewInput) (Publication, error) {
	switch in.Decision {
	case DecisionSave, DecisionReturn, DecisionAccept:
	default:
		return Publication{}, fmt.Errorf("%w: неизвестное решение %q", ErrInvalidInput, in.Decision)
	}
	in.Comment = strings.TrimSpace(in.Comment)
	if utf8.RuneCountInString(in.Comment) > 1000 {
		return Publication{}, fmt.Errorf("%w: замечание длиннее 1000 символов", ErrInvalidInput)
	}
	if in.Decision == DecisionReturn && in.Comment == "" {
		// Возврат без слов — это отказ без причины: креатор всё равно
		// спросит, что не так, только уже в чате и на следующий день.
		return Publication{}, fmt.Errorf("%w: возврат требует замечания", ErrInvalidInput)
	}
	seen := make(map[uuid.UUID]bool, len(in.Marks))
	clean := make([]ReviewMark, 0, len(in.Marks))
	for _, m := range in.Marks {
		if m.ItemID == uuid.Nil || seen[m.ItemID] {
			continue
		}
		seen[m.ItemID] = true
		clean = append(clean, m)
	}
	in.Marks = clean
	return s.repo.Review(ctx, in)
}

// Review — транзакция проверки: вердикты, решение, событие.
func (r *Repo) Review(ctx context.Context, in ReviewInput) (Publication, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Publication{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, err := lockPublicationForEdit(ctx, tx, in.PublicationID)
	if err != nil {
		return Publication{}, err
	}

	platforms, err := publicationPlatforms(ctx, tx, in.PublicationID)
	if err != nil {
		return Publication{}, err
	}
	if len(platforms) == 0 {
		return Publication{}, ErrNothingToReview
	}

	items, err := checklistItemsTx(ctx, tx, projectID)
	if err != nil {
		return Publication{}, err
	}
	known := make(map[uuid.UUID]ChecklistItem, len(items))
	for _, it := range items {
		known[it.ID] = it
	}
	for _, m := range in.Marks {
		if _, ok := known[m.ItemID]; !ok {
			return Publication{}, ErrForeignChecklistItem
		}
	}

	// Принять можно только то, что проверено целиком: каждый
	// обязательный пункт, относящийся к сданным площадкам, должен стоять
	// «да». Пункт, до которого менеджер не дошёл, держит кнопку так же,
	// как пункт с «нет»: непроверенное — не пройденное.
	//
	// Список «что спрашивать» берётся той же функцией, что и у креатора
	// при сдаче (assertChecklist): требования к одному ролику у двух
	// сторон обязаны совпадать до пункта.
	if in.Decision == DecisionAccept {
		passed := make(map[uuid.UUID]bool, len(in.Marks))
		for _, m := range in.Marks {
			passed[m.ItemID] = m.Passed
		}
		missing := missingChecklistTexts(requiredChecklistFor(items, platforms), passed)
		if len(missing) > 0 {
			return Publication{}, fmt.Errorf("%w: %v", ErrReviewBlocked, missing)
		}
	}

	status := ReviewInReview
	switch in.Decision {
	case DecisionReturn:
		status = ReviewReturned
	case DecisionAccept:
		status = ReviewAccepted
	}
	var decidedBy any
	var decidedAt any
	if in.Decision != DecisionSave {
		decidedBy, decidedAt = in.ManagerUserID, time.Now()
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO publication_reviews (publication_id, status, comment, decided_by, decided_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (publication_id) DO UPDATE
SET status = EXCLUDED.status,
    comment = EXCLUDED.comment,
    decided_by = EXCLUDED.decided_by,
    decided_at = EXCLUDED.decided_at,
    updated_at = now()`,
		in.PublicationID, status, in.Comment, decidedBy, decidedAt); err != nil {
		return Publication{}, fmt.Errorf("save review: %w", err)
	}

	// Вердикты приходят целиком, поэтому и заменяются целиком: снятая
	// отметка приходит отсутствием строки.
	if _, err := tx.Exec(ctx,
		`DELETE FROM publication_review_marks WHERE publication_id = $1`, in.PublicationID); err != nil {
		return Publication{}, fmt.Errorf("clear review marks: %w", err)
	}
	for _, m := range in.Marks {
		if _, err := tx.Exec(ctx, `
INSERT INTO publication_review_marks (publication_id, item_id, passed, marked_by)
VALUES ($1, $2, $3, $4)`, in.PublicationID, m.ItemID, m.Passed, in.ManagerUserID); err != nil {
			return Publication{}, fmt.Errorf("save review mark: %w", err)
		}
	}

	if in.Decision != DecisionSave {
		event := outbox.EventPublicationAccepted
		if in.Decision == DecisionReturn {
			event = outbox.EventPublicationReturned
		}
		failed := make([]string, 0, 2)
		for _, m := range in.Marks {
			if !m.Passed {
				failed = append(failed, known[m.ItemID].Text)
			}
		}
		if err := outbox.Emit(ctx, tx, outbox.AggregateProject, projectID.String(), event,
			map[string]any{
				"project_id":     projectID,
				"publication_id": in.PublicationID,
				"comment":        in.Comment,
				"failed_items":   failed,
				"decided_by":     in.ManagerUserID,
			}); err != nil {
			return Publication{}, fmt.Errorf("emit review decision: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Publication{}, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, in.PublicationID)
}

// publicationPlatforms — площадки, по которым ссылки уже сданы.
func publicationPlatforms(ctx context.Context, tx pgx.Tx, pubID uuid.UUID) (map[string]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT platform FROM publication_links WHERE publication_id = $1`, pubID)
	if err != nil {
		return nil, fmt.Errorf("list publication platforms: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool, len(AllPlatforms))
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan platform: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}

// reopenReview — новая сдача отменяет прежнее решение.
//
// Вызывается из сдачи ссылок. Ролик стал другим: и «принято», и
// «возвращено» относились к прежнему, а вердикты по пунктам — к тому,
// что менеджер видел своими глазами. Оставить их значило бы показывать
// проверку чужого ролика; оставить замечание — показывать креатору
// претензию, которую он уже устранил. Остаётся round: по нему видно,
// что ролик переснимали.
func reopenReview(ctx context.Context, tx pgx.Tx, pubID uuid.UUID) error {
	var round int
	err := tx.QueryRow(ctx, `
UPDATE publication_reviews
SET status = 'in_review', comment = '', round = round + 1,
    decided_by = NULL, decided_at = NULL, updated_at = now()
WHERE publication_id = $1 AND status <> 'in_review'
RETURNING round`, pubID).Scan(&round)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reopen review: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM publication_review_marks WHERE publication_id = $1`, pubID); err != nil {
		return fmt.Errorf("clear review marks: %w", err)
	}
	return nil
}
