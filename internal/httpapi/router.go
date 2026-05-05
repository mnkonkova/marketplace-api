package httpapi

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"marketpclce/internal/auth"
	"marketpclce/internal/catalog"
	"marketpclce/internal/clarify"
	"marketpclce/internal/httpapi/handlers"
	"marketpclce/internal/leads"
	"marketpclce/internal/profilecheck"
	"marketpclce/internal/profiles"
	"marketpclce/internal/ratelimit"
	"marketpclce/internal/search"
	"marketpclce/internal/summarize"
)

type Deps struct {
	Logger      *slog.Logger
	HealthDB    handlers.HealthDB
	TokenIssuer *auth.TokenIssuer
	Auth        *auth.Handler
	Catalog     *catalog.Handler
	Profiles     *profiles.Handler
	ProfileCheck *profilecheck.Handler
	Search       *search.Handler
	Summarize   *summarize.Handler
	Clarify     *clarify.Handler
	Leads       *leads.Handler

	WebDir string

	Limiter      *ratelimit.Limiter
	ReadWindows  []ratelimit.Window
	LeadsWindows []ratelimit.Window
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(120 * time.Second))
	r.Use(slogRequestLogger(d.Logger))

	health := handlers.NewHealth(d.HealthDB)
	r.Get("/healthz", health.Live)
	r.Get("/readyz", health.Ready)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/register", d.Auth.Register)
		r.Post("/auth/login", d.Auth.Login)
		r.Post("/auth/refresh", d.Auth.Refresh)

		r.Get("/categories", d.Catalog.Categories)
		r.Get("/skills", d.Catalog.Skills)

		r.Group(func(r chi.Router) {
			r.Use(RateLimit(d.Limiter, "read", d.ReadWindows))
			r.Get("/specialists", d.Search.Search)
			r.Get("/specialists/{id}", d.Profiles.Public)
			r.Get("/search", d.Search.Search)
			r.Get("/categories/stats", d.Search.CategoryStats)
		})

		r.Post("/search/summarize", d.Summarize.Summarize)
		if d.Clarify != nil {
			r.Post("/clarify", d.Clarify.Clarify)
		}

		r.Group(func(r chi.Router) {
			r.Use(auth.OptionalMiddleware(d.TokenIssuer))
			r.Use(RateLimit(d.Limiter, "leads", d.LeadsWindows))
			r.Post("/leads", d.Leads.Create)
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(d.TokenIssuer))
			r.Get("/me", d.Auth.Me)
			r.Get("/me/profile", d.Profiles.Get)
			r.Patch("/me/profile", d.Profiles.Patch)
			r.Put("/me/profile/categories", d.Profiles.SetCategories)
			r.Put("/me/profile/skills", d.Profiles.SetSkills)
			r.Post("/me/profile/publish", d.Profiles.Publish)
			r.Post("/me/profile/unpublish", d.Profiles.Unpublish)
			if d.ProfileCheck != nil {
				r.Post("/me/profile/check", d.ProfileCheck.Check)
			}

			r.Get("/me/leads/incoming", d.Leads.ListIncoming)
			r.Patch("/me/leads/{id}/recipient", d.Leads.UpdateRecipient)
		})
	})

	if d.WebDir != "" {
		mountStatic(r, d.WebDir)
	}

	return r
}

func mountStatic(r chi.Router, dir string) {
	fs := http.FileServer(http.Dir(dir))
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFile(w, req, dir+"/index.html")
	})
	r.Handle("/styles.css", fs)
	r.Handle("/app.js", fs)
	r.Handle("/favicon.ico", fs)
	r.Handle("/favicon.svg", fs)
	// chi с wildcard передаёт FileServer-у обрезанный путь, поэтому модули
	// фронта мы поднимаем отдельными FileServer'ами с явным StripPrefix.
	r.Handle("/shared/*", http.StripPrefix("/shared/", http.FileServer(http.Dir(filepath.Join(dir, "shared")))))
	r.Handle("/pages/*", http.StripPrefix("/pages/", http.FileServer(http.Dir(filepath.Join(dir, "pages")))))

	// SPA fallback: GET вне /api/* отдаёт index.html, чтобы прямой переход
	// на /search или /specialist/abc работал, а не падал в 404.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || strings.HasPrefix(req.URL.Path, "/api/") {
			http.NotFound(w, req)
			return
		}
		http.ServeFile(w, req, dir+"/index.html")
	})
}
