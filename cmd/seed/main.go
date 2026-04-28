package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"

	"marketpclce/internal/config"
	"marketpclce/internal/platform/db"
	"marketpclce/internal/platform/es"
	"marketpclce/internal/search"
)

type seedSpec struct {
	Email            string
	DisplayName      string
	Bio              string
	City             string
	RateMin, RateMax int
	Currency         string
	PrimaryCategory  string
	Categories       []string
	SkillSlugs       []string
	Rating           float64
	Reviews          int
}

var seeds = []seedSpec{
	{
		Email: "anya.editor@example.com", DisplayName: "Аня Соколова",
		Bio:             "Монтаж Reels и шортсов под бьюти и фитнес-бренды. Premiere + CapCut, ритмичный монтаж под музыку, цветокор и саб-титры.",
		City:            "Москва", RateMin: 8000, RateMax: 25000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor"},
		SkillSlugs:      []string{"premiere", "capcut", "reels", "shorts", "tiktok"},
		Rating:          4.8, Reviews: 27,
	},
	{
		Email: "ivan.director@example.com", DisplayName: "Иван Грибов",
		Bio:             "Режиссёр монтажа для YouTube-каналов. Выстраиваю драматургию из сырого материала, длинные ролики и серии. DaVinci, Premiere.",
		City:            "Санкт-Петербург", RateMin: 30000, RateMax: 80000, Currency: "RUB",
		PrimaryCategory: "video_director", Categories: []string{"video_director", "editor"},
		SkillSlugs:      []string{"davinci", "premiere", "youtube"},
		Rating:          4.9, Reviews: 14,
	},
	{
		Email: "lera.motion@example.com", DisplayName: "Лера Цой",
		Bio:             "Моушн-дизайн в After Effects: анимация лого, инфографика, титры, объяснялки. Стилевые гайды под бренд.",
		City:            "Москва", RateMin: 20000, RateMax: 60000, Currency: "RUB",
		PrimaryCategory: "motion", Categories: []string{"motion"},
		SkillSlugs:      []string{"after-effects", "photoshop", "figma"},
		Rating:          4.7, Reviews: 18,
	},
	{
		Email: "kirill.script@example.com", DisplayName: "Кирилл Лемешев",
		Bio:             "Сценарии для рекламных роликов и шортсов. Образование — киновед. Работал с e-com и edtech.",
		City:            "удалённо", RateMin: 5000, RateMax: 25000, Currency: "RUB",
		PrimaryCategory: "scriptwriter", Categories: []string{"scriptwriter"},
		SkillSlugs:      []string{"reels", "shorts", "youtube"},
		Rating:          4.6, Reviews: 9,
	},
	{
		Email: "polina.smm@example.com", DisplayName: "Полина Ким",
		Bio:             "Веду соцсети салонов красоты и небольших food-проектов. Контент-планы, тексты постов, координация съёмок и монтажа.",
		City:            "Москва", RateMin: 30000, RateMax: 70000, Currency: "RUB",
		PrimaryCategory: "smm", Categories: []string{"smm"},
		SkillSlugs:      []string{"reels", "telegram", "tiktok"},
		Rating:          4.5, Reviews: 11,
	},
	{
		Email: "max.ugc@example.com", DisplayName: "Макс Орлов",
		Bio:             "UGC-ролики для брендов косметики, электроники и БАДов. Снимаю на iPhone, монтирую в CapCut. Распаковки и обзоры.",
		City:            "Краснодар", RateMin: 6000, RateMax: 20000, Currency: "RUB",
		PrimaryCategory: "ugc", Categories: []string{"ugc"},
		SkillSlugs:      []string{"capcut", "reels", "tiktok"},
		Rating:          4.8, Reviews: 32,
	},
	{
		Email: "natasha.blogger@example.com", DisplayName: "Наташа Ветрова",
		Bio:             "Лайфстайл-блогер: 180k в Reels, 90k в TikTok. Интеграции брендов косметики, фуд, спорт. Опыт три года.",
		City:            "Москва", RateMin: 60000, RateMax: 250000, Currency: "RUB",
		PrimaryCategory: "blogger", Categories: []string{"blogger"},
		SkillSlugs:      []string{"reels", "tiktok"},
		Rating:          4.9, Reviews: 21,
	},
	{
		Email: "denis.ads@example.com", DisplayName: "Денис Мартынов",
		Bio:             "Таргет в Meta Ads и ВК Реклама, SEO под Яндекс. Запускал кампании для e-com, edtech, локальных услуг. Считаю экономику до клика.",
		City:            "удалённо", RateMin: 40000, RateMax: 120000, Currency: "RUB",
		PrimaryCategory: "ads_seo", Categories: []string{"ads_seo"},
		SkillSlugs:      []string{},
		Rating:          4.6, Reviews: 13,
	},
	{
		Email: "olya.seeding@example.com", DisplayName: "Оля Гриневич",
		Bio:             "Посевы в Telegram-каналах и пабликах VK: подбор площадок под целевую аудиторию, переговоры, расчёт CPM.",
		City:            "удалённо", RateMin: 15000, RateMax: 70000, Currency: "RUB",
		PrimaryCategory: "seeding", Categories: []string{"seeding"},
		SkillSlugs:      []string{"telegram"},
		Rating:          4.7, Reviews: 8,
	},
	{
		Email: "tim.editor2@example.com", DisplayName: "Тим Захарченко",
		Bio:             "Монтаж длинных YouTube-роликов и интервью. Чищу звук, делаю b-roll, склейки, графику в After Effects.",
		City:            "Минск", RateMin: 12000, RateMax: 40000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor", "motion"},
		SkillSlugs:      []string{"premiere", "after-effects", "youtube"},
		Rating:          4.5, Reviews: 6,
	},
	// editor — ещё монтажёры
	{
		Email: "rita.editor@example.com", DisplayName: "Рита Васнецова",
		Bio:             "Монтаж шортсов и Reels для food-проектов и ресторанов. Final Cut, цветокор, динамичные склейки.",
		City:            "Москва", RateMin: 7000, RateMax: 20000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor"},
		SkillSlugs:      []string{"final-cut", "capcut", "reels", "shorts"},
		Rating:          4.7, Reviews: 19,
	},
	{
		Email: "egor.editor@example.com", DisplayName: "Егор Пушкарёв",
		Bio:             "Монтажёр игровых стримов и YouTube-шортсов. Знаю динамику геймплея, делаю быстрые мемные нарезки.",
		City:            "Новосибирск", RateMin: 5000, RateMax: 15000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor"},
		SkillSlugs:      []string{"premiere", "shorts", "youtube", "tiktok"},
		Rating:          4.4, Reviews: 22,
	},
	{
		Email: "sasha.editor@example.com", DisplayName: "Саша Береснев",
		Bio:             "Свадебный и event-монтаж. Многокамерная синхронизация, цветокор, music-video стиль.",
		City:            "Казань", RateMin: 15000, RateMax: 50000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor", "video_director"},
		SkillSlugs:      []string{"davinci", "premiere"},
		Rating:          4.8, Reviews: 41,
	},
	{
		Email: "vlad.editor@example.com", DisplayName: "Влад Окороков",
		Bio:             "Монтаж рекламных роликов под VK Клипы и Telegram. Быстрый ритм, продающие склейки.",
		City:            "удалённо", RateMin: 6000, RateMax: 18000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor"},
		SkillSlugs:      []string{"capcut", "premiere", "vk-clips", "telegram"},
		Rating:          4.6, Reviews: 12,
	},

	// video_director
	{
		Email: "alina.director@example.com", DisplayName: "Алина Маслова",
		Bio:             "Режиссёр монтажа документальных и тревел-проектов. Сторителлинг, ритм, голос автора.",
		City:            "Москва", RateMin: 40000, RateMax: 120000, Currency: "RUB",
		PrimaryCategory: "video_director", Categories: []string{"video_director"},
		SkillSlugs:      []string{"davinci", "premiere", "youtube"},
		Rating:          4.9, Reviews: 28,
	},
	{
		Email: "stas.director@example.com", DisplayName: "Стас Гонтарев",
		Bio:             "Делаю YouTube-сериалы для блогеров: концепция, сценарная структура, постпродакшн.",
		City:            "удалённо", RateMin: 50000, RateMax: 150000, Currency: "RUB",
		PrimaryCategory: "video_director", Categories: []string{"video_director", "scriptwriter"},
		SkillSlugs:      []string{"premiere", "youtube"},
		Rating:          4.7, Reviews: 9,
	},

	// motion
	{
		Email: "katya.motion@example.com", DisplayName: "Катя Долинина",
		Bio:             "Объяснительная анимация для финтеха и edtech. Скрайбинг, инфографика, character animation.",
		City:            "Санкт-Петербург", RateMin: 25000, RateMax: 80000, Currency: "RUB",
		PrimaryCategory: "motion", Categories: []string{"motion"},
		SkillSlugs:      []string{"after-effects", "figma", "photoshop"},
		Rating:          4.8, Reviews: 22,
	},
	{
		Email: "artem.motion@example.com", DisplayName: "Артём Колосов",
		Bio:             "3D-моушн в Cinema 4D + After Effects. Лого-анимации, продуктовые ролики, реклама гаджетов.",
		City:            "Москва", RateMin: 50000, RateMax: 200000, Currency: "RUB",
		PrimaryCategory: "motion", Categories: []string{"motion"},
		SkillSlugs:      []string{"after-effects", "photoshop"},
		Rating:          4.9, Reviews: 16,
	},
	{
		Email: "yulia.motion@example.com", DisplayName: "Юля Раздобарина",
		Bio:             "Моушн-дизайн для Reels и TikTok: kinetic-типографика, стикеры, переходы. Делаю быстро и в стиле бренда.",
		City:            "Минск", RateMin: 8000, RateMax: 25000, Currency: "RUB",
		PrimaryCategory: "motion", Categories: []string{"motion", "editor"},
		SkillSlugs:      []string{"after-effects", "capcut", "reels", "tiktok"},
		Rating:          4.7, Reviews: 30,
	},

	// scriptwriter
	{
		Email: "marina.script@example.com", DisplayName: "Марина Трофимова",
		Bio:             "Сценарии вертикальных роликов для бренд-кампаний. Опыт — beauty, fashion, e-commerce.",
		City:            "Москва", RateMin: 8000, RateMax: 30000, Currency: "RUB",
		PrimaryCategory: "scriptwriter", Categories: []string{"scriptwriter"},
		SkillSlugs:      []string{"reels", "shorts", "tiktok"},
		Rating:          4.6, Reviews: 14,
	},
	{
		Email: "boris.script@example.com", DisplayName: "Борис Лавров",
		Bio:             "Сценарист длинных YouTube-видео и интервью. Структура, цепляющий заход, ретеншн.",
		City:            "удалённо", RateMin: 15000, RateMax: 60000, Currency: "RUB",
		PrimaryCategory: "scriptwriter", Categories: []string{"scriptwriter", "video_director"},
		SkillSlugs:      []string{"youtube"},
		Rating:          4.8, Reviews: 11,
	},

	// smm
	{
		Email: "vera.smm@example.com", DisplayName: "Вера Соломина",
		Bio:             "СММ для образовательных проектов и онлайн-школ. Контент-планы, прогрев, продуктовая воронка в постах.",
		City:            "удалённо", RateMin: 40000, RateMax: 90000, Currency: "RUB",
		PrimaryCategory: "smm", Categories: []string{"smm"},
		SkillSlugs:      []string{"telegram", "reels"},
		Rating:          4.7, Reviews: 17,
	},
	{
		Email: "lena.smm@example.com", DisplayName: "Лена Зыкова",
		Bio:             "Веду инстаграм фитнес-тренеров и нутрициологов. Сторис, рилсы, рубрики, копирайт постов.",
		City:            "Москва", RateMin: 25000, RateMax: 60000, Currency: "RUB",
		PrimaryCategory: "smm", Categories: []string{"smm"},
		SkillSlugs:      []string{"reels", "tiktok"},
		Rating:          4.6, Reviews: 25,
	},
	{
		Email: "roma.smm@example.com", DisplayName: "Рома Карпов",
		Bio:             "СММ для b2b и tech-стартапов. LinkedIn, Telegram, экспертный контент, кейсы. Аналитика подписок.",
		City:            "Санкт-Петербург", RateMin: 50000, RateMax: 120000, Currency: "RUB",
		PrimaryCategory: "smm", Categories: []string{"smm"},
		SkillSlugs:      []string{"telegram"},
		Rating:          4.8, Reviews: 7,
	},

	// ugc
	{
		Email: "diana.ugc@example.com", DisplayName: "Диана Сурикова",
		Bio:             "UGC-ролики для бьюти и фэшн брендов. Возраст 25+, естественная подача, опыт 80+ роликов.",
		City:            "Москва", RateMin: 5000, RateMax: 18000, Currency: "RUB",
		PrimaryCategory: "ugc", Categories: []string{"ugc"},
		SkillSlugs:      []string{"capcut", "reels", "tiktok"},
		Rating:          4.9, Reviews: 47,
	},
	{
		Email: "sveta.ugc@example.com", DisplayName: "Света Бойкова",
		Bio:             "UGC для food, БАДов, бытовой техники. Подача с юмором, мама в декрете, аудитория 25-40.",
		City:            "Воронеж", RateMin: 4000, RateMax: 12000, Currency: "RUB",
		PrimaryCategory: "ugc", Categories: []string{"ugc"},
		SkillSlugs:      []string{"capcut", "reels"},
		Rating:          4.7, Reviews: 35,
	},
	{
		Email: "kostya.ugc@example.com", DisplayName: "Костя Литвинов",
		Bio:             "Мужской UGC: техника, авто, спорт, барбершоп. Снимаю на iPhone Pro, делаю распаковки и обзоры.",
		City:            "Екатеринбург", RateMin: 6000, RateMax: 20000, Currency: "RUB",
		PrimaryCategory: "ugc", Categories: []string{"ugc"},
		SkillSlugs:      []string{"capcut", "tiktok", "reels"},
		Rating:          4.6, Reviews: 28,
	},

	// blogger
	{
		Email: "kira.blogger@example.com", DisplayName: "Кира Ефимова",
		Bio:             "Бьюти-блогер: 320k в Reels, 110k в TikTok. Интеграции косметики, парфюма, ухода. Прозрачная статистика.",
		City:            "Москва", RateMin: 80000, RateMax: 350000, Currency: "RUB",
		PrimaryCategory: "blogger", Categories: []string{"blogger"},
		SkillSlugs:      []string{"reels", "tiktok"},
		Rating:          4.9, Reviews: 18,
	},
	{
		Email: "gleb.blogger@example.com", DisplayName: "Глеб Тарасенко",
		Bio:             "Авто-блогер: 90k YouTube, 60k Telegram. Делаю обзоры, тест-драйвы, нативные интеграции с автохимией и аксессуарами.",
		City:            "Москва", RateMin: 60000, RateMax: 220000, Currency: "RUB",
		PrimaryCategory: "blogger", Categories: []string{"blogger"},
		SkillSlugs:      []string{"youtube", "telegram"},
		Rating:          4.8, Reviews: 12,
	},
	{
		Email: "alya.blogger@example.com", DisplayName: "Аля Деменко",
		Bio:             "Тревел-блогер: 50k Reels, 40k YouTube Shorts. Бренды отелей, авиакомпаний, чемоданов и тревел-аксессуаров.",
		City:            "удалённо", RateMin: 40000, RateMax: 150000, Currency: "RUB",
		PrimaryCategory: "blogger", Categories: []string{"blogger"},
		SkillSlugs:      []string{"reels", "shorts", "youtube"},
		Rating:          4.7, Reviews: 9,
	},
	{
		Email: "fedor.blogger@example.com", DisplayName: "Фёдор Барсуков",
		Bio:             "IT-блогер для разработчиков и тимлидов: 35k Telegram, 22k YouTube. Спонсорские интеграции SaaS-сервисов и edtech.",
		City:            "удалённо", RateMin: 50000, RateMax: 180000, Currency: "RUB",
		PrimaryCategory: "blogger", Categories: []string{"blogger"},
		SkillSlugs:      []string{"telegram", "youtube"},
		Rating:          4.8, Reviews: 14,
	},

	// ads_seo
	{
		Email: "ksenia.ads@example.com", DisplayName: "Ксения Бутова",
		Bio:             "Таргет в инстаграм и ВК для салонов красоты, клиник, фитнес-клубов. Запускаю с нуля, считаю CAC и ROMI.",
		City:            "Москва", RateMin: 30000, RateMax: 90000, Currency: "RUB",
		PrimaryCategory: "ads_seo", Categories: []string{"ads_seo"},
		SkillSlugs:      []string{},
		Rating:          4.7, Reviews: 22,
	},
	{
		Email: "pasha.ads@example.com", DisplayName: "Паша Ягодин",
		Bio:             "Контекст в Яндекс.Директ для e-com и услуг. SEO под Яндекс. Опыт 6 лет, кейсы в нише ремонта и стройки.",
		City:            "Санкт-Петербург", RateMin: 50000, RateMax: 150000, Currency: "RUB",
		PrimaryCategory: "ads_seo", Categories: []string{"ads_seo"},
		SkillSlugs:      []string{},
		Rating:          4.8, Reviews: 30,
	},
	{
		Email: "milana.ads@example.com", DisplayName: "Милана Хатунцева",
		Bio:             "Таргет TikTok Ads и продвижение в Reels через креативы. Опыт с фэшн и косметическими DTC-брендами.",
		City:            "удалённо", RateMin: 35000, RateMax: 120000, Currency: "RUB",
		PrimaryCategory: "ads_seo", Categories: []string{"ads_seo"},
		SkillSlugs:      []string{"tiktok", "reels"},
		Rating:          4.6, Reviews: 11,
	},

	// seeding
	{
		Email: "georgy.seeding@example.com", DisplayName: "Георгий Андреев",
		Bio:             "Посевы в Telegram-каналах: тревел, IT, бизнес, лайфстайл. База 600+ каналов, своя аналитика по CPM.",
		City:            "удалённо", RateMin: 30000, RateMax: 120000, Currency: "RUB",
		PrimaryCategory: "seeding", Categories: []string{"seeding"},
		SkillSlugs:      []string{"telegram"},
		Rating:          4.8, Reviews: 15,
	},
	{
		Email: "alisa.seeding@example.com", DisplayName: "Алиса Тропина",
		Bio:             "Посевы в VK-пабликах для food, fashion, lifestyle брендов. Бартер и платные размещения.",
		City:            "Москва", RateMin: 12000, RateMax: 50000, Currency: "RUB",
		PrimaryCategory: "seeding", Categories: []string{"seeding"},
		SkillSlugs:      []string{"vk-clips"},
		Rating:          4.6, Reviews: 11,
	},

	// смешанные / редкие города
	{
		Email: "nina.smm@example.com", DisplayName: "Нина Чабан",
		Bio:             "СММ + копирайтинг для крафтовых производителей: керамика, кожа, мыло. Эстетика ручной работы.",
		City:            "Калининград", RateMin: 18000, RateMax: 45000, Currency: "RUB",
		PrimaryCategory: "smm", Categories: []string{"smm", "scriptwriter"},
		SkillSlugs:      []string{"reels", "telegram"},
		Rating:          4.5, Reviews: 8,
	},
	{
		Email: "petya.editor@example.com", DisplayName: "Петя Хакимов",
		Bio:             "Монтажёр медицинских и образовательных видео: лекции, обучающие модули, лонгриды. Чёткая структура и графика.",
		City:            "Уфа", RateMin: 10000, RateMax: 35000, Currency: "RUB",
		PrimaryCategory: "editor", Categories: []string{"editor", "motion"},
		SkillSlugs:      []string{"premiere", "after-effects", "youtube"},
		Rating:          4.6, Reviews: 16,
	},
	{
		Email: "marusya.ugc@example.com", DisplayName: "Маруся Зайцева",
		Bio:             "Подростковый UGC: товары для школьников, гаджеты, канцелярия. Аудитория 12-18, естественная подача.",
		City:            "Самара", RateMin: 3000, RateMax: 10000, Currency: "RUB",
		PrimaryCategory: "ugc", Categories: []string{"ugc"},
		SkillSlugs:      []string{"capcut", "tiktok", "reels"},
		Rating:          4.4, Reviews: 19,
	},
}

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		fatal("config", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		fatal("db", err)
	}
	defer pool.Close()

	esClient := es.New(cfg.OpenSearchURL)
	if err := esClient.EnsureIndex(ctx, cfg.OpenSearchIndexProfile, search.IndexMapping()); err != nil {
		slog.Warn("ensure index", "err", err)
	}

	skillByslug := make(map[string]uuid.UUID)
	{
		rows, err := pool.Query(ctx, `SELECT id, slug FROM skills`)
		if err != nil {
			fatal("query skills", err)
		}
		for rows.Next() {
			var id uuid.UUID
			var slug string
			if err := rows.Scan(&id, &slug); err != nil {
				fatal("scan skill", err)
			}
			skillByslug[slug] = id
		}
		rows.Close()
	}

	hash, err := bcrypt.GenerateFromPassword([]byte("seedseed123"), bcrypt.DefaultCost)
	if err != nil {
		fatal("hash", err)
	}

	searchRepo := search.NewRepo(pool)

	clientID, err := upsertClientUser(ctx, pool, "demo-client@example.com", string(hash))
	if err != nil {
		fatal("upsert demo client", err)
	}

	count := 0
	for i, s := range seeds {
		uid, err := upsertUser(ctx, pool, s.Email, string(hash))
		if err != nil {
			slog.Error("upsert user", "email", s.Email, "err", err)
			continue
		}
		if err := upsertProfile(ctx, pool, uid, s); err != nil {
			slog.Error("upsert profile", "email", s.Email, "err", err)
			continue
		}
		if err := replaceCategories(ctx, pool, uid, s.Categories, s.PrimaryCategory); err != nil {
			slog.Error("categories", "email", s.Email, "err", err)
			continue
		}
		if err := replaceSkills(ctx, pool, uid, s.SkillSlugs, skillByslug); err != nil {
			slog.Error("skills", "email", s.Email, "err", err)
			continue
		}
		if err := seedReviews(ctx, pool, clientID, uid, s, i); err != nil {
			slog.Error("seed reviews", "email", s.Email, "err", err)
			continue
		}
		doc, err := searchRepo.LoadDoc(ctx, uid)
		if err != nil {
			slog.Error("load doc", "email", s.Email, "err", err)
			continue
		}
		if err := esClient.IndexDoc(ctx, cfg.OpenSearchIndexProfile, uid.String(), doc); err != nil {
			slog.Error("index", "email", s.Email, "err", err)
			continue
		}
		count++
		slog.Info("seeded", "email", s.Email, "user_id", uid.String())
	}
	slog.Info("done", "seeded", count, "total", len(seeds))
}

