package publications

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// «Это ваш ролик?»
//
// Площадки видят ролик раньше нас: он вышел, набирает просмотры, а в
// сервисе его нет — потому что человек забыл вставить адрес. Именно на
// этом теряются площадки, и именно поэтому сбор находит «четыре из
// пяти» там, где выложено пять.
//
// Находка живёт отдельно от выкладки и ничего сама не меняет: привязку
// подтверждает человек. Сервис, привязывающий ролики сам, при первой же
// ошибке припишет креатору чужую работу — и узнает об этом из счёта.

// SuggestionStatus — что с находкой.
const (
	SuggestionPending   = "pending"
	SuggestionLinked    = "linked"
	SuggestionDismissed = "dismissed"
)

// suggestionWindowDays — насколько далеко от даты выхода ищем выкладку,
// к которой предложить привязку. Неделя в обе стороны: ролик снимают
// заранее и выкладывают с запасом, но «привязать к сроку через месяц» —
// это уже не подсказка, а угадывание.
const suggestionWindowDays = 7

var (
	// ErrSuggestionDecided — по находке уже ответили. Повторное решение
	// не ошибка ввода, а гонка двух вкладок, и отвечать на неё надо
	// текущим состоянием, а не молчанием.
	ErrSuggestionDecided = errors.New("suggestion already decided")
	// ErrSuggestionPlatformMismatch — выкладка и находка с разных
	// площадок: привязывать TikTok в слот Reels нельзя.
	ErrSuggestionPlatformMismatch = errors.New("suggestion platform mismatch")
)

