package publications

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Материалы проекта: бренд-гайд, обучение, «как разложить ролик на пять
// площадок». По требованиям открываются креатору в момент добавления в
// проект, и клиент их не видит — поэтому у материала есть аудитория, а не
// только проект.

// Аудитории материала.
const (
	// AudienceCreators — для состава проекта. Клиенту не показывается.
	AudienceCreators = "creators"
	// AudienceClient — для заказчика. Так помечено то, что приложено к
	// сдаче в общем проекте: это и есть результат работы.
	AudienceClient = "client"
)

// Виды материала. Совпадают с CHECK в 00032 и 00071.
const (
	MaterialDoc   = "doc"
	MaterialVideo = "video"
	MaterialLink  = "link"
	// MaterialContract — договор с креатором.
	//
	// Отдельный вид, а не документ с названием «Договор»: он уходит
	// человеку вместе с добавлением в проект, отдаётся первым в списке
	// и выделяется в кабинете плашкой. Искать его подстрокой в
	// названии — значит однажды не найти, а «первый по порядку»
	// ломается на второй же перестановке материалов.
	MaterialContract = "contract"
)

// DocumentKinds — виды, которые попадают в «Мои документы» креатора.
//
// Договор и только он. Вид «документ» (doc) — материал для съёмки:
// бренд-гайд, сценарий, обучение. Список, а не одно значение: вид у
// договора не последний, и когда появится ТЗ или акт, правка будет
// здесь, а не в запросе и четырёх шаблонах.
var DocumentKinds = []string{MaterialContract}

const (
	materialTitleMax = 200
	materialURLMax   = 2000
	// materialsPerProject — предел на проект. Не про технику: список,
	// который не пролистать, перестаёт быть списком материалов.
	materialsPerProject = 100
)

// ErrTooManyMaterials — в проекте уже некуда добавлять.
var ErrTooManyMaterials = errors.New("too many materials in project")

