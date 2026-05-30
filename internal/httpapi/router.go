package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"marketpclce/internal/auth"
	"marketpclce/internal/catalog"
	"marketpclce/internal/clarify"
	"marketpclce/internal/feed"
	"marketpclce/internal/httpapi/handlers"
	"marketpclce/internal/leads"
	"marketpclce/internal/productions"
	"marketpclce/internal/profilecheck"
	"marketpclce/internal/profiles"
	"marketpclce/internal/projects"
	"marketpclce/internal/ratelimit"
	"marketpclce/internal/reviews"
	"marketpclce/internal/search"
	"marketpclce/internal/summarize"
)

type Deps struct {
	Logger       *slog.Logger
	HealthDB     handlers.HealthDB
	TokenIssuer  *auth.TokenIssuer
	Auth         *auth.Handler
	Catalog      *catalog.Handler
	Profiles     *profiles.Handler
	ProfileCheck *profilecheck.Handler
	Search       *search.Handler
	Feed         *feed.Handler
	Summarize    *summarize.Handler
	Clarify      *clarify.Handler
	Leads        *leads.Handler
	Reviews      *reviews.Handler
	Productions  *productions.Handler
	Projects     *projects.Handler

	// RoleLookup читает users.role для access-токенов в /admin/*.
	// nil → /admin/* недоступны (middleware вернёт 500 role_lookup_unavailable).
	RoleLookup auth.RoleLookup

	CORSOrigins []string

	Limiter          *ratelimit.Limiter
	ReadWindows      []ratelimit.Window
	LeadsWindows     []ratelimit.Window
	ClarifyWindows   []ratelimit.Window
	AuthWindows      []ratelimit.Window
	SummarizeWindows []ratelimit.Window
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
		if d.Productions != nil {
			r.Get("/productions", d.Productions.List)
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

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(d.TokenIssuer))
			r.Get("/me", d.Auth.Me)
			r.Post("/auth/resend-verification", d.Auth.ResendVerification)
			r.Get("/me/profile", d.Profiles.Get)
			r.Patch("/me/profile", d.Profiles.PatchFull)
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
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(d.TokenIssuer))
			r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
			r.Post("/reviews", d.Reviews.Create)
			r.Patch("/reviews/{id}", d.Reviews.Update)
			r.Delete("/reviews/{id}", d.Reviews.Delete)
		})

		// /me/projects/* — ЛК клиента. RequireRoles("client") вместо
		// обычного auth.Middleware: специалисты ходят в свой раздел
		// /me/specialist/projects/*, у них своя выдача (Фаза 3).
		if d.Projects != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRoles(d.TokenIssuer, d.RoleLookup, "client"))
				r.Use(RateLimit(d.Limiter, "read", d.ReadWindows))
				r.Get("/me/projects", d.Projects.ListClient)
				r.Get("/me/projects/{id}", d.Projects.GetClient)
				r.Get("/me/projects/{id}/funnel", d.Projects.GetClientFunnel)
				r.Get("/me/projects/by_lead/{lead_id}", d.Projects.ListClientByLead)
			})
		}

		// /admin/* — разделён по уровню действия:
		//   GET           → admin + moderator (read для наблюдения/аудита)
		//   POST/PATCH/DELETE → только admin (writes по дизайну
		//     single-editor, чтобы исключить lost-update через Directus UI).
		// Rate-limit здесь не вешаем: вызовы служебные, объёмы малы.
		if d.Productions != nil {
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRoles(d.TokenIssuer, d.RoleLookup, "admin", "moderator"))
				r.Get("/admin/productions", d.Productions.AdminList)
				r.Get("/admin/productions/{id}", d.Productions.AdminGet)
			})
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRoles(d.TokenIssuer, d.RoleLookup, "admin"))
				r.Post("/admin/productions", d.Productions.AdminCreate)
				r.Patch("/admin/productions/{id}", d.Productions.AdminUpdate)
				r.Delete("/admin/productions/{id}", d.Productions.AdminDeactivate)
			})
		}
	})

	r.Get("/swagger/*", httpSwagger.Handler(httpSwagger.URL("/swagger/doc.json")))

	return r
}
