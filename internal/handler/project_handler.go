package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/service"
)

// ProjectHandler serves the project screens.
type ProjectHandler struct {
	guard
	projects *service.ProjectService
	members  *service.MemberService
	render   *Renderer
	log      *applog.Logger
	baseURL  string
}

// NewProjectHandler wires the handler.
func NewProjectHandler(a *authz.Service, projects *service.ProjectService, members *service.MemberService,
	render *Renderer, log *applog.Logger, baseURL string) *ProjectHandler {
	return &ProjectHandler{guard: guard{authz: a}, projects: projects, members: members,
		render: render, log: log, baseURL: baseURL}
}

// List shows the projects this operator may see, with their counters.
func (h *ProjectHandler) List(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r, authz.ProjectsRead)
	if !ok {
		return
	}
	rows, err := h.projects.List(r.Context(), scope, r.URL.Query().Get("q"))
	if err != nil {
		h.fail(w, r, "The project list could not be loaded.", err)
		return
	}

	data := map[string]any{
		"projects": rows,
		"filter":   r.URL.Query(),
		"baseURL":  h.baseURL,
		"can":      h.permissions(r),
	}
	// A freshly issued key is passed through the redirect once and shown once.
	// It is never stored in a readable form, so this is the only moment it can
	// be copied.
	if key := r.URL.Query().Get("key"); key != "" {
		data["newKey"] = key
	}
	h.render.Render(w, r, "projects", PageData{Title: "Projects", Active: "projects", Data: data})
}

// Save creates or updates a project.
func (h *ProjectHandler) Save(w http.ResponseWriter, r *http.Request) {
	in := service.ProjectInput{
		Name:        r.PostFormValue("name"),
		Slug:        r.PostFormValue("slug"),
		Description: r.PostFormValue("description"),
		BaseURL:     r.PostFormValue("base_url"),
		Active:      httpx.FormBool(r, "active"),
	}

	// The id may arrive on the path or as a hidden field. The modal reuses one
	// form for create and edit, so the field is what distinguishes them.
	idParam := chi.URLParam(r, "id")
	if idParam == "" {
		idParam = strings.TrimSpace(r.PostFormValue("id"))
	}
	if idParam == "" || idParam == "0" {
		// Creating a project is not a project-scoped act: there is no project yet
		// to be scoped to. No built-in role carries it, so this is the platform
		// administrator's alone.
		if _, ok := h.require(w, r, authz.ProjectsCreate); !ok {
			return
		}
		user := h.user(r)
		var userID *int64
		if user != nil {
			userID = &user.ID
		}

		project, key, err := h.projects.Create(r.Context(), in, userID)
		if err != nil {
			h.validationFail(w, err, "The project could not be created.")
			return
		}
		h.log.Info("project created", "project_id", project.ID, "slug", project.Slug)
		h.redirect(w, r, "/projects?key="+key)
		return
	}

	id, ok := httpx.PathInt64(idParam)
	if !ok {
		http.NotFound(w, r)
		return
	}
	scope, ok := h.require(w, r, authz.ProjectsUpdate)
	if !ok {
		return
	}
	if err := h.projects.Update(r.Context(), scope, id, in); err != nil {
		if denied(w, r, err) {
			return
		}
		h.validationFail(w, err, "The project could not be saved.")
		return
	}
	h.redirect(w, r, "/projects")
}

// RotateKey issues a new API key and shows it once.
func (h *ProjectHandler) RotateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	scope, ok := h.require(w, r, authz.ProjectsRotateKey)
	if !ok {
		return
	}

	key, err := h.projects.RotateKey(r.Context(), scope, id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.log.Error("project: key rotation failed", err.Error(), "project_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The key could not be rotated."))
		return
	}
	h.log.Warn("project: API key rotated", "the previous key stopped working immediately", "project_id", id)
	h.redirect(w, r, "/projects?key="+key)
}

// Delete removes a project and its jobs.
//
// It refuses while active jobs remain unless the caller confirms. Deleting a
// project quietly stops however many scheduled jobs it owned, and that is not
// something to discover afterwards.
func (h *ProjectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	scope, ok := h.require(w, r, authz.ProjectsDelete)
	if !ok {
		return
	}

	err := h.projects.Delete(r.Context(), scope, id, httpx.FormBool(r, "force"))
	if errors.Is(err, service.ErrProjectLocked) {
		httpx.Fail(w, http.StatusConflict,
			"This project still has active jobs. Deactivate them first, or confirm to delete them with it.")
		return
	}
	// A project that is not there, or is out of scope, is a 404 and not a
	// server fault. Reporting it as one pages somebody for a stale link and
	// fills the error log with a non-event.
	if denied(w, r, err) {
		return
	}
	if err != nil {
		h.log.Error("project: delete failed", err.Error(), "project_id", id)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The project could not be deleted."))
		return
	}
	h.log.Warn("project deleted", "its jobs were deactivated with it", "project_id", id)
	h.redirect(w, r, "/projects")
}

