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
	"marketpclce/internal/catalog"
	"marketpclce/internal/clarify"
	"marketpclce/internal/feed"
	"marketpclce/internal/httpapi/handlers"
	"marketpclce/internal/leads"
	"marketpclce/internal/pipelines"
	"marketpclce/internal/productions"
	"marketpclce/internal/profilecheck"
	"marketpclce/internal/projects"
	"marketpclce/internal/profiles"
	"marketpclce/internal/ratelimit"
	"marketpclce/internal/reviews"
	"marketpclce/internal/search"
	"marketpclce/internal/summarize"
)

type Deps struct {
	Logger      *slog.Logger
	HealthDB    handlers.HealthDB
	TokenIssuer *auth.TokenIssuer
	Auth        *auth.Handler
	AuthRepo    auth.IdentityLoader
	Catalog     *catalog.Handler
	Profiles     *profiles.Handler
	ProfileCheck *profilecheck.Handler
	Search       *search.Handler
	Feed         *feed.Handler
	Summarize   *summarize.Handler
	Clarify     *clarify.Handler
	Leads       *leads.Handler
	Reviews     *reviews.Handler
	Productions *productions.Handler
	Pipelines   *pipelines.Handler
	Projects    *projects.Handler
	Admin       *admin.Handler

	CORSOrigins []string

	Limiter          *ratelimit.Limiter
	ReadWindows      []ratelimit.Window
	LeadsWindows     []ratelimit.Window
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

	r.Route("/api/v1", func(r chi.Router) {
		// /auth/* — анти-брутфорс по IP. register/login/refresh/verify-email
		// без auth, поэтому ключ — только IP. Логин/регистрация на одной
		// корзине: 10 попыток в минуту суммарно достаточно для живого юзера
		// и душит автоматизированный перебор.
		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, "auth", d.AuthWindows))
			r.Post("/auth/register", d.Auth.Register)
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

		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, "read", d.ReadWindows))
			r.Get("/specialists/{id}", d.Profiles.Public)
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
			r.Use(auth.OptionalMiddleware(d.TokenIssuer))
			r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
			r.Post("/leads", d.Leads.Create)
		})

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
			r.Use(auth.Middleware(d.TokenIssuer))
			r.Get("/me", d.Auth.Me)
			// Pipelines читаются всеми залогиненными — нужен и менеджеру (для
			// канбана), и админу. Запись по-прежнему только под admin.
			if d.Pipelines != nil {
				r.Get("/pipelines/{id}", d.Pipelines.AdminGetPipeline)
			}
			r.Post("/auth/resend-verification", d.Auth.ResendVerification)
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
			r.Post("/me/portfolio/upload-url", d.Profiles.PortfolioUploadURL)
			r.Put("/me/portfolio/{id}/categories", d.Profiles.PortfolioSetCategories)
			r.Delete("/me/portfolio/{id}", d.Profiles.PortfolioDelete)

			// Аплоад картинки (аватар / превью к видео) — общий presigned PUT.
			r.Post("/me/uploads/image", d.Profiles.ImageUploadURL)

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
				r.Post("/me/projects/{id}/steps/{step_id}/submit_review", d.Projects.ClientSubmitReview)
				r.Get("/me/specialist/projects", d.Projects.SpecialistList)
				r.Get("/me/specialist/projects/{id}/funnel", d.Projects.SpecialistGetFunnel)
			}
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(d.TokenIssuer))
			r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
			r.Post("/reviews", d.Reviews.Create)
			r.Patch("/reviews/{id}", d.Reviews.Update)
			r.Delete("/reviews/{id}", d.Reviews.Delete)
		})

		// /manager/* — только role=manager AND is_approved.
		if d.AuthRepo != nil && d.Projects != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.Middleware(d.TokenIssuer))
				// Admin может ходить через manager-URL — effectiveOwnerID()
				// пропускает assigned_to-фильтр для роли admin.
				r.Use(auth.RequireRoles(d.AuthRepo, auth.RoleManager, auth.RoleAdmin))
				r.Use(RateLimit(d.Limiter, "crm", d.CRMWindows))

				r.Get("/manager/projects/inbox", d.Projects.ManagerInbox)
				r.Get("/manager/projects", d.Projects.ManagerListAssigned)
				r.Get("/manager/projects/{id}", d.Projects.ManagerGetFull)
				r.Patch("/manager/projects/{id}", d.Projects.ManagerPatch)
				r.Post("/manager/projects/{id}/claim", d.Projects.ManagerClaim)
				r.Post("/manager/projects/{id}/advance_stage", d.Projects.ManagerAdvanceStage)
				r.Post("/manager/projects/{id}/move_stage", d.Projects.ManagerMoveStage)
				r.Post("/manager/projects/{id}/move_step", d.Projects.ManagerMoveStep)
				if d.Admin != nil {
					r.Post("/manager/users/{id}/generate_invite", d.Admin.ManagerGenerateInvite)
				}
				r.Post("/manager/projects/{id}/steps/{step_id}/start", d.Projects.ManagerStartStep)
				r.Post("/manager/projects/{id}/steps/{step_id}/complete", d.Projects.ManagerCompleteStep)
				r.Post("/manager/projects/{id}/steps/{step_id}/skip", d.Projects.ManagerSkipStep)
				r.Get("/manager/projects/{id}/events", d.Projects.ManagerListEvents)
				r.Get("/manager/projects/{id}/comments", d.Projects.ManagerListComments)
				r.Post("/manager/projects/{id}/comments", d.Projects.ManagerCreateComment)
			})
		}

		// /admin/* — только role=admin. AuthRepo обязателен (если не передан,
		// группа не маунтится — невозможно проверить роль).
		if d.AuthRepo != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.Middleware(d.TokenIssuer))
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
					r.Post("/admin/managers/{id}/approve", d.Admin.AdminApproveManager)
					r.Post("/admin/managers/{id}/revoke", d.Admin.AdminRevokeManager)
					r.Post("/admin/users", d.Admin.AdminCreateClient)
					r.Post("/admin/users/{id}/generate_invite", d.Admin.AdminGenerateInvite)
				}
				if d.Projects != nil {
					r.Get("/admin/projects", d.Projects.AdminListProjects)
					r.Post("/admin/projects", d.Projects.AdminCreateProject)
					r.Get("/admin/projects/{id}", d.Projects.AdminGetProject)
					r.Post("/admin/projects/{id}/advance_stage", d.Projects.AdminAdvanceStage)
					r.Post("/admin/projects/{id}/move_stage", d.Projects.AdminMoveStage)
					r.Post("/admin/projects/{id}/move_step", d.Projects.AdminMoveStep)
					r.Post("/admin/projects/{id}/assign", d.Projects.AdminAssignManager)
					r.Get("/admin/projects/{id}/events", d.Projects.AdminListProjectEvents)
					r.Get("/admin/projects/{id}/comments", d.Projects.AdminListProjectComments)
					r.Post("/admin/projects/{id}/comments", d.Projects.AdminCreateProjectComment)
				}
			})
		}
	})

	r.Get("/swagger/*", httpSwagger.Handler(httpSwagger.URL("/swagger/doc.json")))

	return r
}
