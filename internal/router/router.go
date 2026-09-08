// Package router assembles the HTTP routes.
//
// One file, so the whole surface of the service can be read at once. A route
// table split across packages is how an endpoint ends up outside the
// middleware everyone assumed was in front of it.
package router

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/handler"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// Handlers is everything the router mounts.
type Handlers struct {
	Setup     *handler.SetupHandler
	Auth      *handler.AuthHandler
	Lang      *handler.LangHandler
	Docs      *handler.DocsHandler
	Dashboard *handler.DashboardHandler
	Jobs      *handler.JobHandler
	Runs      *handler.RunHandler
	Projects  *handler.ProjectHandler
	Settings  *handler.SettingsHandler
	API       *handler.APIHandler
	Health    *handler.HealthHandler
}

// New builds the route table.
//
// proxy decides which address the rate limiters attribute a request to. Its
// zero value trusts no forwarding header and uses the socket, which is the
// right default for a service installed on somebody's own VM.
func New(h Handlers, mw *middleware.Set, proxy auth.TrustedProxy, log *applog.Logger) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	// chimw.RealIP is deliberately NOT mounted. It overwrites RemoteAddr from
	// the leftmost X-Forwarded-For entry with no notion of a trusted proxy, and
	// the leftmost entry is whatever the caller sent. Mounted here it would
	// hand every rate-limited route a bucket key the attacker picks, which is
	// the same as having no limit. The client address comes from
	// auth.TrustedProxy instead, which trusts nothing unless configured to.
	r.Use(chimw.Logger)

	// The order of these two is load bearing and reads backwards. r.Use(A);
	// r.Use(B) builds A(B(handler)), so a panic unwinds through B first, and
	// chi's Recoverer recovers without re-panicking. PanicLogger has to be the
	// INNER one: it records the panic, re-panics, and Recoverer writes the
	// 500. Swap them and every production crash is invisible outside stdout.
	r.Use(chimw.Recoverer)
	r.Use(middleware.PanicLogger(log))

	r.Use(middleware.SecurityHeaders)
	// A request that has not finished in a minute is not going to. The cap is
	// on the HTTP layer only; a job's own execution runs on the worker pool
	// with its own, much longer, budget.
	r.Use(chimw.Timeout(60 * time.Second))

	// The static files and the probes sit OUTSIDE the setup gate. The setup
	// screen needs the stylesheet to be legible, and an orchestrator asking
	// whether the process is alive must get an answer rather than a redirect to
	// a form.
	r.Handle("/static/*", handler.StaticHandler())

	r.Get("/healthz", h.Health.Live)
	r.Get("/readyz", h.Health.Ready)

	// Everything else is behind the gate: until an account exists, the only
	// reachable screen is the one that creates it.
	//
	// A Group rather than another r.Use, and not a matter of taste: chi refuses
	// Use once a route has been registered on the mux, and the three routes
	// above have to stay outside the gate.
	r.Group(func(r chi.Router) {
		r.Use(mw.SetupGate)
		mount(r, h, mw, proxy)
	})

	return r
}

