package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"marketpclce/internal/outbox"
)

// Отклик креатора на рассылку.
//
// Отклик — это НЕ согласие и не место в составе: заявка одна, а
// откликнуться на неё могут двадцать. Состав из откликнувшихся собирает
// менеджер, и до этого момента у человека нет ни проекта, ни сроков.
// Поэтому строка отклика живёт в своей таблице, а не меняет заказ.

// ResponseMode — как человек ответил.
type ResponseMode string

const (
	// ModeAttach — приложил файл в кабинете: файл уже в нашем бакете.
	ModeAttach ResponseMode = "attach"
	// ModeUpload — прислал файл в бот, и мы кладём его ещё и в
	// портфолио: он присылал его НАМ, а искать потом пойдёт у себя.
	// Копию в портфолио делает та сторона, куда файл приехал, — здесь
	// только ссылка.
	ModeUpload ResponseMode = "upload"
	// ModeFromPortfolio — «отправить из моих»: грузить нечего,
	// показываем менеджеру уже загруженные ролики.
	ModeFromPortfolio ResponseMode = "from_portfolio"
	// ModeDecline — отказался. Строка нужна: «ответил нет» и «молчит»
	// для менеджера разные вещи, а второе выглядит как её отсутствие.
	ModeDecline ResponseMode = "decline"
)

// ErrNothingAttached — отклик без работы: ни файла, ни роликов.
var ErrNothingAttached = errors.New("response has neither a file nor portfolio items")

// ErrNotYourPortfolio — в отклике чужие ролики.
var ErrNotYourPortfolio = errors.New("portfolio item belongs to another creator")

// responseNoteMax — предел на комментарий к отклику. Не про технику:
// менеджер читает два десятка таких строк подряд, и «письмо» в них
// перестаёт читаться на третьем.
const responseNoteMax = 1000

// ResponseInput — что прислал креатор.
type ResponseInput struct {
	OrderID       uuid.UUID
	CreatorUserID uuid.UUID
	Mode          ResponseMode
	// FileURL — объект в бакете (префикс orders/). Для attach и upload.
	FileURL string
	// PortfolioItems — что показать из уже загруженного. Для
	// from_portfolio.
	PortfolioItems []uuid.UUID
	Note           string
}

// ResponseItem — ролик из портфолио, приложенный к отклику.
type ResponseItem struct {
	ID           uuid.UUID `json:"id"`
	Title        string    `json:"title"`
	VideoURL     string    `json:"video_url,omitempty"`
	PreviewURL   string    `json:"preview_url,omitempty"`
	ThumbnailURL string    `json:"thumbnail_url,omitempty"`
}

