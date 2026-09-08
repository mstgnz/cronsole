package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// SettingsHandler serves the administrator screens: accounts, notification
// lists and the application log.
type SettingsHandler struct {
	auth          *service.AuthService
	notifications *service.NotificationService
	members       *service.MemberService
	hosts         *service.HostOverrideService
	logs          domain.AppLogRepository
	render        *Renderer
	log           *applog.Logger
}

// NewSettingsHandler wires the handler.
func NewSettingsHandler(authService *service.AuthService, notifications *service.NotificationService,
	members *service.MemberService, hosts *service.HostOverrideService, logs domain.AppLogRepository,
	render *Renderer, log *applog.Logger) *SettingsHandler {
	return &SettingsHandler{auth: authService, notifications: notifications, members: members,
		hosts: hosts, logs: logs, render: render, log: log}
}

const logsPerPage = 60

// Show renders the settings screen.
func (h *SettingsHandler) Show(w http.ResponseWriter, r *http.Request) {
	page := max1(httpx.QueryInt(r, "page", 1))
	level := r.URL.Query().Get("level")

	users, _, err := h.auth.ListUsers(r.Context(), r.URL.Query().Get("q"), 0, 200)
	if err != nil {
		h.fail(w, r, "The account list could not be loaded.", err)
		return
	}
	notifications, err := h.notifications.List(r.Context())
	if err != nil {
		h.fail(w, r, "The notification list could not be loaded.", err)
		return
	}
	logs, total, err := h.logs.List(r.Context(), level, (page-1)*logsPerPage, logsPerPage)
	if err != nil {
		h.fail(w, r, "The application log could not be loaded.", err)
		return
	}
	roles, err := h.members.RunVisibility(r.Context(), httpx.UserFrom(r.Context()))
	if err != nil {
		h.fail(w, r, "The role settings could not be loaded.", err)
		return
	}
	hosts, err := h.hosts.List(r.Context())
	if err != nil {
		h.fail(w, r, "The host routes could not be loaded.", err)
		return
	}

	h.render.Render(w, r, "settings", PageData{
		Title:  "Settings",
		Active: "settings",
		Data: map[string]any{
			"users":         users,
			"notifications": notifications,
			"roles":         roles,
			"hosts":         hosts,
			"logs":          logs,
			"logTotal":      total,
			"page":          page,
			"pages":         pageCount(total, logsPerPage),
			"level":         level,
			"droppedLogs":   h.log.Dropped(),
		},
	})
}

// SaveHostOverride creates or updates a host route.
//
// It sits on /settings, which is administrator only, and that is the whole
// access control: a route is an SSRF primitive and is never delegated to a
// project role. See the security note in cronsole.sql.
func (h *SettingsHandler) SaveHostOverride(w http.ResponseWriter, r *http.Request) {
	in := service.HostOverrideInput{
		Hostname: r.PostFormValue("hostname"),
		Address:  r.PostFormValue("address"),
		Port:     r.PostFormValue("port"),
		Note:     r.PostFormValue("note"),
		Active:   httpx.FormBool(r, "active"),
	}
	if user := httpx.UserFrom(r.Context()); user != nil {
		in.Actor = &user.ID
	}

	idParam := chi.URLParam(r, "id")
	if idParam == "" {
		idParam = strings.TrimSpace(r.PostFormValue("id"))
	}
	if idParam != "" && idParam != "0" {
		id, ok := httpx.PathInt64(idParam)
		if !ok {
			http.NotFound(w, r)
			return
		}
		in.ID = id
	}

	row, err := h.hosts.Save(r.Context(), in)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.validationFail(w, err, "The host route could not be saved.")
		return
	}

	// Recorded, not just applied. Changing where a hostname resolves changes
	// where credentials are sent, and that must be answerable afterwards.
	h.log.Warn("settings: host route saved",
		row.Hostname+" -> "+row.Address, "host_override_id", row.ID, "active", row.Active)
	h.redirect(w, r, "/settings")
}

// DeleteHostOverride removes a host route.
func (h *SettingsHandler) DeleteHostOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.hosts.Delete(r.Context(), id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.log.Error("settings: host route delete failed", err.Error(), "host_override_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The host route could not be deleted."))
		return
	}
	h.log.Warn("settings: host route removed", "", "host_override_id", id)
	h.redirect(w, r, "/settings")
}