func fatal(label string, err error) {
	slog.Error(label, "err", err)
	os.Exit(1)
}

func upsertUser(ctx context.Context, pool *pgxpool.Pool, email, hash string) (uuid.UUID, error) {
	const q = `
INSERT INTO users (email, password_hash, kind)
VALUES ($1, $2, 'specialist')
ON CONFLICT (email) DO UPDATE SET updated_at = now()
RETURNING id`
	var id uuid.UUID
	err := pool.QueryRow(ctx, q, email, hash).Scan(&id)
	return id, err
}

func upsertClientUser(ctx context.Context, pool *pgxpool.Pool, email, hash string) (uuid.UUID, error) {
	const q = `
INSERT INTO users (email, password_hash, kind)
VALUES ($1, $2, 'client')
ON CONFLICT (email) DO UPDATE SET updated_at = now()
RETURNING id`
	var id uuid.UUID
	err := pool.QueryRow(ctx, q, email, hash).Scan(&id)
	return id, err
}

var reviewAuthors = []string{
	"Мария К.", "Алексей П.", "Дарья С.", "Никита В.",
	"Ольга М.", "Артём Л.", "Светлана Б.", "Денис Ж.",
	"Виктория Т.", "Павел Н.",
}

var reviewTexts = []string{
	"Сделал быстро и в срок, по правкам — без вопросов. Рекомендую.",
	"Отличный результат: попали в стиль бренда с первой итерации.",
	"Очень внимательно отнёсся к ТЗ, предложил пару решений лучше моих. Берём ещё.",
	"Работали удалённо, всё чётко по дедлайнам. Качеством довольны.",
	"Профессионально и без воды. Связь была всегда, на правки реагирует быстро.",
	"Сильное портфолио — и реальная работа полностью соответствует ожиданиям.",
	"Сложный бриф, разобрались вместе. Финальная сдача без правок.",
	"Долго искали такого специалиста — наконец нашли. Спасибо!",
}

