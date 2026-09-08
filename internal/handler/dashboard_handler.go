package handler

import (
	"net/http"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/hostinfo"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// DashboardHandler serves the front page.
type DashboardHandler struct {
	guard
	stats  *service.StatsService
	host   *hostinfo.Reader
	render *Renderer
	log    *applog.Logger
}

// NewDashboardHandler wires the handler.
//
// host may be nil, in which case the machine panel is simply absent.
func NewDashboardHandler(a *authz.Service, stats *service.StatsService, host *hostinfo.Reader, render *Renderer, log *applog.Logger) *DashboardHandler {
	return &DashboardHandler{guard: guard{authz: a}, stats: stats, host: host, render: render, log: log}
}

// Show renders the dashboard.
//
// The first thing on it is the dispatcher's pulse, not the job counts. If the
// dispatcher is dead every other number on the page is a description of the
// past, and a screen that leads with those reads as calm.
func (h *DashboardHandler) Show(w http.ResponseWriter, r *http.Request) {
	hours := httpx.QueryInt(r, "hours", 24)
	if hours < 1 || hours > 168 {
		hours = 24
	}

	scope, ok := h.scope(w, r, authz.JobsRead)
	if !ok {
		return
	}

	board, err := h.stats.Build(r.Context(), scope, hours)
	if err != nil {
		h.log.Error("dashboard: could not be built", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		h.render.Render(w, r, "error", PageData{
			Title: "Error",
			Flash: &Flash{Kind: "danger", Message: tr(r, "The dashboard could not be loaded.")},
		})
		return
	}

	data := map[string]any{
		"board": board,
		"hours": hours,
		"can":   h.permissions(r),
	}

	// The machine the console is installed on, for whoever administers it.
	//
	// Deliberately not scoped like everything else on this page: the host is
	// not project data, and a team given a role on one project is not being
	// given a view of the server's memory. Absent rather than empty where there
	// is nothing to read, because a row of zeros reads as "all is well".
	if h.host != nil {
		if user := h.user(r); user != nil && user.IsAdmin {
			if stats := h.host.Read(); stats.Supported {
				data["host"] = stats
			}
		}
	}

	// The dashboard refreshes itself on a timer. Only the body is swapped, so
	// a screen left open on a wall does not reload the whole document every
	// thirty seconds.
	if httpx.IsHTMX(r) {
		h.render.Partial(w, r, "dashboard", "dashboard-body", data)
		return
	}
	h.render.Render(w, r, "dashboard", PageData{Title: "Dashboard", Active: "dashboard", Data: data})
}
