package clarify

import (
	"fmt"
	"strings"
)

// CategoryRef — минимальное описание категории для промпта. Совместимо
// с `catalog.Category` по полям Code/Title/Description; чтобы не тащить
// зависимость от catalog в clarify, конкретный тип переезжает через
// тонкий адаптер на стороне main (см. CategoryLister).
type CategoryRef struct {
	Code        string
	Title       string
	Description string
}

const promptHeader = `Ты — ассистент маркетплейса marketpclce, который помогает заказчику сформулировать задачу до запуска поиска по каталогу специалистов в сфере видео и контент-продакшна.

КАТЕГОРИИ
`

const promptTail = `
НАВЫКИ (slug)
premiere, after-effects, davinci, final-cut, capcut, photoshop, figma,
reels, tiktok, youtube, shorts, vk-clips, telegram

ЗАДАЧА
Веди короткий деловой диалог (максимум 1-2 уточнения, лучше одно). Цель — собрать достаточно деталей, чтобы запустить поиск:
- категория (если ещё не выбрана)
- платформа/формат (Reels/TikTok/YouTube/Shorts/Telegram/VK)
- бюджет (rate_min, rate_max в RUB)
- город (только если требуется оффлайн-съёмка)
- ключевые скиллы (slug из списка выше)
- ниша / тематика (попадает в q)

ПРАВИЛА
1. Не перегружай вопросами. Если в первом сообщении пользователя уже есть платформа/формат/бюджет, не переспрашивай — сразу done=true.
2. Не выдумывай навыки и категории — только из списков выше. Если пользователь упомянул что-то иное, оставь это в q как свободный текст.
3. message всегда на русском, в дружелюбном деловом тоне, без воды и клише.
4. Когда done=true: message — короткое подтверждение («Понял, ищу…»), а search заполнен полями q/categories/skills/city. RateMin/RateMax только если пользователь явно назвал бюджет.
5. Если пользователь явно говорит «найди уже» / «хватит вопросов» — сразу done=true с тем, что есть.
6. Если запрос вообще не про видео/контент-продакшн — done=true, message объясняет что подбор будет приблизительным.

ФОРМАТ
Возвращай строго JSON по схеме. Если done=false — search можно опустить или оставить пустым. Если done=true — search обязательно заполнен.`

// fallbackCategories — статический список, используется если БД-источник
// категорий недоступен (lister == nil или вернул ошибку). Промпт остаётся
// рабочим, но без свежих кодов из catalog.
var fallbackCategories = []CategoryRef{
	{"editor", "Монтажёр", "видеомонтаж, нарезка, цветокор"},
	{"video_director", "Видеоредактор / режиссёр монтажа", "концепция и сторителлинг"},
	{"motion", "Моушн-дизайнер", "After Effects, анимация"},
	{"scriptwriter", "Сценарист", ""},
	{"smm", "СММ-специалист", "контент-планы, ведение соцсетей"},
	{"ugc", "UGC-контент", "ролики от первого лица для брендов"},
	{"blogger", "Блогер", "своя аудитория, интеграции"},
	{"ads_seo", "Таргет + SEO", ""},
	{"seeding", "Посевы", ""},
}

// buildSystemPrompt собирает system-prompt из живого списка категорий.
// Если cats пустой — берёт fallbackCategories. Если задан category — добавляет
// в конец секцию ТЕКУЩИЙ КОНТЕКСТ с зафиксированным кодом.
func buildSystemPrompt(cats []CategoryRef, category string) string {
	if len(cats) == 0 {
		cats = fallbackCategories
	}
	var b strings.Builder
	b.WriteString(promptHeader)
	for _, c := range cats {
		if c.Description != "" {
			fmt.Fprintf(&b, "- %s — %s (%s)\n", c.Code, c.Title, c.Description)
		} else {
			fmt.Fprintf(&b, "- %s — %s\n", c.Code, c.Title)
		}
	}
	b.WriteString(promptTail)
	if category != "" {
		b.WriteString("\n\nТЕКУЩИЙ КОНТЕКСТ\nПользователь уже выбрал категорию `")
		b.WriteString(category)
		b.WriteString("`. Категорию переспрашивать не надо — она уже зафиксирована и должна попасть в search.categories.")
	}
	return b.String()
}

func responseSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"message", "done"},
		"properties": map[string]any{
			"message": map[string]any{"type": "string"},
			"done":    map[string]any{"type": "boolean"},
			"search": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"q":          map[string]any{"type": "string"},
					"categories": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"skills":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"city":       map[string]any{"type": "string"},
					"rate_min":   map[string]any{"type": []string{"integer", "null"}},
					"rate_max":   map[string]any{"type": []string{"integer", "null"}},
				},
			},
		},
	}
}