// --- members ---

// Members shows who may reach a project.
func (h *ProjectHandler) Members(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.renderMembers(w, r, id, nil); err != nil {
		if denied(w, r, err) {
			return
		}
		h.fail(w, r, "The member list could not be loaded.", err)
	}
}

// AddMember grants somebody a role on a project.
func (h *ProjectHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	err := h.members.GrantByEmail(r.Context(), h.user(r), id,
		r.PostFormValue("email"), r.PostFormValue("role"))
	if err != nil {
		h.memberError(w, r, id, err)
		return
	}
	h.log.Info("project: member granted", "project_id", id, "role", r.PostFormValue("role"))
	_ = h.renderMembers(w, r, id, &Flash{Kind: "success", Message: tr(r, "Access granted.")})
}

// RemoveMember takes a role away on a project.
func (h *ProjectHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathInt64(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	userID, uok := httpx.PathInt64(chi.URLParam(r, "userID"))
	roleID, rok := httpx.PathInt64(chi.URLParam(r, "roleID"))
	if !uok || !rok {
		http.NotFound(w, r)
		return
	}

	if err := h.members.Revoke(r.Context(), h.user(r), id, userID, roleID); err != nil {
		h.memberError(w, r, id, err)
		return
	}
	h.log.Info("project: member revoked", "project_id", id, "user_id", userID)
	_ = h.renderMembers(w, r, id, &Flash{Kind: "success", Message: tr(r, "Access removed.")})
}

// renderMembers draws the panel. Both the read and the two writes end here, so
// the screen after a change is the screen the change actually produced rather
// than one assembled from what the handler believes it did.
func (h *ProjectHandler) renderMembers(w http.ResponseWriter, r *http.Request, projectID int64, flash *Flash) error {
	caller := h.user(r)
	members, err := h.members.ListMembers(r.Context(), caller, projectID)
	if err != nil {
		return err
	}
	// Not being able to grant is not an error here: a reader of the member list
	// simply gets the list without the form.
	roles, _ := h.members.Roles(r.Context(), caller, projectID)

	scope, ok := h.scope(w, r, authz.ProjectsRead)
	if !ok {
		return nil
	}
	project, err := h.projects.Get(r.Context(), scope, projectID)
	if err != nil {
		return err
	}

	data := map[string]any{
		"project": project,
		"members": members,
		"roles":   roles,
		"can":     h.permissions(r),
		"flash":   flash,
	}
	if httpx.IsHTMX(r) {
		h.render.Partial(w, r, "project_members", "member-panel", data)
		return nil
	}
	h.render.Render(w, r, "project_members", PageData{
		Title:  "Members",
		Active: "projects",
		Data:   data,
	})
	return nil
}

// memberError answers a failed grant or revoke.
//
// The membership rules are refusals a person needs to read, not internal
// errors: "you cannot grant a role above your own" is the answer, and hiding it
// behind a generic 403 leaves them clicking the same button again.
func (h *ProjectHandler) memberError(w http.ResponseWriter, r *http.Request, projectID int64, err error) {
	switch {
	case errors.Is(err, service.ErrSelfMembership),
		errors.Is(err, service.ErrRoleTooHigh),
		errors.Is(err, service.ErrTargetIsAdmin),
		errors.Is(err, service.ErrUnscopedMembership):
		_ = h.renderMembers(w, r, projectID, &Flash{Kind: "warning", Message: err.Error()})
	case errors.Is(err, service.ErrNotFound):
		_ = h.renderMembers(w, r, projectID, &Flash{
			Kind:    "warning",
			Message: tr(r, "No account with that address. They must sign in once before they can be added."),
		})
	case denied(w, r, err):
	default:
		h.log.Error("project: membership change failed", err.Error(), "project_id", projectID)
		httpx.Fail(w, http.StatusInternalServerError, tr(r, "The change could not be applied."))
	}
}

func (h *ProjectHandler) validationFail(w http.ResponseWriter, err error, fallback string) {
	var ve *service.ValidationError
	if errors.As(err, &ve) {
		httpx.FailWith(w, http.StatusBadRequest, ve.Error(), ve.Errors)
		return
	}
	h.log.Error("project: save failed", err.Error())
	httpx.Fail(w, http.StatusInternalServerError, fallback)
}

func (h *ProjectHandler) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if httpx.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *ProjectHandler) fail(w http.ResponseWriter, r *http.Request, message string, err error) {
	h.log.Error("project handler: "+message, err.Error())
	w.WriteHeader(http.StatusInternalServerError)
	h.render.Render(w, r, "error", PageData{Title: "Error", Flash: &Flash{Kind: "danger", Message: message}})
}
