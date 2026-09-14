package publications

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
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
}

// ChecklistTemplates — действующая библиотека. Выключенные шаблоны не
// показываем: подключать их всё равно нельзя.
func (r *Repo) ChecklistTemplates(ctx context.Context) ([]ChecklistTemplate, error) {
	rows, err := r.db.Query(ctx, `
SELECT t.id, t.name, t.description, t.version, COUNT(i.id)
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
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Version, &t.ItemsCount); err != nil {
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
SELECT id, name, description, version FROM checklist_templates WHERE id = $1`, id).
		Scan(&out.ID, &out.Name, &out.Description, &out.Version)
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
	ctx context.Context, replaces uuid.UUID, name, description string, items []ChecklistTemplateItem,
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
	ctx context.Context, replaces uuid.UUID, name, description string, items []ChecklistTemplateItem,
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
	return s.repo.SaveChecklistTemplate(ctx, replaces, name, strings.TrimSpace(description), clean)
}

func (s *Service) DeactivateChecklistTemplate(ctx context.Context, id uuid.UUID) error {
	return s.repo.DeactivateChecklistTemplate(ctx, id)
}