// mount registers everything that sits behind the setup gate.
func mount(r chi.Router, h Handlers, mw *middleware.Set, proxy auth.TrustedProxy) {
	// The first run. Rate limited and origin checked like the login form, and
	// the gate makes it a 404 the moment an account exists.
	setupLimiter := auth.NewLimiter(10, 5*time.Minute)
	r.Group(func(r chi.Router) {
		r.Use(middleware.Language)
		r.Get(middleware.SetupPath, h.Setup.Show)
		r.With(middleware.SameOrigin, middleware.RateLimit(setupLimiter, proxy,
			"Too many attempts. Try again in a few minutes.")).
			Post(middleware.SetupPath, h.Setup.Save)
	})

	// The interface language is resolved for the SCREENS only, and deliberately
	// not for /api/v1. An API error that changes wording with an Accept-Language
	// header is one nobody can grep for, so the API stays English by never
	// having a language on its context.
	//
	// Switching language is a POST so a prefetch cannot change it, and it sits
	// outside the auth group so it works on the login screen too.
	r.With(middleware.Language, middleware.SameOrigin).Post("/lang", h.Lang.Set)

	// Sign in. Rate limited by client address: without it the login form is an
	// unlimited password oracle.
	loginLimiter := auth.NewLimiter(10, 5*time.Minute)
	// The reset routes get a limit of their own, and a tighter one. Each
	// request sends mail to an address the caller named, so an unbounded form
	// is a way to have this deployment post to somebody else's inbox.
	resetLimiter := auth.NewLimiter(5, 15*time.Minute)
	r.Group(func(r chi.Router) {
		r.Use(middleware.Language)
		r.Use(mw.GuestOnly)
		r.Get("/login", h.Auth.LoginPage)
		r.With(middleware.RateLimit(loginLimiter, proxy, "Too many attempts. Try again in a few minutes.")).
			Post("/login", h.Auth.Login)

		r.Get("/forgot", h.Auth.ForgotPage)
		r.With(middleware.SameOrigin, middleware.RateLimit(resetLimiter, proxy,
			"Too many attempts. Try again in a few minutes.")).
			Post("/forgot", h.Auth.Forgot)

		r.Get("/reset", h.Auth.ResetPage)
		r.With(middleware.SameOrigin, middleware.RateLimit(resetLimiter, proxy,
			"Too many attempts. Try again in a few minutes.")).
			Post("/reset", h.Auth.Reset)
	})

	// The interface. Every route below requires a signed in operator, and
	// every state changing one is additionally checked for its origin.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Language)
		r.Use(mw.WebAuth)
		r.Use(mw.Permissions)
		r.Use(middleware.SameOrigin)

		r.Get("/", h.Dashboard.Show)
		r.Post("/logout", h.Auth.Logout)
		r.Get("/profile", h.Auth.Profile)
		r.Post("/profile/password", h.Auth.ChangePassword)

		// The API reference. Behind sign in, like everything else here: the
		// spec is not a secret, but there is no reason for a deployment to
		// publish its own API surface to whoever finds the host.
		r.Get("/docs", h.Docs.Page)
		r.Get("/openapi.yaml", h.Docs.Spec)

		r.Route("/jobs", func(r chi.Router) {
			r.Get("/", h.Jobs.List)
			r.Get("/new", h.Jobs.Form)
			r.Post("/", h.Jobs.Save)
			r.Get("/schedule-preview", h.Jobs.PreviewSchedule)

			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", h.Jobs.Form)
				r.Post("/", h.Jobs.Save)
				r.Post("/toggle", h.Jobs.Toggle)
				r.Post("/run", h.Jobs.Run)
				r.Post("/clone", h.Jobs.Clone)
				r.Post("/delete", h.Jobs.Delete)
				r.Post("/schedules", h.Jobs.AddSchedule)
				r.Post("/schedules/{scheduleID}/delete", h.Jobs.RemoveSchedule)
				r.Post("/links", h.Jobs.AddLink)
				r.Post("/links/{linkID}/delete", h.Jobs.RemoveLink)
			})
		})

		r.Route("/runs", func(r chi.Router) {
			r.Get("/", h.Runs.List)
			r.Get("/{id}", h.Runs.Detail)
		})

		r.Route("/projects", func(r chi.Router) {
			r.Get("/", h.Projects.List)
			r.Post("/", h.Projects.Save)
			r.Post("/{id}", h.Projects.Save)
			r.Post("/{id}/rotate-key", h.Projects.RotateKey)
			r.Post("/{id}/delete", h.Projects.Delete)

			// Who may reach the project. Delegated on purpose: a project
			// administrator adds their own colleagues without an account on
			// the settings screen, which is where the platform-wide switches
			// are.
			r.Get("/{id}/members", h.Projects.Members)
			r.Post("/{id}/members", h.Projects.AddMember)
			r.Post("/{id}/members/{userID}/{roleID}/delete", h.Projects.RemoveMember)
		})

		// Settings changes accounts and alerting, so it is administrators only.
		r.Route("/settings", func(r chi.Router) {
			r.Use(mw.AdminOnly)
			r.Get("/", h.Settings.Show)
			r.Post("/users", h.Settings.SaveUser)
			r.Post("/users/{id}", h.Settings.SaveUser)
			r.Post("/users/{id}/delete", h.Settings.DeleteUser)
			r.Post("/roles/run-visibility", h.Settings.SaveRunVisibility)
			r.Post("/notifications", h.Settings.SaveNotification)
			r.Post("/notifications/{id}", h.Settings.SaveNotification)
			r.Post("/notifications/{id}/delete", h.Settings.DeleteNotification)

			// Where a hostname is dialled. Administrator only and never a
			// project role: an arbitrary host-to-address map can point a
			// permitted host anywhere.
			r.Post("/hosts", h.Settings.SaveHostOverride)
			r.Post("/hosts/{id}", h.Settings.SaveHostOverride)
			r.Post("/hosts/{id}/delete", h.Settings.DeleteHostOverride)
		})
	})

	// The machine API. Project scoped, authenticated by API key. This is how a
	// project registers its own jobs from a deploy pipeline.
	syncLimiter := auth.NewLimiter(60, time.Minute)
	apiLoginLimiter := auth.NewLimiter(10, 5*time.Minute)
	r.Route("/api/v1", func(r chi.Router) {
		// Issuing a token is the one API route that cannot require a token.
		// Rate limited like the form, and for the same reason: without it this
		// is an unlimited password oracle.
		r.With(middleware.JSONOnly, middleware.RateLimit(apiLoginLimiter, proxy, "too many attempts")).
			Post("/login", h.Auth.APILogin)

		r.Group(func(r chi.Router) {
			r.Use(mw.ProjectKey)
			r.Use(middleware.RateLimit(syncLimiter, proxy, "rate limit exceeded"))
			r.With(middleware.JSONOnly).Post("/sync", h.API.Sync)
			r.Get("/jobs", h.API.ProjectJobs)
			r.Post("/jobs/{code}/run", h.API.ProjectTrigger)
			r.Get("/runs", h.API.ProjectRuns)
		})

		// Operator scoped, authenticated by session token. For scripting
		// against the whole service rather than one project.
		r.Route("/admin", func(r chi.Router) {
			r.Use(mw.APIAuth)
			r.Use(mw.Permissions)
			r.Get("/me", h.API.Me)
			r.Get("/summary", h.API.Summary)
			r.Get("/schedule-preview", h.API.PreviewSchedule)
			r.Get("/projects", h.API.Projects)

			r.Get("/jobs", h.API.Jobs)
			r.With(middleware.JSONOnly).Post("/jobs", h.API.CreateJob)
			r.Get("/jobs/{id}", h.API.Job)
			r.With(middleware.JSONOnly).Put("/jobs/{id}", h.API.UpdateJob)
			r.Delete("/jobs/{id}", h.API.DeleteJob)
			r.Post("/jobs/{id}/run", h.API.TriggerJob)

			r.Get("/runs", h.API.Runs)
		})
	})

	// An unknown API path answers JSON; an unknown page goes to the dashboard.
	// Answering a missing API route with an HTML redirect is how a client ends
	// up parsing a login page as its payload.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if len(req.URL.Path) >= 5 && req.URL.Path[:5] == "/api/" {
			httpx.Fail(w, http.StatusNotFound, "not found")
			return
		}
		http.Redirect(w, req, "/", http.StatusSeeOther)
	})
}
