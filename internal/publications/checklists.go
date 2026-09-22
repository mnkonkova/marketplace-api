package publications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"marketpclce/internal/audit"
)

// Библиотека чек-листов. Снимок в проект уже умели делать (SnapshotChecklist),
// а вот посмотреть, из чего выбирать и что в проект уже подключено, — нет.

// ChecklistTemplate — шаблон из библиотеки.
type ChecklistTemplate struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Version     int       `json:"version"`
	ItemsCount  int       `json:"items_count"`
	// ProjectsCount — на скольких проектах эта версия подключена.
	// Отвечает на вопрос, который возникает перед правкой шаблона:
	// новая версия не тронет уже подключённые снимки, и знать, сколько
	// проектов останутся на старых пунктах, нужно ДО выпуска.
	ProjectsCount int `json:"projects_count"`
}

// ChecklistTemplates — действующая библиотека. Выключенные шаблоны не
// показываем: подключать их всё равно нельзя.
func (r *Repo) ChecklistTemplates(ctx context.Context) ([]ChecklistTemplate, error) {
	// Подключения считаем подзапросом, а не вторым LEFT JOIN: два join'а
	// к одной группировке перемножили бы строки, и число пунктов уехало
	// бы вслед за числом проектов.
	rows, err := r.db.Query(ctx, `
SELECT t.id, t.name, t.description, t.version, COUNT(i.id),
       (SELECT COUNT(*) FROM project_checklist_snapshot s WHERE s.template_id = t.id)
FROM checklist_templates t
LEFT JOIN checklist_template_items i ON i.template_id = t.id
WHERE t.is_active
GROUP BY t.id, t.name, t.description, t.version
ORDER BY t.name, t.version DESC`)
	if err != nil {
		return nil, fmt.Errorf("list checklist templates: %w", err)
	}
	defer rows.Close()
	out := make([]ChecklistTemplate, 0)
	for rows.Next() {
		var t ChecklistTemplate
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Version, &t.ItemsCount,
			&t.ProjectsCount); err != nil {
			return nil, fmt.Errorf("scan checklist template: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) ChecklistTemplates(ctx context.Context) ([]ChecklistTemplate, error) {
	return s.repo.ChecklistTemplates(ctx)
}

// ---- библиотека под админом ----
//
// Шаблон правится ТОЛЬКО целиком и только новой версией: в проекты он
// уходит снимком, и подменять пункты под уже идущими проектами нельзя —
// креатор отмечал одно, а спросят с него другое. Поэтому «сохранить» —
// это выпустить следующую версию и погасить прежнюю, а не UPDATE.

// ChecklistTemplateItem — пункт шаблона. platform пустой = общий пункт.
type ChecklistTemplateItem struct {
	Text string `json:"text"`
	// Platform — площадка пункта; пусто означает «для всех».
	Platform   string `json:"platform,omitempty"`
	IsRequired bool   `json:"is_required"`
}

// ChecklistTemplateFull — шаблон вместе с пунктами.
type ChecklistTemplateFull struct {
	ChecklistTemplate
	Items []ChecklistTemplateItem `json:"items"`
}

// ChecklistTemplateWithItems — один шаблон целиком.
func (r *Repo) ChecklistTemplateWithItems(ctx context.Context, id uuid.UUID) (ChecklistTemplateFull, error) {
	var out ChecklistTemplateFull
	err := r.db.QueryRow(ctx, `
SELECT t.id, t.name, t.description, t.version,
       (SELECT COUNT(*) FROM project_checklist_snapshot s WHERE s.template_id = t.id)
FROM checklist_templates t WHERE t.id = $1`, id).
		Scan(&out.ID, &out.Name, &out.Description, &out.Version, &out.ProjectsCount)
	if err != nil {
		return ChecklistTemplateFull{}, ErrNotFound
	}
	rows, err := r.db.Query(ctx, `
SELECT text, COALESCE(platform, ''), is_required
FROM checklist_template_items WHERE template_id = $1 ORDER BY sort_order`, id)
	if err != nil {
		return ChecklistTemplateFull{}, fmt.Errorf("template items: %w", err)
	}
	defer rows.Close()
	out.Items = make([]ChecklistTemplateItem, 0)
	for rows.Next() {
		var it ChecklistTemplateItem
		if err := rows.Scan(&it.Text, &it.Platform, &it.IsRequired); err != nil {
			return ChecklistTemplateFull{}, fmt.Errorf("scan template item: %w", err)
		}
		out.Items = append(out.Items, it)
	}
	out.ItemsCount = len(out.Items)
	return out, rows.Err()
}

// SaveChecklistTemplate — выпустить шаблон новой версией.
//
// replaces пустой — это новый шаблон с версии 1. Иначе прежняя версия
// гасится (is_active = FALSE) в той же транзакции: уникальный индекс по
// имени среди активных иначе не пустил бы вторую.
func (r *Repo) SaveChecklistTemplate(
	ctx context.Context, actorID, replaces uuid.UUID, name, description string, items []ChecklistTemplateItem,
) (ChecklistTemplateFull, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return ChecklistTemplateFull{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	version := 1
	if replaces != uuid.Nil {
		if err := tx.QueryRow(ctx,
			`UPDATE checklist_templates SET is_active = FALSE, updated_at = now()
             WHERE id = $1 AND is_active RETURNING version + 1`, replaces).Scan(&version); err != nil {
			return ChecklistTemplateFull{}, ErrNotFound
		}
	}

	var out ChecklistTemplateFull
	if err := tx.QueryRow(ctx, `
INSERT INTO checklist_templates (name, description, version)
VALUES ($1, $2, $3) RETURNING id, name, description, version`,
		name, description, version).
		Scan(&out.ID, &out.Name, &out.Description, &out.Version); err != nil {
		return ChecklistTemplateFull{}, fmt.Errorf("insert template: %w", err)
	}

	for i, it := range items {
		var platform *string
		if it.Platform != "" {
			p := it.Platform
			platform = &p
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO checklist_template_items (template_id, text, platform, is_required, sort_order)
VALUES ($1, $2, $3, $4, $5)`, out.ID, it.Text, platform, it.IsRequired, i); err != nil {
			return ChecklistTemplateFull{}, fmt.Errorf("insert template item: %w", err)
		}
	}
	payload := map[string]any{"name": out.Name, "version": out.Version, "items": len(items)}
	if replaces != uuid.Nil {
		payload["replaces"] = replaces.String()
	}
	if err := audit.Write(ctx, tx, actorID, audit.ActionChecklistPublish,
		audit.ObjectChecklist, out.ID.String(), payload); err != nil {
		return ChecklistTemplateFull{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ChecklistTemplateFull{}, fmt.Errorf("commit: %w", err)
	}
	out.Items = items
	out.ItemsCount = len(items)
	return out, nil
}

// DeactivateChecklistTemplate — убрать шаблон из библиотеки. Проекты, где
// он уже подключён, не трогаем: у них свой снимок.
func (r *Repo) DeactivateChecklistTemplate(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE checklist_templates SET is_active = FALSE, updated_at = now()
         WHERE id = $1 AND is_active`, id)
	if err != nil {
		return fmt.Errorf("deactivate template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) ChecklistTemplate(ctx context.Context, id uuid.UUID) (ChecklistTemplateFull, error) {
	return s.repo.ChecklistTemplateWithItems(ctx, id)
}

// SaveChecklistTemplate — проверить и выпустить версию шаблона.
func (s *Service) SaveChecklistTemplate(
	ctx context.Context, actorID, replaces uuid.UUID, name, description string, items []ChecklistTemplateItem,
) (ChecklistTemplateFull, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ChecklistTemplateFull{}, fmt.Errorf("%w: у шаблона должно быть название", ErrInvalidInput)
	}
	if utf8.RuneCountInString(name) > 120 {
		return ChecklistTemplateFull{}, fmt.Errorf("%w: название слишком длинное", ErrInvalidInput)
	}
	clean := make([]ChecklistTemplateItem, 0, len(items))
	for _, it := range items {
		it.Text = strings.TrimSpace(it.Text)
		if it.Text == "" {
			continue
		}
		if utf8.RuneCountInString(it.Text) > 300 {
			return ChecklistTemplateFull{}, fmt.Errorf("%w: пункт слишком длинный", ErrInvalidInput)
		}
		if it.Platform != "" && !IsKnownPlatform(it.Platform) {
			return ChecklistTemplateFull{}, fmt.Errorf(
				"%w: площадка пункта не из пяти известных: %s", ErrInvalidInput, it.Platform)
		}
		clean = append(clean, it)
	}
	if len(clean) == 0 {
		return ChecklistTemplateFull{}, fmt.Errorf(
			"%w: шаблон без пунктов ничего не проверяет", ErrInvalidInput)
	}
	if len(clean) > 60 {
		return ChecklistTemplateFull{}, fmt.Errorf(
			"%w: шестьдесят пунктов креатор не отметит — это не чеклист", ErrInvalidInput)
	}
	return s.repo.SaveChecklistTemplate(ctx, actorID, replaces, name, strings.TrimSpace(description), clean)
}

func (s *Service) DeactivateChecklistTemplate(ctx context.Context, id uuid.UUID) error {
	return s.repo.DeactivateChecklistTemplate(ctx, id)
}

// ---- подключение чек-листа новому проекту ----
//
// «Действующий чек-лист» определён однозначно ровно тогда, когда
// действующий шаблон ОДИН. Уникальный индекс в базе держит одно активное
// имя, но имён может быть несколько — «UGC для маркетплейса» и
// «Продакшн», — и какой из них подставить новому проекту, не знает
// никто, кроме человека.
//
// Поэтому при неоднозначности НЕ ВЫБИРАЕМ. Пустой чек-лист менеджер
// увидит и подключит нужный сам; подставленный наугад он не увидит — и
// креатор получит требования от чужого проекта.

// ActiveChecklistTemplateID — единственный действующий шаблон библиотеки.
// uuid.Nil, когда шаблонов нет или их больше одного.
func (r *Repo) ActiveChecklistTemplateID(ctx context.Context) (uuid.UUID, error) {
	// LIMIT 2, а не 1: по одной строке не отличить «единственный» от
	// «первый попавшийся», а разница здесь и есть всё правило.
	rows, err := r.db.Query(ctx, `
SELECT id FROM checklist_templates
WHERE is_active
  AND EXISTS (SELECT 1 FROM checklist_template_items i WHERE i.template_id = checklist_templates.id)
LIMIT 2`)
	if err != nil {
		return uuid.Nil, fmt.Errorf("active checklist template: %w", err)
	}
	defer rows.Close()
	var found []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return uuid.Nil, fmt.Errorf("scan active template: %w", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	if len(found) != 1 {
		return uuid.Nil, nil
	}
	return found[0], nil
}

// AttachActiveChecklist — подключить новому проекту действующий чек-лист.
// Реализует projects.ChecklistAttacher.
//
// Возвращает число скопированных пунктов; ноль означает «подключать было
// нечего», и это не ошибка.
func (s *Service) AttachActiveChecklist(ctx context.Context, projectID, actor uuid.UUID) (int, error) {
	templateID, err := s.repo.ActiveChecklistTemplateID(ctx)
	if err != nil {
		return 0, err
	}
	if templateID == uuid.Nil {
		return 0, nil
	}
	return s.repo.SnapshotChecklist(ctx, projectID, templateID, actor)
}

// ---- пункт, заведённый под конкретный проект ----
//
// Снимок — копия проекта, и дополнять её можно: правило снимка о том,
// что ПРАВКА БИБЛИОТЕКИ не доезжает до идущих проектов, а не о том, что
// проект нельзя уточнить. «Шрифт титров — Onest Bold» касается одного
// бренда, и класть его в общую библиотеку неверно.
//
// Такой пункт отличается от скопированного пустым source_item_id — это
// поле и заводилось как «откуда взялось».

// AddChecklistItem — добавить пункт в чек-лист проекта.
func (r *Repo) AddChecklistItem(ctx context.Context, projectID uuid.UUID, text, platform string, required bool) (ChecklistItem, error) {
	var it ChecklistItem
	// Порядок — в конец списка. Вставлять в середину незачем: пункты
	// проверяют все до одного, а не по порядку важности.
	err := r.db.QueryRow(ctx, `
INSERT INTO project_checklist_items
    (project_id, source_item_id, text, platform, is_required, sort_order, added_for_project)
VALUES ($1, NULL, $2, NULLIF($3, ''), $4,
        COALESCE((SELECT MAX(sort_order) + 1 FROM project_checklist_items WHERE project_id = $1), 1),
        TRUE)
RETURNING id, project_id, text, platform, is_required, sort_order, added_for_project`,
		projectID, text, platform, required).
		Scan(&it.ID, &it.ProjectID, &it.Text, &it.Platform, &it.IsRequired, &it.SortOrder,
			&it.AddedForProject)
	if err != nil {
		return ChecklistItem{}, fmt.Errorf("add checklist item: %w", err)
	}
	return it, nil
}

// ErrChecklistItemUsed — по пункту уже отчитывались.
var ErrChecklistItemUsed = errors.New("checklist item already marked")

// DeleteChecklistItem — убрать пункт из чек-листа проекта.
//
// Пункт, по которому креатор уже отчитывался, не удаляем: отметки висят
// на нём внешним ключом с каскадом, и вместе с пунктом исчез бы след
// того, что человек это проверял. Спорить потом будет нечем.
func (r *Repo) DeleteChecklistItem(ctx context.Context, projectID, itemID uuid.UUID) error {
	// Считаем обе стороны: и отметку креатора «я сделал», и вердикт
	// менеджера «я проверил». Пункт, по которому кто-то из них уже
	// высказался, — это след разговора, а не строка в списке.
	var marks int
	if err := r.db.QueryRow(ctx, `
SELECT (SELECT COUNT(*) FROM publication_checklist_marks WHERE item_id = $1)
     + (SELECT COUNT(*) FROM publication_review_marks WHERE item_id = $1)`, itemID).
		Scan(&marks); err != nil {
		return fmt.Errorf("count checklist marks: %w", err)
	}
	if marks > 0 {
		return ErrChecklistItemUsed
	}
	tag, err := r.db.Exec(ctx,
		`DELETE FROM project_checklist_items WHERE id = $1 AND project_id = $2`, itemID, projectID)
	if err != nil {
		return fmt.Errorf("delete checklist item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) AddChecklistItem(ctx context.Context, projectID uuid.UUID, text, platform string, required bool) (ChecklistItem, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return ChecklistItem{}, fmt.Errorf("%w: текст пункта пустой", ErrInvalidInput)
	}
	if utf8.RuneCountInString(text) > 300 {
		return ChecklistItem{}, fmt.Errorf("%w: пункт длиннее 300 символов", ErrInvalidInput)
	}
	return s.repo.AddChecklistItem(ctx, projectID, text, platform, required)
}

func (s *Service) DeleteChecklistItem(ctx context.Context, projectID, itemID uuid.UUID) error {
	return s.repo.DeleteChecklistItem(ctx, projectID, itemID)
}