// LinkSuggestion — находка, показанная креатору.
type LinkSuggestion struct {
	ID            uuid.UUID  `json:"id"`
	ProjectID     uuid.UUID  `json:"project_id"`
	ProjectTitle  string     `json:"project_title"`
	CreatorUserID uuid.UUID  `json:"creator_user_id"`
	Platform      string     `json:"platform"`
	URL           string     `json:"url"`
	Title         string     `json:"title,omitempty"`
	AuthorHandle  string     `json:"author_handle,omitempty"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// Suggested* — выкладка, к которой предлагаем привязать. Считается
	// при чтении, а не хранится: срок переносят, выкладку закрывают, и
	// прибитая к находке ссылка протухла бы на первом же переносе.
	// Пусто — подходящей выкладки сейчас нет, и кнопка «Привязать»
	// показывать некуда.
	SuggestedPublicationID *uuid.UUID `json:"suggested_publication_id,omitempty"`
	SuggestedDueDate       *time.Time `json:"suggested_due_date,omitempty"`
}

// AddSuggestionInput — что приносит обход аккаунта.
type AddSuggestionInput struct {
	ProjectID     uuid.UUID
	CreatorUserID uuid.UUID
	URL           string
	Title         string
	AuthorHandle  string
	PublishedAt   *time.Time
}

// AddSuggestion — положить находку.
//
// Повторная находка того же ролика ничего не меняет: обход аккаунта
// идёт каждый день, и без этого правила карточка «это ваш ролик?»
// размножалась бы сама. Отвергнутое («не мой») тоже не возвращается —
// отказ хранится строкой ровно за этим.
func (s *Service) AddSuggestion(ctx context.Context, in AddSuggestionInput) (LinkSuggestion, error) {
	in.Title = strings.TrimSpace(in.Title)
	if utf8.RuneCountInString(in.Title) > 200 {
		return LinkSuggestion{}, fmt.Errorf("%w: название ролика длиннее 200 символов", ErrInvalidInput)
	}
	in.AuthorHandle = strings.TrimSpace(in.AuthorHandle)
	link, err := ParseLink(in.URL)
	if err != nil {
		return LinkSuggestion{}, fmt.Errorf("%q: %w", in.URL, err)
	}
	return s.repo.AddSuggestion(ctx, in, link)
}

// AddSuggestion — запись находки. ON CONFLICT DO NOTHING и повторное
// чтение: вернуть надо ту строку, что лежит, а не тишину.
func (r *Repo) AddSuggestion(ctx context.Context, in AddSuggestionInput, link Link) (LinkSuggestion, error) {
	var id uuid.UUID
	err := r.db.QueryRow(ctx, `
WITH ins AS (
    INSERT INTO publication_link_suggestions
      (project_id, creator_user_id, platform, url, url_canonical,
       title, author_handle, published_at)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
    ON CONFLICT (project_id, url_canonical) DO NOTHING
    RETURNING id
)
SELECT id FROM ins
UNION ALL
SELECT id FROM publication_link_suggestions
WHERE project_id = $1 AND url_canonical = $5
LIMIT 1`,
		in.ProjectID, in.CreatorUserID, link.Platform, link.Raw, link.Canonical,
		in.Title, in.AuthorHandle, in.PublishedAt).Scan(&id)
	if err != nil {
		return LinkSuggestion{}, fmt.Errorf("add link suggestion: %w", err)
	}
	return r.suggestionByID(ctx, id)
}

// suggestionsSelect — общая выборка. Выкладка-кандидат подбирается
// LATERAL'ом: ближайшая по дате незакрытая выкладка ЭТОГО креатора в
// ЭТОМ проекте, у которой площадка находки ещё не сдана.
//
// В дне выкладок бывает несколько, поэтому номер в дне дописан в
// ORDER BY: без него из двух равноудалённых роликов одного дня
// кандидатом становился случайный, и одна и та же находка показывала
// бы человеку то первый ролик, то второй.
var suggestionsSelect = `
SELECT s.id, s.project_id, COALESCE(pr.title, ''), s.creator_user_id,
       s.platform, s.url, s.title, s.author_handle, s.published_at, s.created_at,
       cand.id, cand.due_date
FROM publication_link_suggestions s
JOIN projects pr ON pr.id = s.project_id
LEFT JOIN LATERAL (
    SELECT p.id, p.due_date
    FROM project_publications p
    WHERE p.project_id = s.project_id
      AND p.creator_user_id = s.creator_user_id
      AND p.status IN ('planned', 'partial')
      AND ABS(p.due_date - COALESCE(s.published_at::date, CURRENT_DATE)) <=
          ` + strconv.Itoa(suggestionWindowDays) + `
      AND NOT EXISTS (
          SELECT 1 FROM publication_links l
          WHERE l.publication_id = p.id AND l.platform = s.platform
      )
    ORDER BY ABS(p.due_date - COALESCE(s.published_at::date, CURRENT_DATE)), p.due_date, p.day_slot
    LIMIT 1
) cand ON TRUE`

func scanSuggestion(row pgx.Row) (LinkSuggestion, error) {
	var s LinkSuggestion
	err := row.Scan(&s.ID, &s.ProjectID, &s.ProjectTitle, &s.CreatorUserID,
		&s.Platform, &s.URL, &s.Title, &s.AuthorHandle, &s.PublishedAt, &s.CreatedAt,
		&s.SuggestedPublicationID, &s.SuggestedDueDate)
	return s, err
}

func (r *Repo) suggestionByID(ctx context.Context, id uuid.UUID) (LinkSuggestion, error) {
	s, err := scanSuggestion(r.db.QueryRow(ctx, suggestionsSelect+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkSuggestion{}, ErrNotFound
	}
	if err != nil {
		return LinkSuggestion{}, fmt.Errorf("get link suggestion: %w", err)
	}
	return s, nil
}

// CreatorSuggestions — что ждёт ответа у этого креатора.
//
// По всем его проектам сразу: находка приходит от площадки, а не от
// проекта, и заставлять человека обходить шесть вкладок в поисках
// карточки «это ваш ролик?» — значит не показать её вовсе.
func (r *Repo) CreatorSuggestions(ctx context.Context, creatorID uuid.UUID) ([]LinkSuggestion, error) {
	rows, err := r.db.Query(ctx, suggestionsSelect+`
WHERE s.creator_user_id = $1 AND s.status = 'pending'
  AND pr.status <> 'cancelled'
ORDER BY s.created_at DESC`, creatorID)
	if err != nil {
		return nil, fmt.Errorf("list link suggestions: %w", err)
	}
	defer rows.Close()
	out := make([]LinkSuggestion, 0)
	for rows.Next() {
		s, err := scanSuggestion(rows)
		if err != nil {
			return nil, fmt.Errorf("scan link suggestion: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// decideSuggestion — перевести находку в конечное состояние.
//
// Условие status = 'pending' стоит в самом UPDATE: две вкладки, открытые
// на одной карточке, иначе привяжут ролик дважды.
func (r *Repo) decideSuggestion(ctx context.Context, id, actor uuid.UUID, status string) error {
	tag, err := r.db.Exec(ctx, `
UPDATE publication_link_suggestions
SET status = $3, decided_at = now(), decided_by = $2
WHERE id = $1 AND status = 'pending'`, id, actor, status)
	if err != nil {
		return fmt.Errorf("decide link suggestion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSuggestionDecided
	}
	return nil
}

// CreatorSuggestions — список для кабинета креатора.
func (s *Service) CreatorSuggestions(ctx context.Context, creatorID uuid.UUID) ([]LinkSuggestion, error) {
	return s.repo.CreatorSuggestions(ctx, creatorID)
}

// DismissSuggestion — «не мой».
//
// Отказ хранится, а не удаляется: обход аккаунта идёт каждый день, и
// удалённая находка вернулась бы завтра той же карточкой.
func (s *Service) DismissSuggestion(ctx context.Context, id, creatorID uuid.UUID) error {
	found, err := s.repo.suggestionByID(ctx, id)
	if err != nil {
		return err
	}
	// Чужая находка — «не найдено», а не «нельзя»: 403 подтвердил бы
	// постороннему, что такая карточка существует.
	if found.CreatorUserID != creatorID {
		return ErrNotFound
	}
	return s.repo.decideSuggestion(ctx, id, creatorID, SuggestionDismissed)
}

// LinkSuggestion — «да, мой»: находка становится сданной ссылкой.
//
// Идёт тем же путём, что и ссылка, вставленная руками (SubmitLinks): те
// же проверки чеклиста, то же закрытие выкладки при пятой площадке, то
// же событие. Иначе у привязанного ролика была бы вторая, более
// снисходительная дорога в систему.
func (s *Service) LinkSuggestion(ctx context.Context, id, creatorID, publicationID uuid.UUID,
	checked []uuid.UUID) (Publication, error) {

	found, err := s.repo.suggestionByID(ctx, id)
	if err != nil {
		return Publication{}, err
	}
	if found.CreatorUserID != creatorID {
		return Publication{}, ErrNotFound
	}
	if publicationID == uuid.Nil {
		if found.SuggestedPublicationID == nil {
			return Publication{}, fmt.Errorf(
				"%w: к какой выкладке привязать — подходящей нет, укажите её явно", ErrInvalidInput)
		}
		publicationID = *found.SuggestedPublicationID
	}
	pub, err := s.SubmitLinks(ctx, SubmitLinksInput{
		PublicationID:  publicationID,
		ActorUserID:    creatorID,
		URLs:           []string{found.URL},
		CheckedItemIDs: checked,
	})
	if err != nil {
		return Publication{}, err
	}
	// Ссылка уже сдана — карточку закрываем. Если на этом шаге что-то
	// пойдёт не так, хуже всего, что случится: карточка останется
	// висеть, и человек нажмёт «не мой». Ролик при этом на месте.
	if err := s.repo.decideSuggestion(ctx, id, creatorID, SuggestionLinked); err != nil &&
		!errors.Is(err, ErrSuggestionDecided) {
		return pub, err
	}
	return pub, nil
}