// Response — отклик, как его видит менеджер.
type Response struct {
	OrderID       uuid.UUID `json:"order_id"`
	CreatorUserID uuid.UUID `json:"creator_user_id"`
	CreatorName   string    `json:"creator_name,omitempty"`
	AvatarURL     string    `json:"avatar_url,omitempty"`
	// IsPreferred — заказчик отметил этого человека «хочу особенно».
	// В списке откликов это главный признак: из двадцати согласных
	// начинают с тех, кого просили.
	IsPreferred bool `json:"is_preferred"`
	// InCrew — человек уже в составе проекта. Без этого менеджер жмёт
	// «Взять в проект» второй раз и получает отказ вместо ответа.
	InCrew    bool           `json:"in_crew"`
	Mode      ResponseMode   `json:"mode"`
	FileURL   string         `json:"file_url,omitempty"`
	Note      string         `json:"note,omitempty"`
	Items     []ResponseItem `json:"items,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// SaveResponse — записать отклик креатора.
//
// Строка кандидата заводится, если её не было: рассылка ушла всем, и
// ответить может тот, кого заказчик не отмечал. Повторный отклик
// переписывает прежний — человек передумал и прислал другой ролик, и
// две строки от одного человека менеджеру только мешают.
func (r *Repo) SaveResponse(ctx context.Context, in ResponseInput, now time.Time) (Response, error) {
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > responseNoteMax {
		return Response{}, fmt.Errorf("%w: комментарий длиннее %d символов",
			ErrInvalidInput, responseNoteMax)
	}
	switch in.Mode {
	case ModeAttach, ModeUpload:
		if strings.TrimSpace(in.FileURL) == "" {
			return Response{}, ErrNothingAttached
		}
	case ModeFromPortfolio:
		if len(in.PortfolioItems) == 0 {
			return Response{}, ErrNothingAttached
		}
	case ModeDecline:
		in.FileURL = ""
		in.PortfolioItems = nil
	default:
		return Response{}, fmt.Errorf("%w: неизвестный способ ответа", ErrInvalidInput)
	}

	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var (
			status    OrderStatus
			projectID *uuid.UUID
		)
		if err := tx.QueryRow(ctx,
			`SELECT status, project_id FROM creator_orders WHERE id = $1 FOR UPDATE`,
			in.OrderID).Scan(&status, &projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		// Закрытую заявку не переоткрываем откликом: состав утверждён,
		// и «я тоже хочу» здесь уже ничего не меняет.
		if status == StatusCancelled || status == StatusFinalized || status == StatusPaid {
			return ErrWrongStatus
		}

		// Кандидат: заводим, если человека в подборке не было. Статус
		// «откликнулся» — не «согласился»: согласие подтверждает
		// менеджер, добавляя человека в состав. Принятого не трогаем:
		// он уже в проекте, и откат его в responded сломал бы состав.
		candidateStatus := CandidateResponded
		if in.Mode == ModeDecline {
			candidateStatus = CandidateDeclined
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO order_candidates (order_id, creator_user_id, priority, status, responded_at)
VALUES ($1, $2,
        (SELECT COALESCE(MAX(priority), 0) + 1 FROM order_candidates WHERE order_id = $1),
        $3, $4)
ON CONFLICT (order_id, creator_user_id) DO UPDATE
   SET status = CASE WHEN order_candidates.status = 'accepted'
                     THEN order_candidates.status ELSE EXCLUDED.status END,
       responded_at = EXCLUDED.responded_at`,
			in.OrderID, in.CreatorUserID, string(candidateStatus), now); err != nil {
			return fmt.Errorf("upsert candidate: %w", err)
		}

		if _, err := tx.Exec(ctx, `
INSERT INTO order_candidate_responses
    (order_id, creator_user_id, mode, file_url, note, created_at, updated_at)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $6)
ON CONFLICT (order_id, creator_user_id) DO UPDATE
   SET mode = EXCLUDED.mode, file_url = EXCLUDED.file_url,
       note = EXCLUDED.note, updated_at = EXCLUDED.updated_at`,
			in.OrderID, in.CreatorUserID, string(in.Mode), in.FileURL, in.Note, now); err != nil {
			return fmt.Errorf("save response: %w", err)
		}

		// Ролики отклика переписываем целиком: «прислал другие» — это
		// другой набор, а не добавка к прежнему.
		if _, err := tx.Exec(ctx,
			`DELETE FROM order_response_portfolio_items
              WHERE order_id = $1 AND creator_user_id = $2`,
			in.OrderID, in.CreatorUserID); err != nil {
			return fmt.Errorf("clear response items: %w", err)
		}
		if len(in.PortfolioItems) > 0 {
			// Чужие ролики в отклик не попадают: иначе показать
			// менеджеру можно было бы чью угодно работу.
			var mine int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM portfolio_items WHERE id = ANY($1) AND user_id = $2`,
				in.PortfolioItems, in.CreatorUserID).Scan(&mine); err != nil {
				return fmt.Errorf("check portfolio: %w", err)
			}
			if mine != len(in.PortfolioItems) {
				return ErrNotYourPortfolio
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO order_response_portfolio_items (order_id, creator_user_id, item_id)
SELECT $1, $2, i FROM unnest($3::uuid[]) AS t(i)`,
				in.OrderID, in.CreatorUserID, in.PortfolioItems); err != nil {
				return fmt.Errorf("save response items: %w", err)
			}
		}

		payload := map[string]any{
			"order_id":   in.OrderID,
			"creator_id": in.CreatorUserID,
			"mode":       string(in.Mode),
		}
		withProject(payload, projectID)
		return outbox.Emit(ctx, tx, outbox.AggregateProject, in.OrderID.String(),
			outbox.EventOrderResponseReceived, payload)
	})
	if err != nil {
		return Response{}, err
	}
	return r.ResponseOf(ctx, in.OrderID, in.CreatorUserID)
}