// Material — файл, видео или ссылка, приложенные к проекту.
type Material struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	// Audience — creators или client. Определяет, кто материал увидит.
	Audience  string     `json:"audience"`
	SortOrder int        `json:"sort_order"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// AddMaterialInput — что добавляет менеджер.
type AddMaterialInput struct {
	ProjectID uuid.UUID
	Kind      string
	Title     string
	URL       string
	Audience  string
	CreatedBy uuid.UUID
}

// ListMaterials — материалы проекта для указанной аудитории. Пустая
// аудитория — все: так их видит менеджер.
func (r *Repo) ListMaterials(ctx context.Context, projectID uuid.UUID, audience string) ([]Material, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, project_id, kind, title, url, audience, sort_order, created_by, created_at
FROM project_materials
WHERE project_id = $1
  AND ($2 = '' OR audience = $2)
  -- Материалы конкретной сдачи живут в карточке сдачи, а не в общем
  -- списке материалов проекта.
  AND delivery_id IS NULL
  AND deleted_at IS NULL
-- Договор — первым, каким бы ни был его порядковый номер. Это первое,
-- что человек ищет, когда его добавили в проект, и последнее, что он
-- хочет искать глазами в списке из тридцати референсов.
ORDER BY (kind <> 'contract'), sort_order, created_at`, projectID, audience)
	if err != nil {
		return nil, fmt.Errorf("list materials: %w", err)
	}
	defer rows.Close()
	out := make([]Material, 0)
	for rows.Next() {
		var m Material
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Kind, &m.Title, &m.URL,
			&m.Audience, &m.SortOrder, &m.CreatedBy, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan material: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMaterial — добавить материал в конец списка.
func (r *Repo) AddMaterial(ctx context.Context, in AddMaterialInput) (Material, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Material{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Порядковый номер считаем в транзакции: два одновременных добавления
	// иначе получат один и тот же sort_order и встанут в списке как
	// придётся.
	var next, total int
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(sort_order), -1) + 1, COUNT(*) FILTER (WHERE deleted_at IS NULL)
FROM project_materials WHERE project_id = $1 AND delivery_id IS NULL`,
		in.ProjectID).Scan(&next, &total); err != nil {
		return Material{}, fmt.Errorf("next sort order: %w", err)
	}
	if total >= materialsPerProject {
		return Material{}, ErrTooManyMaterials
	}

	m := Material{
		ProjectID: in.ProjectID, Kind: in.Kind, Title: in.Title,
		URL: in.URL, Audience: in.Audience, SortOrder: next,
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO project_materials (project_id, kind, title, url, audience, sort_order, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, created_by, created_at`,
		in.ProjectID, in.Kind, in.Title, in.URL, in.Audience, next, in.CreatedBy).
		Scan(&m.ID, &m.CreatedBy, &m.CreatedAt); err != nil {
		return Material{}, fmt.Errorf("insert material: %w", err)
	}
	// Материал креатору — это изменение его задания, и узнать о нём он
	// должен от нас, а не открыв проект по своей воле.
	//
	// Сравниваем с КОНСТАНТОЙ, а не со строкой: здесь стояло
	// `== "creator" || == "all"`, а legal-значения — `creators` и
	// `client` (CHECK в 00036). Условие не выполнялось никогда, и
	// уведомление «задание изменилось» не уходило ни разу за всё время
	// жизни материалов. Молча: код есть, тест на отправку был, но
	// вызывался он с той же выдуманной строкой.
	if in.Audience == AudienceCreators {
		if err := notifyBrief(ctx, tx, in.ProjectID,
			ReminderMaterialsUpdated, nil, time.Now()); err != nil {
			return Material{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Material{}, fmt.Errorf("commit: %w", err)
	}
	return m, nil
}

// DeleteMaterial — убрать материал. Возвращает ErrNotFound, если он не из
// этого проекта: id материала сам по себе не даёт права его удалить.
//
// Мягко: строка остаётся с отметкой, кто и когда убрал. Раньше удаление
// стирало материал бесследно, и про убранный договор потом нельзя было
// узнать, что он вообще был.
func (r *Repo) DeleteMaterial(ctx context.Context, projectID, materialID, actor uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Кому он был виден — нужно знать ДО удаления: после него спросить
	// уже не у кого, а убранный у креатора материал меняет его задание
	// ровно так же, как добавленный.
	var audience string
	err = tx.QueryRow(ctx, `
UPDATE project_materials SET deleted_at = now(), deleted_by = $3
WHERE id = $1 AND project_id = $2 AND delivery_id IS NULL AND deleted_at IS NULL
RETURNING audience`, materialID, projectID, actor).Scan(&audience)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete material: %w", err)
	}
	// Та же выдуманная строка, что и при добавлении, — и тот же
	// результат: убранный у креатора материал молчал.
	if audience == AudienceCreators {
		if err := notifyBrief(ctx, tx, projectID,
			ReminderMaterialsUpdated, nil, time.Now()); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ---- сервис ----

// ListMaterials — материалы проекта. audience пустая = все (менеджер).
func (s *Service) ListMaterials(ctx context.Context, projectID uuid.UUID, audience string) ([]Material, error) {
	return s.repo.ListMaterials(ctx, projectID, audience)
}

// AddMaterial — проверить и добавить.
func (s *Service) AddMaterial(ctx context.Context, in AddMaterialInput) (Material, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.URL = strings.TrimSpace(in.URL)
	in.Kind = strings.TrimSpace(in.Kind)

	switch in.Kind {
	case MaterialDoc, MaterialVideo, MaterialLink, MaterialContract:
	default:
		return Material{}, fmt.Errorf("%w: kind must be doc, video, link or contract",
			ErrInvalidInput)
	}
	switch in.Audience {
	case "":
		in.Audience = AudienceCreators
	case AudienceCreators, AudienceClient:
	default:
		return Material{}, fmt.Errorf("%w: audience must be creators or client", ErrInvalidInput)
	}
	if in.Title == "" || len([]rune(in.Title)) > materialTitleMax {
		return Material{}, fmt.Errorf("%w: title must be 1-%d chars", ErrInvalidInput, materialTitleMax)
	}
	if len(in.URL) > materialURLMax {
		return Material{}, fmt.Errorf("%w: url is too long", ErrInvalidInput)
	}
	if !isHTTPLink(in.URL) {
		return Material{}, fmt.Errorf("%w: url must be an http(s) link", ErrInvalidInput)
	}
	return s.repo.AddMaterial(ctx, in)
}

// DeleteMaterial — убрать материал из проекта.
func (s *Service) DeleteMaterial(ctx context.Context, projectID, materialID, actor uuid.UUID) error {
	return s.repo.DeleteMaterial(ctx, projectID, materialID, actor)
}

// ---- мои документы (креатор) ----

// isHTTPLink — ссылка, которую можно отдать в интерфейс кликабельной:
// только http(s) и с хостом. javascript: в ссылке — исполняемый код у
// читателя в браузере. Одна проверка для материалов и документов.
func isHTTPLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
