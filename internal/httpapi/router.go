package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"marketpclce/internal/admin"
	"marketpclce/internal/auth"
	"marketpclce/internal/billing"
	"marketpclce/internal/catalog"
	"marketpclce/internal/clarify"
	"marketpclce/internal/feed"
	"marketpclce/internal/httpapi/handlers"
	"marketpclce/internal/leads"
	"marketpclce/internal/orders"
	"marketpclce/internal/partner"
	"marketpclce/internal/pipelines"
	"marketpclce/internal/productions"
	"marketpclce/internal/profilecheck"
	"marketpclce/internal/profiles"
	"marketpclce/internal/projects"
	"marketpclce/internal/publications"
	"marketpclce/internal/ratelimit"
	"marketpclce/internal/ratings"
	"marketpclce/internal/reviews"
	"marketpclce/internal/search"
	"marketpclce/internal/summarize"
	"marketpclce/internal/support"
)

type Deps struct {
	Logger      *slog.Logger
	HealthDB    handlers.HealthDB
	TokenIssuer *auth.TokenIssuer
	Auth        *auth.Handler
	AuthRepo    auth.IdentityLoader
	// AuthRevocation — чекер отзыва access-токенов (data-sec D8). nil = без
	// проверки (только для тестового окружения). В проде проставляется в
	// cmd/api/main.go тем же *auth.Repo.
	AuthRevocation auth.RevocationChecker
	Catalog        *catalog.Handler
	Profiles       *profiles.Handler
	ProfileCheck   *profilecheck.Handler
	Search         *search.Handler
	Feed           *feed.Handler
	Summarize      *summarize.Handler
	Clarify        *clarify.Handler
	Leads          *leads.Handler
	Reviews        *reviews.Handler
	Productions    *productions.Handler
	Pipelines      *pipelines.Handler
	Projects       *projects.Handler
	// Publications — выкладки креаторов на проектной странице
	// (проекты вида creators_turnkey). nil — ручки не маунтятся.
	Publications *publications.Handler
	// Orders — самостоятельный подбор креаторов клиентом.
	Orders *orders.Handler
	// Billing — условия, платежи заказчика и начисления креаторам.
	// nil — денежные ручки не маунтятся.
	Billing *billing.Handler
	Support *support.Handler
	// Partner — подтверждение регистрации для «Бота Работ». nil, если общий
	// секрет не задан: тогда ручки просто нет, а не есть неработающая.
	Partner *partner.Handler
	Admin   *admin.Handler
	// Ratings — справочник порогов оценок. nil — админские ручки не
	// маунтятся.
	Ratings *ratings.Handler

	CORSOrigins []string

	Limiter      *ratelimit.Limiter
	ReadWindows  []ratelimit.Window
	LeadsWindows []ratelimit.Window
	// UploadWindows — на выдачу presigned-ссылок. Единственное место, где
	// залогиненный человек тратит наши деньги: ссылка = объект в бакете, а
	// размер объявляет клиент, и подпись его не навязывает.
	UploadWindows    []ratelimit.Window
	ClarifyWindows   []ratelimit.Window
	AuthWindows      []ratelimit.Window
	SummarizeWindows []ratelimit.Window
	CRMWindows       []ratelimit.Window
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(120 * time.Second))
	r.Use(slogRequestLogger(d.Logger))
	r.Use(CORS(d.CORSOrigins))
	r.Use(PrometheusMetrics())

	health := handlers.NewHealth(d.HealthDB)
	r.Get("/healthz", health.Live)
	r.Get("/readyz", health.Ready)

	// /metrics не проходит через Caddy (он проксирует только /api/* и
	// /swagger/*), поэтому эндпоинт доступен только внутри docker network —
	// alloy скрейпит api:8080/metrics, наружу не торчит.
	r.Handle("/metrics", promhttp.Handler())

	// Страница специалиста с og-метой под конкретного человека. Живёт вне
	// /api/v1: это не API, а HTML-оболочка SPA — Caddy проксирует сюда
	// /specialist/* именно ради превью ссылок в мессенджерах, которые JS
	// не исполняют. См. internal/profiles/og.go.
	r.Get("/specialist/{id}", d.Profiles.SpecialistPage)

	r.Route("/api/v1", func(r chi.Router) {
		// /auth/* — анти-брутфорс по IP. register/login/refresh/verify-email
		// без auth, поэтому ключ — только IP. Логин/регистрация на одной
		// корзине: 10 попыток в минуту суммарно достаточно для живого юзера
		// и душит автоматизированный перебор.
		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, "auth", d.AuthWindows))
			r.Post("/auth/register", d.Auth.Register)
			// Под тем же лимитом, что register/login: перебирать адреса через
			// этот роут не быстрее, чем через форму входа.
			r.Get("/auth/email-available", d.Auth.EmailAvailable)
			// Вход через Яндекс: фронт присылает одноразовый code, обмен на
			// токен делает бэкенд — client_secret на клиент не попадает.
			r.Post("/auth/yandex", d.Auth.YandexLogin)
			r.Post("/auth/login", d.Auth.Login)
			r.Post("/auth/refresh", d.Auth.Refresh)
			r.Post("/auth/verify-email", d.Auth.VerifyEmail)
			r.Post("/auth/password-reset/request", d.Auth.RequestPasswordReset)
			r.Post("/auth/password-reset/confirm", d.Auth.ConfirmPasswordReset)
		})

		r.Get("/categories", d.Catalog.Categories)
		r.Get("/skills", d.Catalog.Skills)
		// Публичный список активных продакшенов — нужен на форме профиля
		// специалиста (выбор production_id vs is_freelance). См. Ф2.
		if d.Productions != nil {
			r.Get("/productions", d.Productions.Public)
		}

		// Статус профиля для «Бота Работ». Не под пользовательской
		// авторизацией: спрашивает сервер о сервере, человека в этот момент
		// на линии нет. Пускает общий секрет — тот же, которым подписаны
		// вебхуки; ручка проверяет его сама.
		if d.Partner != nil {
			r.Get("/partner/specialists/{id}/status", d.Partner.SpecialistStatus)
		}

		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, "read", d.ReadWindows))
			// /specialists/{id} оборачиваем в OptionalAuth: если caller
			// прислал валидный Bearer-токен И он = owner профиля, handler
			// отдаёт превью (даже если is_published=false или moderation
			// не approved). Без токена / для чужих — публичный strict-фильтр.
			r.With(auth.OptionalMiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation)).
				Get("/specialists/{id}", d.Profiles.Public)
			r.Get("/search", d.Search.Search)
			r.Get("/categories/stats", d.Search.CategoryStats)
			r.Get("/specialists/{id}/reviews", d.Reviews.ListBySpecialist)
			if d.Feed != nil {
				r.Get("/feed", d.Feed.Feed)
			}
		})

		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, "summarize", d.SummarizeWindows))
			r.Post("/search/summarize", d.Summarize.Summarize)
		})
		if d.Clarify != nil {
			r.Group(func(r chi.Router) {
				r.Use(RateLimit(d.Limiter, "clarify", d.ClarifyWindows))
				r.Post("/clarify", d.Clarify.Clarify)
			})
		}

		r.Group(func(r chi.Router) {
			r.Use(auth.OptionalMiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation))
			r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
			r.Post("/leads", d.Leads.Create)
		})

		// Support: открыт гостям, JWT опционален. Под тем же rate-limit что
		// leads — 5/мин per IP, защита от спама из футера.
		if d.Support != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.OptionalMiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation))
				r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
				r.Post("/support/messages", d.Support.Create)
			})
		}

		// Публичный redeem_invite — magic-link обмен на JWT.
		// Под rate-limit "auth" группой: тот же лимит что register/login
		// (10/мин per IP) — защита от брутфорса token-ов.
		if d.Admin != nil {
			r.Group(func(r chi.Router) {
				r.Use(RateLimit(d.Limiter, "auth", d.AuthWindows))
				r.Post("/auth/redeem_invite/{token}", d.Admin.RedeemInvite)
			})
		}

		r.Group(func(r chi.Router) {
			r.Use(auth.MiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation))
			r.Get("/me", d.Auth.Me)
			// Pipelines читаются всеми залогиненными — нужен и менеджеру (для
			// канбана), и админу. Запись по-прежнему только под admin.
			if d.Pipelines != nil {
				r.Get("/pipelines/{id}", d.Pipelines.AdminGetPipeline)
			}
			r.Post("/auth/resend-verification", d.Auth.ResendVerification)
			// Привязка к «Боту Работ»: под авторизацией, потому что весь её
			// смысл — подтвердить аккаунт оттуда, где человек уже вошёл.
			if d.Partner != nil {
				r.Post("/partner/telegram-link", d.Partner.Link)
			}
			r.Get("/me/profile", d.Profiles.Get)
			r.Patch("/me/profile", d.Profiles.PatchFull)
			r.Get("/me/client-profile", d.Profiles.GetClient)
			r.Patch("/me/client-profile", d.Profiles.PatchClient)
			r.Post("/me/profile/publish", d.Profiles.Publish)
			r.Post("/me/profile/unpublish", d.Profiles.Unpublish)
			if d.ProfileCheck != nil {
				r.Post("/me/profile/check", d.ProfileCheck.Check)
			}

			r.Get("/me/portfolio", d.Profiles.PortfolioList)
			r.Post("/me/portfolio", d.Profiles.PortfolioCreate)
			r.Post("/me/portfolio/photoset", d.Profiles.PortfolioPhotoSetCreate)

			// Выдача presigned-ссылок — под своим лимитом. Остальной кабинет
			// не трогаем: там человек читает и правит свои же строки, а здесь
			// каждый вызов разрешает положить в бакет ещё один объект.
			// Размер при этом объявляет клиент, и подпись его не навязывает —
			// то есть без лимита счёт за хранение ограничен только совестью.
			r.Group(func(r chi.Router) {
				r.Use(RateLimit(d.Limiter, "uploads", d.UploadWindows))
				r.Post("/me/portfolio/upload-url", d.Profiles.PortfolioUploadURL)
				// S3 multipart для крупного видео (> 5 МБ, до 200 МБ).
				r.Post("/me/portfolio/multipart/start", d.Profiles.PortfolioMultipartStart)
				r.Post("/me/portfolio/multipart/part-url", d.Profiles.PortfolioMultipartPartURL)
				r.Post("/me/uploads/image", d.Profiles.ImageUploadURL)
			})
			r.Post("/me/portfolio/multipart/complete", d.Profiles.PortfolioMultipartComplete)
			r.Post("/me/portfolio/multipart/abort", d.Profiles.PortfolioMultipartAbort)
			r.Put("/me/portfolio/{id}/categories", d.Profiles.PortfolioSetCategories)
			r.Put("/me/portfolio/{id}/featured", d.Profiles.PortfolioSetFeatured)
			r.Patch("/me/portfolio/{id}", d.Profiles.PortfolioUpdate)
			r.Post("/me/portfolio/{id}/images", d.Profiles.PortfolioImagesAppend)
			r.Put("/me/portfolio/{id}/images/order", d.Profiles.PortfolioImagesReorder)
			r.Delete("/me/portfolio/{id}", d.Profiles.PortfolioDelete)
			r.Delete("/me/portfolio/images/{img_id}", d.Profiles.PortfolioImageDelete)

			r.Get("/me/leads/incoming", d.Leads.ListIncoming)
			r.Patch("/me/leads/{id}/recipient", d.Leads.UpdateRecipient)

			// CRM v5: клиентский кабинет «Мои проекты». RequireRoles не нужен —
			// эндпоинты сами фильтруют по client_user_id, доступ ограничен
			// своими проектами. Запрашивать может любой авторизованный, но
			// увидит только своё.
			if d.Projects != nil {
				r.Get("/me/projects", d.Projects.ClientList)
				r.Get("/me/projects/{id}/funnel", d.Projects.ClientGetFunnel)
				r.Get("/me/projects/{id}/comments", d.Projects.ClientListComments)
				r.Post("/me/projects/{id}/comments", d.Projects.ClientCreateComment)
				r.Get("/me/projects/{id}/comments/participants", d.Projects.ClientMentionCandidates)
				// Переписка креатора и исполнителя общего проекта. Ручки
				// зависят от d.Projects, а не от d.Publications: ветку даёт
				// ResolveCreatorThread, выкладки тут ни при чём.
				r.Get("/me/creator/projects/{id}/comments", d.Projects.CreatorListComments)
				r.Post("/me/creator/projects/{id}/comments", d.Projects.CreatorCreateComment)
				r.Get("/me/creator/projects/{id}/comments/participants", d.Projects.CreatorMentionCandidates)
				r.Post("/me/projects/{id}/steps/{step_id}/submit_review", d.Projects.ClientSubmitReview)
				r.Get("/me/specialist/projects", d.Projects.SpecialistList)
				r.Get("/me/specialist/projects/{id}/funnel", d.Projects.SpecialistGetFunnel)

				// Общий проект: клиент выбирает исполнителя и ставит срок,
				// исполнитель сдаёт, клиент принимает. Менеджерских ручек
				// тут нет — по требованиям менеджер в этом виде не участвует.
				r.Get("/me/general-projects", d.Projects.ClientListGeneral)
				r.Post("/me/general-projects", d.Projects.ClientCreateGeneral)
				r.Get("/me/general-projects/{id}", d.Projects.ClientGetGeneral)
				r.Post("/me/general-projects/{id}/accept", d.Projects.ClientAcceptDelivery)
				r.Post("/me/general-projects/{id}/rework", d.Projects.ClientReworkDelivery)
				r.Post("/me/general-projects/{id}/cancel", d.Projects.ClientCancelGeneral)

				r.Get("/me/specialist/general-projects", d.Projects.SpecialistListGeneral)
				r.Get("/me/specialist/general-projects/{id}", d.Projects.SpecialistGetGeneral)
				r.Post("/me/specialist/general-projects/{id}/deliver", d.Projects.SpecialistDeliver)
			}
			// Выкладки креатора. RequireRoles не нужен по той же причине,
			// что и у клиентских ручек выше: выдача сама фильтрует по
			// creator_user_id, и чужие выкладки в неё не попадают.
			if d.Publications != nil {
				r.Get("/me/creator/projects/{id}/publications", d.Publications.CreatorList)
				// Креатор заводит себе выкладку сам — чтобы добрать до
				// ступени, когда план периода уже выполнен. Согласования
				// менеджером нет намеренно.
				r.Post("/me/creator/projects/{id}/publications", d.Publications.CreatorAddPublication)
				r.Get("/me/creator/projects/{id}/checklist", d.Publications.CreatorChecklist)
				r.Post("/me/creator/publications/{pub_id}/links", d.Publications.CreatorSubmitLinks)
				r.Post("/me/creator/publications/{pub_id}/date_request", d.Publications.CreatorRequestDateChange)
				r.Get("/me/creator/projects/{id}/report", d.Publications.CreatorReport)
				// «В каких проектах я креатор» — до этого ответить было
				// нечем, и на страницу выкладок можно было попасть только
				// по прямой ссылке.
				r.Get("/me/creator/projects", d.Publications.CreatorProjects)
				r.Get("/me/creator/projects/{id}", d.Publications.CreatorProjectCard)
				r.Get("/me/creator/projects/{id}/materials", d.Publications.CreatorMaterials)

				// Взгляд клиента на проект. Здесь же, а не в блоке заказов:
				// это выкладки, и зависят они от d.Publications.
				r.Get("/me/projects/{id}/report", d.Publications.ClientReport)
				r.Get("/me/projects/{id}/videos", d.Publications.ClientVideos)
				r.Get("/me/projects/{id}/calendar", d.Publications.ClientCalendar)
				r.Get("/me/projects/{id}/notifications", d.Publications.ClientGetPrefs)
				r.Put("/me/projects/{id}/notifications", d.Publications.ClientSavePrefs)
				r.Get("/me/projects/{id}/materials", d.Publications.ClientMaterials)
				r.Get("/me/projects/{id}/report.csv", d.Publications.ClientReportCSV)
			}
			if d.Billing != nil {
				// Сводка по всем проектам заказчика: кросс-проектного
				// среза в продукте не было вовсе, и «сколько мне стоит
				// тысяча просмотров» внутри одного проекта не считается.
				r.Get("/me/overview", d.Billing.ClientOverviewHandler)
				// Деньги глазами тех, кто их платит и получает.
				r.Get("/me/projects/{id}/billing", d.Billing.ClientBilling)
				r.Get("/me/creator/projects/{id}/earnings", d.Billing.CreatorEarnings)
				// Смета заказа: точные оклады и прогноз бонуса по истории
				// той самой подборки.
				r.Get("/me/orders/{id}/estimate", d.Billing.ClientOrderEstimate)
				// Сумма на странице подбора: состав ещё собирают, заказа
				// нет, а показать стоимость надо уже сейчас.
				r.Post("/me/orders/estimate", d.Billing.ClientDraftEstimate)
			}
			if d.Orders != nil {
				// Подбор клиентом. RequireRoles не нужен: выдача сама
				// фильтрует по владельцу заказа и по приглашённому
				// креатору, чужого не покажет.
				r.Get("/me/orders/terms", d.Orders.ClientTerms)
				r.Post("/me/orders/terms/consent", d.Orders.ClientConsent)
				r.Get("/me/orders/limit", d.Orders.ClientLimit)
				r.Post("/me/orders/availability", d.Orders.ClientAvailability)
				r.Get("/me/orders", d.Orders.ClientListOrders)
				r.Post("/me/orders", d.Orders.ClientCreateOrder)
				r.Get("/me/orders/{id}", d.Orders.ClientGetOrder)
				r.Post("/me/orders/{id}/invite", d.Orders.ClientInvite)
				r.Post("/me/orders/{id}/cancel", d.Orders.ClientCancelOrder)
				// Порядок приоритета можно переставить, пока людей не
				// позвали: передумать после «отправить» — нормальная просьба.
				r.Put("/me/orders/{id}/priority", d.Orders.ClientReorderPriority)

				r.Get("/me/creator/invitations", d.Orders.CreatorInvitations)
				r.Post("/me/creator/invitations/{order_id}/respond", d.Orders.CreatorRespond)
				r.Get("/me/creator/availability", d.Orders.CreatorGetAvailability)
				r.Put("/me/creator/availability", d.Orders.CreatorSetAvailability)
			}
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.MiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation))
			r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
			r.Post("/reviews", d.Reviews.Create)
			r.Patch("/reviews/{id}", d.Reviews.Update)
			r.Delete("/reviews/{id}", d.Reviews.Delete)
		})

		// /manager/* — только role=manager AND is_approved.
		if d.AuthRepo != nil && d.Projects != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.MiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation))
				// Admin может ходить через manager-URL — effectiveOwnerID()
				// пропускает assigned_to-фильтр для роли admin.
				r.Use(auth.RequireRoles(d.AuthRepo, auth.RoleManager, auth.RoleAdmin))
				r.Use(RateLimit(d.Limiter, "crm", d.CRMWindows))

				r.Get("/manager/projects/inbox", d.Projects.ManagerInbox)
				r.Get("/manager/projects", d.Projects.ManagerListAssigned)
				r.Post("/manager/projects", d.Projects.ManagerCreateProject)
				r.Get("/manager/projects/{id}", d.Projects.ManagerGetFull)
				r.Patch("/manager/projects/{id}", d.Projects.ManagerPatch)
				r.Post("/manager/projects/{id}/claim", d.Projects.ManagerClaim)
				r.Post("/manager/projects/{id}/approve_specialist", d.Projects.ManagerApproveSpecialist)
				r.Post("/manager/projects/{id}/reject_specialist", d.Projects.ManagerRejectSpecialist)
				r.Post("/manager/projects/{id}/assign_specialist", d.Projects.ManagerAssignSpecialist)
				r.Post("/manager/projects/{id}/advance_stage", d.Projects.ManagerAdvanceStage)
				r.Post("/manager/projects/{id}/move_stage", d.Projects.ManagerMoveStage)
				r.Post("/manager/projects/{id}/move_step", d.Projects.ManagerMoveStep)
				if d.Admin != nil {
					r.Post("/manager/users/{id}/generate_invite", d.Admin.ManagerGenerateInvite)
					// Лукап юзеров (создать проект для существующего клиента,
					// назначить спеца). manager и admin — равные права.
					r.Get("/manager/users/search", d.Admin.AdminSearchUsers)
				}
				if d.Pipelines != nil {
					// Менеджеру нужен список воронок для селекта в форме
					// «создать проект». Read-only — редактировать может админ.
					r.Get("/manager/pipelines", d.Pipelines.AdminListPipelines)
				}
				r.Post("/manager/projects/{id}/steps/{step_id}/start", d.Projects.ManagerStartStep)
				r.Post("/manager/projects/{id}/steps/{step_id}/complete", d.Projects.ManagerCompleteStep)
				r.Post("/manager/projects/{id}/steps/{step_id}/skip", d.Projects.ManagerSkipStep)
				r.Get("/manager/projects/{id}/events", d.Projects.ManagerListEvents)
				r.Get("/manager/projects/{id}/comments", d.Projects.ManagerListComments)
				r.Post("/manager/projects/{id}/comments", d.Projects.ManagerCreateComment)
				r.Get("/manager/projects/{id}/comments/participants", d.Projects.ManagerMentionCandidates)

				if d.Publications != nil {
					r.Get("/manager/projects/{id}/publications", d.Publications.ManagerList)
					r.Post("/manager/projects/{id}/publications/preview", d.Publications.ManagerPreviewBatch)
					r.Post("/manager/projects/{id}/publications/batch", d.Publications.ManagerCreateBatch)
					r.Post("/manager/projects/{id}/publications/cancel_batch", d.Publications.ManagerCancelBatch)
					r.Post("/manager/publications/{pub_id}/close", d.Publications.ManagerClosePublication)
					r.Post("/manager/publications/{pub_id}/remind", d.Publications.ManagerRemindNow)
					r.Get("/manager/projects/{id}/report", d.Publications.ManagerReport)
					r.Get("/manager/projects/{id}/report.csv", d.Publications.ManagerReportCSV)

					// Состав проекта. Раньше был только POST и DELETE:
					// добавить креатора менеджер мог, а посмотреть, кто в
					// проекте, — нет.
					r.Get("/manager/projects/{id}/creators", d.Publications.ManagerListCreators)
					r.Post("/manager/projects/{id}/creators", d.Publications.ManagerAddCreator)
					r.Delete("/manager/projects/{id}/creators/{creator_id}", d.Publications.ManagerRemoveCreator)

					// Материалы проекта: бренд-гайд и обучение креаторам,
					// клиентские — заказчику.
					r.Get("/manager/projects/{id}/materials", d.Publications.ManagerListMaterials)
					r.Post("/manager/projects/{id}/materials", d.Publications.ManagerAddMaterial)
					r.Delete("/manager/projects/{id}/materials/{material_id}", d.Publications.ManagerDeleteMaterial)

					// Чеклист: снимок проекта и библиотека, из которой его берут.
					r.Get("/manager/projects/{id}/checklist", d.Publications.ManagerChecklist)
					r.Post("/manager/projects/{id}/checklist", d.Publications.ManagerSnapshotChecklist)
					r.Get("/manager/checklist_templates", d.Publications.ManagerChecklistTemplates)
					// Переключатели проекта: этап черновика и показ
					// статистики заказчику. Оба меняют работу, а не вид.
					r.Get("/manager/projects/{id}/settings", d.Publications.ManagerProjectSettings)
					r.Put("/manager/projects/{id}/settings", d.Publications.ManagerSaveProjectSettings)

					// Автопинг: какие напоминания бот шлёт по проекту сам.
					r.Get("/manager/projects/{id}/autoping", d.Publications.ManagerAutoping)
					r.Put("/manager/projects/{id}/autoping", d.Publications.ManagerSaveAutoping)

					r.Post("/manager/publication_date_requests/{req_id}/decide", d.Publications.ManagerDecideDateRequest)
				}
				if d.Billing != nil {
					// Деньги. Платёжного провайдера нет: подтверждение
					// получения и отметка выплаты — именные действия
					// менеджера, как переходы этапов в продакшн-проекте.
					r.Get("/manager/projects/{id}/billing", d.Billing.ManagerBilling)
					r.Put("/manager/projects/{id}/billing", d.Billing.ManagerSaveTerms)
					r.Post("/manager/projects/{id}/billing/adopt", d.Billing.ManagerAdoptTerms)
					r.Put("/manager/projects/{id}/payments/{kind}", d.Billing.ManagerSetPayment)
					r.Post("/manager/projects/{id}/payments/{kind}/confirm", d.Billing.ManagerConfirmPayment)
					r.Post("/manager/projects/{id}/accruals/recalc", d.Billing.ManagerRecalcAccruals)
					// Периоды проекта: катятся от первой публикации, а
					// подытоживаются сами через 14 дней после конца
					// (тикер в воркере). Ручного подытога нет намеренно.
					r.Get("/manager/projects/{id}/billing/periods", d.Billing.ManagerPeriods)
					r.Post("/manager/projects/{id}/accruals/{accrual_id}/approve", d.Billing.ManagerApproveAccrual)
					r.Post("/manager/projects/{id}/accruals/{accrual_id}/paid", d.Billing.ManagerPayAccrual)
					r.Put("/manager/projects/{id}/creators/{creator_id}/utm", d.Billing.ManagerSaveUTM)
					// Подписчики: сборщика по ним нет, число вписывает
					// менеджер — те же две ручки, что у меток.
					r.Get("/manager/projects/{id}/subscribers", d.Billing.ManagerSubscribers)
					r.Put("/manager/projects/{id}/creators/{creator_id}/subscribers",
						d.Billing.ManagerSaveSubscribers)
				}
				if d.Orders != nil {
					r.Get("/manager/orders", d.Orders.ManagerNeedingAttention)
					// Заказ по проекту: /manager/orders отдаёт только
					// застрявшие, а у проекта заказ давно оплачен.
					r.Get("/manager/projects/{id}/order", d.Orders.ManagerProjectOrder)
					r.Post("/manager/orders/{id}/candidates", d.Orders.ManagerAddCandidates)
					r.Post("/manager/orders/{id}/invite", d.Orders.ManagerInvite)
					r.Post("/manager/orders/{id}/paid", d.Orders.ManagerMarkPaid)
				}
			})
		}

		// /admin/* — только role=admin. AuthRepo обязателен (если не передан,
		// группа не маунтится — невозможно проверить роль).
		if d.AuthRepo != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.MiddlewareWithRevocation(d.TokenIssuer, d.AuthRevocation))
				r.Use(auth.RequireRoles(d.AuthRepo, auth.RoleAdmin))
				r.Use(RateLimit(d.Limiter, "crm", d.CRMWindows))

				if d.Productions != nil {
					r.Get("/admin/productions", d.Productions.AdminList)
					r.Post("/admin/productions", d.Productions.AdminCreate)
					r.Patch("/admin/productions/{id}", d.Productions.AdminPatch)
					r.Delete("/admin/productions/{id}", d.Productions.AdminDelete)
				}
				if d.Pipelines != nil {
					r.Get("/admin/pipelines", d.Pipelines.AdminListPipelines)
					r.Post("/admin/pipelines", d.Pipelines.AdminCreatePipeline)
					r.Get("/admin/pipelines/{id}", d.Pipelines.AdminGetPipeline)
					r.Patch("/admin/pipelines/{id}", d.Pipelines.AdminPatchPipeline)
					r.Delete("/admin/pipelines/{id}", d.Pipelines.AdminDeletePipeline)
					r.Post("/admin/pipelines/{id}/stages", d.Pipelines.AdminCreateStage)
					r.Patch("/admin/pipelines/stages/{id}", d.Pipelines.AdminPatchStage)
					r.Delete("/admin/pipelines/stages/{id}", d.Pipelines.AdminDeleteStage)
					r.Post("/admin/pipelines/stages/{id}/steps", d.Pipelines.AdminCreateStep)
					r.Patch("/admin/pipelines/steps/{id}", d.Pipelines.AdminPatchStep)
					r.Delete("/admin/pipelines/steps/{id}", d.Pipelines.AdminDeleteStep)
					r.Put("/admin/pipelines/{id}/reorder", d.Pipelines.AdminReorder)
					r.Post("/admin/pipelines/{id}/make_default", d.Pipelines.AdminMakeDefault)
				}
				if d.Admin != nil {
					r.Get("/admin/managers", d.Admin.AdminListManagers)
					r.Post("/admin/managers/promote", d.Admin.AdminPromoteToManager)
					r.Post("/admin/managers/{id}/approve", d.Admin.AdminApproveManager)
					r.Post("/admin/managers/{id}/revoke", d.Admin.AdminRevokeManager)
					r.Get("/admin/users", d.Admin.AdminListAllUsers)
					r.Post("/admin/users", d.Admin.AdminCreateClient)
					r.Post("/admin/users/{id}/generate_invite", d.Admin.AdminGenerateInvite)
					// Ссылка для входа сотруднику. Только здесь, в
					// админской секции: цель может быть менеджером или
					// админом, и под ролью manager такой ручки быть не
					// должно (data-sec D1 — см. internal/admin/login_link.go).
					r.Post("/admin/users/{id}/login_link", d.Admin.AdminStaffLoginLink)
					r.Post("/admin/users/{id}/verify_email", d.Admin.AdminVerifyEmail)
					r.Post("/admin/users/{id}/deactivate", d.Admin.AdminDeactivateUser)
					r.Post("/admin/users/{id}/activate", d.Admin.AdminActivateUser)
					r.Get("/admin/users/search", d.Admin.AdminSearchUsers)
					// {id} после /search: chi разводит их сам, но читателю
					// порядок подсказывает, что «search» — не uuid.
					r.Get("/admin/users/{id}", d.Admin.AdminGetUser)
					r.Post("/admin/users/{id}/mark_test", d.Admin.AdminMarkUserTest)

					// Сводка, журнал, команда и ⌘K-поиск — оболочка админки.
					r.Get("/admin/summary", d.Admin.AdminSummary)
					r.Get("/admin/team", d.Admin.AdminListTeam)
					r.Get("/admin/audit", d.Admin.AdminListAudit)
					r.Get("/admin/search", d.Admin.AdminGlobalSearch)

					// Модерация публикаций специалистов — docs/SPECIALIST_MODERATION.md
					r.Get("/admin/moderation/specialists", d.Admin.AdminListPendingSpecialists)
					r.Get("/admin/moderation/specialists/count", d.Admin.AdminPendingModerationCount)
					r.Get("/admin/moderation/specialists/{id}", d.Admin.AdminGetSpecialistForModeration)
					r.Post("/admin/moderation/specialists/{id}/approve", d.Admin.AdminApproveSpecialist)
					r.Post("/admin/moderation/specialists/{id}/reject", d.Admin.AdminRejectSpecialist)
				}
				if d.Projects != nil {
					r.Get("/admin/projects", d.Projects.AdminListProjects)
					r.Post("/admin/projects", d.Projects.AdminCreateProject)
					r.Get("/admin/projects/{id}", d.Projects.AdminGetProject)
					r.Delete("/admin/projects/{id}", d.Projects.AdminCancelProject)
					r.Post("/admin/projects/{id}/advance_stage", d.Projects.AdminAdvanceStage)
					r.Post("/admin/projects/{id}/move_stage", d.Projects.AdminMoveStage)
					r.Post("/admin/projects/{id}/move_step", d.Projects.AdminMoveStep)
					r.Post("/admin/projects/{id}/change_funnel", d.Projects.AdminChangeFunnel)
					r.Post("/admin/projects/{id}/restore", d.Projects.AdminRestoreProject)
					r.Post("/admin/projects/{id}/mark_test", d.Projects.AdminMarkProjectTest)
					r.Post("/admin/projects/{id}/assign", d.Projects.AdminAssignManager)
					// Передача проектов уходящего менеджера. Живёт рядом с
					// проектами, а не с пользователями: событие и outbox на
					// каждый проект те же, что у одиночного назначения.
					r.Post("/admin/managers/{id}/transfer_projects", d.Projects.AdminTransferProjects)
					r.Post("/admin/projects/{id}/assign_specialist", d.Projects.AdminAssignSpecialist)
					r.Get("/admin/projects/{id}/events", d.Projects.AdminListProjectEvents)
					r.Get("/admin/projects/{id}/comments", d.Projects.AdminListProjectComments)
					r.Post("/admin/projects/{id}/comments", d.Projects.AdminCreateProjectComment)
				}

				// Библиотека чеклистов. Правит её админ: пункты
				// одинаковы для всех проектов, и держать их у каждого
				// менеджера своим набором — значит спрашивать с
				// креаторов разное за одну и ту же работу.
				if d.Publications != nil {
					r.Get("/admin/checklist_templates", d.Publications.AdminListChecklistTemplates)
					r.Post("/admin/checklist_templates", d.Publications.AdminSaveChecklistTemplate)
					r.Get("/admin/checklist_templates/{id}", d.Publications.AdminGetChecklistTemplate)
					r.Delete("/admin/checklist_templates/{id}", d.Publications.AdminDeleteChecklistTemplate)
				}

				// Прайс площадки: сколько платит клиент за креатора и
				// сколько из этого получает сам креатор. Правится только
				// выпуском новой версии — под старой стоит согласие
				// клиентов, и переписывать её задним числом нельзя.
				// Справочник порогов оценок: версионируется как прайс,
				// правится только выпуском новой версии.
				if d.Ratings != nil {
					r.Get("/admin/rating_scales", d.Ratings.AdminListScales)
					r.Get("/admin/rating_scales/current", d.Ratings.AdminCurrentScale)
					r.Post("/admin/rating_scales", d.Ratings.AdminPublishScale)
				}
				if d.Billing != nil {
					r.Get("/admin/terms", d.Billing.AdminListTerms)
					r.Post("/admin/terms", d.Billing.AdminPublishTerms)
					// Переоткрытие периода — только админ и только со
					// следом в журнале: оно переписывает историю
					// расчёта.
					r.Post("/admin/projects/{id}/billing/unlock_period", d.Billing.AdminUnlockPeriod)
				}
			})
		}
	})

	r.Get("/swagger/*", httpSwagger.Handler(httpSwagger.URL("/swagger/doc.json")))

	return r
}