// SaveUser creates or updates an account.
func (h *SettingsHandler) SaveUser(w http.ResponseWriter, r *http.Request) {
	in := service.UserInput{
		Fullname: r.PostFormValue("fullname"),
		Email:    r.PostFormValue("email"),
		Phone:    r.PostFormValue("phone"),
		Password: r.PostFormValue("password"),
		IsAdmin:  httpx.FormBool(r, "is_admin"),
		Active:   httpx.FormBool(r, "active"),
	}

	// One modal serves create and edit, so the hidden field is what
	// distinguishes them when the path carries no id.
	idParam := chi.URLParam(r, "id")
	if idParam == "" {
		idParam = strings.TrimSpace(r.PostFormValue("id"))
	}
	if idParam == "" || idParam == "0" {
		if _, err := h.auth.CreateUser(r.Context(), in); err != nil {
			h.validationFail(w, err, "The account could not be created.")
			return
		}
		h.redirect(w, r, "/settings")
		return
	}

	id, ok := httpx.PathInt64(idParam)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.auth.UpdateUser(r.Context(), id, in); err != nil {
		h.validationFail(w, err, "The account could not be saved.")
		return
	}
	// A password supplied on the edit form is a reset. It is applied
	// separately from the profile write so an empty field means "leave it",
	// never "clear it".
	if strings.TrimSpace(in.Password) != "" {
		if err := h.auth.ResetPassword(r.Context(), id, in.Password); err != nil {
			h.validationFail(w, err, "The password could not be set.")
			return
		}
	}
	h.redirect(w, r, "/settings")
}

// DeleteUser deactivates an account.
func (h *SettingsHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	// An administrator removing their own account would lock themselves out
	// mid-request, and the screen they land on would be the login page with no
	// explanation.
	if user := httpx.UserFrom(r.Context()); user != nil && user.ID == id {
		httpx.Fail(w, http.StatusBadRequest, tr(r, "You cannot delete the account you are signed in with."))
		return
	}

	if err := h.auth.DeleteUser(r.Context(), id); err != nil {
		// An account that is not there is a 404, not a server fault: a stale
		// link should not page anybody.
		if errors.Is(err, service.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.log.Error("settings: account delete failed", err.Error(), "user_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The account could not be deleted."))
		return
	}
	h.log.Warn("account deactivated", "", "user_id", id)
	h.redirect(w, r, "/settings")
}

// SaveRunVisibility narrows what a role sees of a run.
//
// A role is global, so this is the platform administrator's: narrowing "reader"
// here changes what every reader sees, on every project.
func (h *SettingsHandler) SaveRunVisibility(w http.ResponseWriter, r *http.Request) {
	view := authz.RunView{
		Output:     httpx.FormBool(r, "output"),
		Error:      httpx.FormBool(r, "error"),
		RequestURL: httpx.FormBool(r, "request_url"),
	}

	err := h.members.SetRunVisibility(r.Context(), httpx.UserFrom(r.Context()),
		r.PostFormValue("role"), view)
	if err != nil {
		if denied(w, r, err) {
			return
		}
		h.validationFail(w, err, "The role could not be saved.")
		return
	}
	h.log.Info("settings: run visibility changed", "role", r.PostFormValue("role"))
	h.redirect(w, r, "/settings")
}

// SaveNotification creates or updates a recipient set.
func (h *SettingsHandler) SaveNotification(w http.ResponseWriter, r *http.Request) {
	in := service.NotificationInput{
		Name:      r.PostFormValue("name"),
		OnSuccess: httpx.FormBool(r, "on_success"),
		OnFailure: httpx.FormBool(r, "on_failure"),
		Active:    httpx.FormBool(r, "active"),
		Emails:    splitList(r.PostFormValue("emails")),
	}

	idParam := chi.URLParam(r, "id")
	if idParam == "" {
		idParam = strings.TrimSpace(r.PostFormValue("id"))
	}
	if idParam == "" || idParam == "0" {
		user := httpx.UserFrom(r.Context())
		var userID *int64
		if user != nil {
			userID = &user.ID
		}
		if _, err := h.notifications.Create(r.Context(), in, userID); err != nil {
			h.validationFail(w, err, "The notification list could not be created.")
			return
		}
		h.redirect(w, r, "/settings")
		return
	}

	id, ok := httpx.PathInt64(idParam)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.notifications.Update(r.Context(), id, in); err != nil {
		h.validationFail(w, err, "The notification list could not be saved.")
		return
	}
	h.redirect(w, r, "/settings")
}

// DeleteNotification removes a recipient set.
func (h *SettingsHandler) DeleteNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.notifications.Delete(r.Context(), id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.log.Error("settings: notification delete failed", err.Error(), "notification_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The notification list could not be deleted."))
		return
	}
	h.redirect(w, r, "/settings")
}

// splitList reads a textarea of addresses, one per line or comma separated.
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if trimmed := strings.TrimSpace(f); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (h *SettingsHandler) validationFail(w http.ResponseWriter, err error, fallback string) {
	var ve *service.ValidationError
	if errors.As(err, &ve) {
		httpx.FailWith(w, http.StatusBadRequest, ve.Error(), ve.Errors)
		return
	}
	h.log.Error("settings: save failed", err.Error())
	httpx.Fail(w, http.StatusInternalServerError, fallback)
}

func (h *SettingsHandler) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *SettingsHandler) fail(w http.ResponseWriter, r *http.Request, message string, err error) {
	h.log.Error("settings handler: "+message, err.Error())
	w.WriteHeader(http.StatusInternalServerError)
	h.render.Render(w, r, "error", PageData{Title: "Error", Flash: &Flash{Kind: "danger", Message: message}})
}
