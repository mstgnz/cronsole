// Package httpx holds what the HTTP layers share: request context values,
// JSON responses, and query parsing.
//
// It exists so middleware and handlers can agree on those without one
// importing the other. Both need the caller's identity; neither should own it.
package httpx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

type ctxKey string

const (
	ctxUser    ctxKey = "user"
	ctxProject ctxKey = "project"
	ctxPerms   ctxKey = "permissions"
	ctxLang    ctxKey = "lang"
)

// WithUser attaches the authenticated operator to a request context.
func WithUser(ctx context.Context, user *domain.User) context.Context {
	return context.WithValue(ctx, ctxUser, user)
}

// UserFrom returns the authenticated operator, or nil.
func UserFrom(ctx context.Context) *domain.User {
	user, _ := ctx.Value(ctxUser).(*domain.User)
	return user
}

// WithProject attaches the API key's project to a request context.
func WithProject(ctx context.Context, project *domain.Project) context.Context {
	return context.WithValue(ctx, ctxProject, project)
}

// ProjectFrom returns the API key's project, or nil.
func ProjectFrom(ctx context.Context) *domain.Project {
	project, _ := ctx.Value(ctxProject).(*domain.Project)
	return project
}

// LangCookie is where the interface language preference lives.
//
// A cookie rather than a column on users: it needs no migration, it works
// before sign in on the login screen, and the preference is per browser, which
// is what somebody switching languages to check a translation actually wants.
const LangCookie = "cs_lang"

// WithLang attaches the resolved interface language.
func WithLang(ctx context.Context, lang i18n.Lang) context.Context {
	return context.WithValue(ctx, ctxLang, lang)
}

// LangFrom returns the interface language, defaulting rather than returning a
// zero value: a Lang("") would select no template set and render nothing.
func LangFrom(ctx context.Context) i18n.Lang {
	lang, _ := ctx.Value(ctxLang).(i18n.Lang)
	if lang == "" {
		return i18n.Default
	}
	return lang
}

// ResolveLang decides the language for a request: an explicit choice first,
// then what the browser asks for, then English.
func ResolveLang(r *http.Request) i18n.Lang {
	if c, err := r.Cookie(LangCookie); err == nil && i18n.Valid(c.Value) {
		return i18n.Lang(c.Value)
	}
	return i18n.FromAcceptLanguage(r.Header.Get("Accept-Language"))
}

// WithPermissions attaches what the operator may do, resolved once per request.
//
// On the context rather than passed by each handler, because the layout draws
// the navigation from it. A handler that forgot to pass it would render a menu
// with every entry missing, and the bug would look like a permission problem.
func WithPermissions(ctx context.Context, perms map[string]bool) context.Context {
	return context.WithValue(ctx, ctxPerms, perms)
}

// PermissionsFrom returns what the operator may do. Never nil, so a template can
// index it without a guard.
func PermissionsFrom(ctx context.Context) map[string]bool {
	perms, _ := ctx.Value(ctxPerms).(map[string]bool)
	if perms == nil {
		return map[string]bool{}
	}
	return perms
}

// Envelope is the shape of every JSON response.
type Envelope struct {
	Status  bool   `json:"status"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
	Errors  any    `json:"errors,omitempty"`
}

// JSON writes a JSON response.
func JSON(w http.ResponseWriter, code int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// OK writes a successful JSON response.
func OK(w http.ResponseWriter, data any) {
	JSON(w, http.StatusOK, Envelope{Status: true, Data: data})
}

// Fail writes an error JSON response.
//
// The message is what the caller is told. It is chosen by the handler from a
// sentinel, never taken from a driver or a library error: those carry table
// names, query fragments and file paths, and an error response is the cheapest
// place to leak the shape of a system.
func Fail(w http.ResponseWriter, code int, message string) {
	JSON(w, code, Envelope{Status: false, Message: message})
}

// FailWith writes an error response carrying field level detail.
func FailWith(w http.ResponseWriter, code int, message string, errs any) {
	JSON(w, code, Envelope{Status: false, Message: message, Errors: errs})
}

// MaxBodyBytes bounds a request body. Without it a single large POST can be
// read into memory in full.
const MaxBodyBytes = 1 << 20

// DecodeJSON reads a JSON body into v, refusing unknown fields.
//
// Unknown fields are refused rather than ignored, so a caller that misspells
// "schedules" is told, instead of silently registering a job with no schedule.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// A second JSON value in the same body is a sign the caller is confused
	// about the format, and taking only the first would hide it.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errTrailingContent
	}
	return nil
}

type constError string

func (e constError) Error() string { return string(e) }

const errTrailingContent = constError("body must contain a single JSON object")

// QueryInt reads an integer query parameter.
func QueryInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// QueryInt64Ptr reads an optional bigint query parameter. A missing or
// unparseable value yields nil, which every filter reads as "not applied".
func QueryInt64Ptr(r *http.Request, name string) *int64 {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// QueryStringPtr reads an optional string query parameter.
func QueryStringPtr(r *http.Request, name string) *string {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	return &raw
}

// QueryBoolPtr reads an optional boolean query parameter, where an empty value
// means "either".
func QueryBoolPtr(r *http.Request, name string) *bool {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &b
}

// QueryDatePtr reads an optional date, accepting a plain date or a local
// datetime as produced by the form controls.
func QueryDatePtr(r *http.Request, name string, loc *time.Location, endOfDay bool) *time.Time {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		t, err := time.ParseInLocation(layout, raw, loc)
		if err != nil {
			continue
		}
		if layout == "2006-01-02" && endOfDay {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return &t
	}
	return nil
}

// FormBool reads a checkbox. An absent checkbox is false, which is why the
// presence of the key is the whole test.
func FormBool(r *http.Request, name string) bool {
	v := strings.TrimSpace(r.PostFormValue(name))
	switch strings.ToLower(v) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// FormInt reads an integer form field.
func FormInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.PostFormValue(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// FormInt64Ptr reads an optional bigint form field.
func FormInt64Ptr(r *http.Request, name string) *int64 {
	raw := strings.TrimSpace(r.PostFormValue(name))
	if raw == "" || raw == "0" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// PathInt64 reads a bigint from a URL path parameter.
func PathInt64(raw string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// IsHTMX reports whether the request came from an HTMX swap, so a handler can
// answer with a fragment instead of a redirect.
func IsHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// SanitizeRedirect keeps a redirect target on this origin.
//
// The rules are deliberately blunt: a single leading slash, no second slash or
// backslash after it, no control characters. Everything else falls back. A
// value like "//evil.com" is a valid URL to a browser and is exactly what an
// open redirect is made of.
func SanitizeRedirect(value, fallback string) string {
	if value == "" || !strings.HasPrefix(value, "/") {
		return fallback
	}
	if len(value) > 1 && (value[1] == '/' || value[1] == '\\') {
		return fallback
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fallback
		}
	}
	return value
}
