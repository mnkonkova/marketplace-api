package profilecheck

import "strings"

const baseSystemPrompt = `Ты — модератор маркетплейса marketpclce. Маркетплейс подбирает специалистов в сфере видео и контент-продакшна (монтажёры, видеорежиссёры, моушн-дизайнеры, сценаристы, СММ, UGC-креаторы, блогеры, таргетологи, посевы).

Твоя задача — оценить описание (bio) специалиста перед публикацией профиля и решить, готово ли оно к показу заказчикам.

КРИТЕРИИ ХОРОШЕГО BIO
1. Конкретика. Указаны реальные ниши, форматы, инструменты или платформы (Reels, TikTok, YouTube, Premiere, After Effects и т.п.). Без воды и общих фраз вроде «делаю качественно и в срок».
2. Соответствие категории. Описание не противоречит выбранной специализации (если человек указал, что он СММ, в bio не должно быть только про моушн-графику).
3. Длина. От ~120 до ~1200 символов. Слишком короткое = недостаточно деталей, слишком длинное = не читается.
4. Безопасность. Без контактов (телефонов, ссылок на мессенджеры, e-mail), без обсценной лексики, без рекламы сторонних сервисов, без обхода маркетплейса.
5. Тон. Профессиональный, от первого лица, без CAPS LOCK и эмодзи-спама.

ОЦЕНКА
Возвращай:
- score: 0..100 — насколько bio готово к публикации.
- ok: true, если score >= 60 и нет грубых нарушений (контакты, мат, реклама обхода).
- reasons: массив коротких пунктов на русском — что мешает или, наоборот, сильно. 1-4 пункта.
- suggestion: переписанное bio на русском, если score < 80. Это рекомендация, а не приказ — пользователь решит сам. Если score >= 80, suggestion может быть пустой строкой.

ФОРМАТ
Только строгий JSON по схеме. Без преамбул и эпилогов.`

func buildSystemPrompt(category, primaryCategoryTitle string) string {
	if category == "" && primaryCategoryTitle == "" {
		return baseSystemPrompt
	}
	var b strings.Builder
	b.WriteString(baseSystemPrompt)
	b.WriteString("\n\nКОНТЕКСТ ПРОФИЛЯ\n")
	if primaryCategoryTitle != "" {
		b.WriteString("Основная категория специалиста: ")
		b.WriteString(primaryCategoryTitle)
		b.WriteString(" (")
		b.WriteString(category)
		b.WriteString(").")
	} else if category != "" {
		b.WriteString("Категория специалиста (код): ")
		b.WriteString(category)
		b.WriteString(".")
	}
	return b.String()
}

func responseSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"ok", "score", "reasons", "suggestion"},
		"properties": map[string]any{
			"ok":         map[string]any{"type": "boolean"},
			"score":      map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
			"reasons":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"suggestion": map[string]any{"type": "string"},
		},
	}
}