// ResponseOf — отклик одного человека. Нужен и самому креатору: его
// экран показывает, что именно он прислал.
func (r *Repo) ResponseOf(ctx context.Context, orderID, creatorID uuid.UUID) (Response, error) {
	list, err := r.listResponses(ctx, orderID, &creatorID)
	if err != nil {
		return Response{}, err
	}
	if len(list) == 0 {
		return Response{}, ErrNotFound
	}
	return list[0], nil
}

// ListResponses — кто откликнулся на заявку. Отмеченные заказчиком
// сверху: из двадцати согласных менеджер начинает с тех, кого просили.
func (r *Repo) ListResponses(ctx context.Context, orderID uuid.UUID) ([]Response, error) {
	return r.listResponses(ctx, orderID, nil)
}

func (r *Repo) listResponses(
	ctx context.Context, orderID uuid.UUID, only *uuid.UUID,
) ([]Response, error) {
	rows, err := r.db.Query(ctx, `
SELECT resp.creator_user_id,
       COALESCE(
         NULLIF(sp.display_name, ''),
         NULLIF(cp.display_name, ''),
         split_part(u.email, '@', 1),
         ''
       ),
       COALESCE(sp.avatar_url, ''),
       c.is_preferred,
       EXISTS (SELECT 1 FROM project_creators pc
                JOIN creator_orders o2 ON o2.project_id = pc.project_id
               WHERE o2.id = resp.order_id
                 AND pc.creator_user_id = resp.creator_user_id
                 AND pc.removed_at IS NULL),
       resp.mode, COALESCE(resp.file_url, ''), resp.note, resp.created_at
FROM order_candidate_responses resp
JOIN order_candidates c          ON c.order_id = resp.order_id
                                AND c.creator_user_id = resp.creator_user_id
LEFT JOIN users u                ON u.id = resp.creator_user_id
LEFT JOIN specialist_profiles sp ON sp.user_id = resp.creator_user_id
LEFT JOIN client_profiles cp     ON cp.user_id = resp.creator_user_id
WHERE resp.order_id = $1
  AND ($2::uuid IS NULL OR resp.creator_user_id = $2)
ORDER BY c.is_preferred DESC, resp.created_at`, orderID, only)
	if err != nil {
		return nil, fmt.Errorf("list responses: %w", err)
	}
	defer rows.Close()
	out := make([]Response, 0, 8)
	byCreator := make(map[uuid.UUID]int, 8)
	for rows.Next() {
		resp := Response{OrderID: orderID}
		if err := rows.Scan(&resp.CreatorUserID, &resp.CreatorName, &resp.AvatarURL,
			&resp.IsPreferred, &resp.InCrew, &resp.Mode, &resp.FileURL,
			&resp.Note, &resp.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan response: %w", err)
		}
		byCreator[resp.CreatorUserID] = len(out)
		out = append(out, resp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Ролики отклика — вторым запросом, а не джойном: джойн размножил
	// бы строку отклика по числу роликов, и «согласных» стало бы втрое
	// больше, чем людей.
	itemRows, err := r.db.Query(ctx, `
SELECT ri.creator_user_id, p.id, COALESCE(p.title, ''),
       COALESCE(p.video_url, ''), COALESCE(p.preview_url, ''),
       COALESCE(p.thumbnail_url, '')
FROM order_response_portfolio_items ri
JOIN portfolio_items p ON p.id = ri.item_id
WHERE ri.order_id = $1
  AND ($2::uuid IS NULL OR ri.creator_user_id = $2)
ORDER BY p.created_at`, orderID, only)
	if err != nil {
		return nil, fmt.Errorf("list response items: %w", err)
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var (
			creator uuid.UUID
			it      ResponseItem
		)
		if err := itemRows.Scan(&creator, &it.ID, &it.Title,
			&it.VideoURL, &it.PreviewURL, &it.ThumbnailURL); err != nil {
			return nil, fmt.Errorf("scan response item: %w", err)
		}
		if i, ok := byCreator[creator]; ok {
			out[i].Items = append(out[i].Items, it)
		}
	}
	return out, itemRows.Err()
}