func seedReviews(ctx context.Context, pool *pgxpool.Pool, authorID, targetID uuid.UUID, s seedSpec, idx int) error {
	if _, err := pool.Exec(ctx, `DELETE FROM reviews WHERE target_user_id = $1`, targetID); err != nil {
		return fmt.Errorf("clear reviews: %w", err)
	}
	n := 3
	if s.Reviews < n {
		n = s.Reviews
	}
	if n <= 0 {
		return nil
	}
	base := time.Now().Add(-30 * 24 * time.Hour)
	for k := 0; k < n; k++ {
		author := reviewAuthors[(idx+k)%len(reviewAuthors)]
		text := reviewTexts[(idx*3+k)%len(reviewTexts)]
		rating := 5
		if k == n-1 && s.Rating < 4.8 {
			rating = 4
		}
		created := base.Add(time.Duration(k*7*24) * time.Hour)
		if _, err := pool.Exec(ctx, `
INSERT INTO reviews (author_user_id, target_user_id, rating, text, author_name, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
			authorID, targetID, rating, text, author, created); err != nil {
			return fmt.Errorf("insert review: %w", err)
		}
	}
	return nil
}

func upsertProfile(ctx context.Context, pool *pgxpool.Pool, uid uuid.UUID, s seedSpec) error {
	const q = `
INSERT INTO specialist_profiles (
  user_id, display_name, bio, city, rate_min, rate_max, currency,
  is_published, rating_avg, reviews_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, $8, $9)
ON CONFLICT (user_id) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  bio          = EXCLUDED.bio,
  city         = EXCLUDED.city,
  rate_min     = EXCLUDED.rate_min,
  rate_max     = EXCLUDED.rate_max,
  currency     = EXCLUDED.currency,
  is_published = TRUE,
  rating_avg   = EXCLUDED.rating_avg,
  reviews_count= EXCLUDED.reviews_count,
  updated_at   = now()`
	_, err := pool.Exec(ctx, q,
		uid, s.DisplayName, s.Bio, s.City,
		s.RateMin, s.RateMax, s.Currency,
		s.Rating, s.Reviews,
	)
	return err
}

func replaceCategories(ctx context.Context, pool *pgxpool.Pool, uid uuid.UUID, codes []string, primary string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM specialist_categories WHERE user_id = $1`, uid); err != nil {
		return err
	}
	for _, code := range codes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO specialist_categories (user_id, category_code, is_primary) VALUES ($1, $2, $3)`,
			uid, code, code == primary); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func replaceSkills(ctx context.Context, pool *pgxpool.Pool, uid uuid.UUID, slugs []string, byslug map[string]uuid.UUID) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM specialist_skills WHERE user_id = $1`, uid); err != nil {
		return err
	}
	for _, slug := range slugs {
		sid, ok := byslug[slug]
		if !ok {
			return fmt.Errorf("unknown skill slug %q", slug)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO specialist_skills (user_id, skill_id) VALUES ($1, $2)`,
			uid, sid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
