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

// SkillRef — slug навыка для словаря в промпте. Title нужен LLM'у,
// чтобы корректно распознать русские названия инструментов в свободном
// тексте пользователя и отобразить на канонический slug.
type SkillRef struct {
	Slug  string
	Title string
}

const promptHeader = `Ты — ассистент маркетплейса marketpclce, который помогает заказчику сформулировать задачу до запуска поиска по каталогу специалистов в сфере видео и контент-продакшна.

КАТЕГОРИИ
`

const promptTailStart = `
ЗАДАЧА
Веди короткий деловой диалог (максимум 1-2 уточнения, лучше одно). Цель — собрать достаточно деталей, чтобы запустить поиск:
- категория (если ещё не выбрана)
- платформа/формат (Reels/TikTok/YouTube/Shorts/Telegram/VK)
- бюджет (rate_min, rate_max в RUB)
- город (только если требуется оффлайн-съёмка)
- ключевые скиллы (slug из списка выше)
- ниша / тематика (попадает в q)

ПРАВИЛА
1. Поиск по имени: если запрос выглядит как имя/ник конкретного человека (например «Борис Лавров», «Ваня», «mary_design», 1–3 слова с заглавной буквы, кириллица или латинский никнейм; нет упоминаний платформы, бюджета, навыков, категории, ниши) — НЕ задавай уточнений. Сразу done=true, message — короткое «Ищу по имени…», search.q = исходный текст пользователя без изменений, остальные поля search оставь пустыми (categories=[], skills=[], city="", без rate_min/rate_max). Совпадение с реальным профилем найдёт следующий этап поиска — твоё дело распознать, что это имя, а не задача.
2. Не перегружай вопросами. Если в первом сообщении пользователя уже есть платформа/формат/бюджет, не переспрашивай — сразу done=true.
3. Не выдумывай навыки и категории — только из списков выше. Если пользователь упомянул что-то иное, оставь это в q как свободный текст.
4. message всегда на русском, в дружелюбном деловом тоне, без воды и клише.
5. Когда done=true: message — короткое подтверждение («Понял, ищу…»), а search заполнен полями q/categories/skills/city. RateMin/RateMax только если пользователь явно назвал бюджет.
6. Если пользователь явно говорит «найди уже» / «хватит вопросов» — сразу done=true с тем, что есть.
7. Если запрос вообще не про видео/контент-продакшн — done=true, message объясняет что подбор будет приблизительным.

ФОРМАТ
Возвращай строго JSON по схеме. Если done=false — search можно опустить или оставить пустым. Если done=true — search обязательно заполнен.`

// fallbackSkills — статический словарь навыков, используется если БД-источник
// недоступен (lister == nil или вернул ошибку). Покрывает базу из миграций
// 00001 + 00009; если расширяешь skills в БД и хочешь, чтобы LLM понимал
// их даже при упавшей БД — добавь сюда же. Платформы тоже здесь, чтобы
// промпт без БД был полным.
var fallbackSkills = []SkillRef{
	// tools
	{"premiere", "Adobe Premiere Pro"}, {"after-effects", "Adobe After Effects"},
	{"davinci", "DaVinci Resolve"}, {"final-cut", "Final Cut Pro"},
	{"capcut", "CapCut"}, {"audition", "Adobe Audition"}, {"videoleap", "Videoleap"},
	{"cinema-4d", "Cinema 4D"},
	{"photoshop", "Adobe Photoshop"}, {"illustrator", "Adobe Illustrator"},
	{"indesign", "Adobe InDesign"}, {"coreldraw", "CorelDRAW"},
	{"figma", "Figma"}, {"adobe-xd", "Adobe XD"}, {"sketch", "Sketch"},
	{"yandex-direct", "Яндекс.Директ"}, {"google-ads", "Google Ads"},
	{"yandex-metrica", "Яндекс.Метрика"}, {"google-analytics", "Google Analytics"},
	{"midjourney", "Midjourney"}, {"stable-diffusion", "Stable Diffusion"},
	{"runway", "Runway"}, {"sora", "Sora"}, {"kling", "Kling"}, {"suno", "Suno"},
	// skills
	{"video-editing", "Видеомонтаж"}, {"color-grading", "Цветокоррекция"},
	{"audio-editing", "Обработка звука"}, {"editing-theory", "Теория монтажа"},
	{"storytelling", "Сторителлинг"}, {"editing-direction", "Режиссура монтажа"},
	{"motion-design", "Моушн-дизайн"},
	{"graphic-design", "Графический дизайн"}, {"illustration", "Иллюстрирование"},
	{"vector-graphics", "Векторная графика"}, {"typography", "Типографика"},
	{"branding", "Фирменный стиль"}, {"design-concept", "Дизайн-концепция"},
	{"web-design", "Веб-дизайн"}, {"print-design", "Полиграфический дизайн"},
	{"packaging-design", "Дизайн упаковки"}, {"outdoor-design", "Наружная реклама"},
	{"email-design", "Дизайн рассылок"}, {"retouching", "Ретушь"},
	{"videography", "Видеосъёмка"},
	{"photography", "Фотография"}, {"product-photography", "Предметная фотосъёмка"},
	{"photo-retouching", "Фото-ретушь"},
	{"scriptwriting", "Сценарии"}, {"copywriting", "Копирайтинг"},
	{"smm-strategy", "SMM-стратегия"}, {"content-planning", "Контент-план"},
	{"content-marketing", "Контент-маркетинг"}, {"stories-making", "Сторисмейкинг"},
	{"influencer-marketing", "Инфлюенс-маркетинг"}, {"messenger-marketing", "Мессенджер-маркетинг"},
	{"targeted-ads", "Таргетированная реклама"}, {"contextual-ads", "Контекстная реклама"},
	{"retargeting", "Ретаргетинг"}, {"lead-generation", "Лидогенерация"},
	{"campaign-planning", "Планирование кампаний"}, {"seo", "SEO"},
	{"prompt-engineering", "Промптинг"},
	{"acting", "Актёрское мастерство"}, {"voiceover", "Озвучка"},
	// platforms
	{"reels", "Instagram Reels"}, {"tiktok", "TikTok"}, {"youtube", "YouTube"},
	{"shorts", "YouTube Shorts"}, {"vk-clips", "VK Клипы"}, {"telegram", "Telegram"},
}

// fallbackCategories — статический список, используется если БД-источник
// категорий недоступен (lister == nil или вернул ошибку). Должен совпадать
// с миграцией `specialty_categories` — иначе LLM не узнает о категориях,
// которые на самом деле существуют (designer, photographer и т.п.).
var fallbackCategories = []CategoryRef{
	{"editor", "Монтажёр", "видеомонтаж, нарезка, цветокор"},
	{"video_director", "Видеоредактор / режиссёр монтажа", "концепция и сторителлинг"},
	{"motion", "Моушн-дизайнер", "After Effects, анимация"},
	{"scriptwriter", "Сценарист", "сценарии для роликов, шортсов, рекламы"},
	{"ugc", "UGC-контент", "ролики от первого лица для брендов"},
	{"videographer", "Видеооператор", "съёмка рекламных роликов, репортаж, интервью"},
	{"photographer", "Фотограф", "предметная, портретная, репортажная съёмка"},
	{"actor", "Актёр", "съёмки в рекламе и UGC, озвучка"},
	{"designer", "Дизайнер", "графический дизайн, креативы, обложки, превью"},
	{"ai_creator", "ИИ-креатор", "генерация видео/фото/звука через нейросети, промптинг"},
	{"smm", "СММ", "ведение соцсетей, контент-планы"},
	{"blogger", "Блогер", "своя аудитория, интеграции"},
	{"ads_seo", "Таргет + SEO", "таргетированная реклама и SEO-продвижение"},
	{"seeding", "Посевы", "размещения в каналах и пабликах"},
}

// buildSystemPrompt собирает system-prompt из живых списков категорий и
// навыков. Если cats/skills пустые — берёт fallback'и из этого же файла.
// Если задан category — добавляет в конец секцию ТЕКУЩИЙ КОНТЕКСТ с
// зафиксированным кодом, и навыки приходят уже отфильтрованные по нему +
// платформы.
func buildSystemPrompt(cats []CategoryRef, skills []SkillRef, category string) string {
	if len(cats) == 0 {
		cats = fallbackCategories
	}
	if len(skills) == 0 {
		skills = fallbackSkills
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
	b.WriteString("\nНАВЫКИ (slug — название)\nИспользуй только slug из этого списка. Если пользователь упомянул что-то другое — оставь свободным текстом в q.\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- %s — %s\n", s.Slug, s.Title)
	}
	b.WriteString(promptTailStart)
	if category != "" {
		b.WriteString("\n\nТЕКУЩИЙ КОНТЕКСТ\nПользователь уже выбрал категорию `")
		b.WriteString(category)
		b.WriteString("`. Категорию переспрашивать не надо — она уже зафиксирована и должна попасть в search.categories. Скиллы выше отфильтрованы под эту категорию плюс универсальные платформы.")
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
