// Package handler maps HTTP to the services and back.
//
// Handlers hold no business rules. They read a request, call one service
// method, and translate the answer. A rule that lives here is a rule the API,
// the interface and the sync endpoint each get their own version of.
package handler

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/httpx"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

//go:embed all:templates
var templateFS embed.FS

// Renderer holds the compiled template sets.
//
// One set per page PER LANGUAGE, compiled at start up. Parsing per request
// would reread every file on every hit and, worse, would turn a broken template
// into a runtime 500 on the one screen nobody opened during testing. Compiling
// up front makes it a boot failure.
//
// A set per language rather than a translator passed through the data: `t` is
// bound into the FuncMap, and a FuncMap is fixed when the template is parsed.
// The alternatives were cloning the set on every request, or threading the
// language into every partial's data map by hand. Two small languages is a few
// hundred kilobytes of parse trees held once.
type Renderer struct {
	sets     map[i18n.Lang]map[string]*template.Template
	location *time.Location
	log      *applog.Logger
	version  string
}

// PageData is what every page receives.
type PageData struct {
	Title   string
	Active  string
	User    *domain.User
	Flash   *Flash
	Version string
	// Lang is the language this page rendered in, for the html lang attribute
	// and the picker's current selection.
	Lang i18n.Lang
	// Path is where the reader is, so the language switcher can return them to
	// it rather than to the dashboard.
	Path string
	// Perms is what this operator may do, filled by the renderer from the
	// request rather than by each handler. The layout draws the navigation from
	// it, and a handler that forgot to pass it would render a menu with every
	// entry missing.
	Perms map[string]bool
	Data  map[string]any
}

// Can reports whether the operator holds a permission.
//
// For the templates: {{ if $.Can "jobs.create" }}. It answers the verb only,
// never the row, so a button it shows can still be refused by the endpoint when
// the project is out of scope. That is the right way round: hiding a control
// somebody may use on one project and not another would hide it everywhere.
func (p PageData) Can(key string) bool { return p.Perms[key] }

// Flash is a one-shot message shown at the top of a page.
type Flash struct {
	Kind    string // success, danger, warning, info
	Message string
}

// NewRenderer compiles every page, once per language.
func NewRenderer(loc *time.Location, log *applog.Logger, version string) (*Renderer, error) {
	r := &Renderer{
		sets:     map[i18n.Lang]map[string]*template.Template{},
		location: loc,
		log:      log,
		version:  version,
	}

	pages, err := fs.Glob(templateFS, "templates/pages/*.gohtml")
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("handler: no page templates found")
	}

	for _, lang := range i18n.Supported {
		r.sets[lang] = map[string]*template.Template{}
		for _, page := range pages {
			name := strings.TrimSuffix(page[len("templates/pages/"):], ".gohtml")
			set, err := template.New("layout.gohtml").Funcs(r.funcs(lang)).ParseFS(
				templateFS,
				"templates/layout.gohtml",
				"templates/partials/*.gohtml",
				page,
			)
			if err != nil {
				return nil, fmt.Errorf("handler: template %q (%s): %w", name, lang, err)
			}
			r.sets[lang][name] = set
		}
	}
	return r, nil
}

// Render writes a full page.
//
// The template is executed into a buffer first. Writing straight to the
// response means a template that fails halfway has already sent a 200 and half
// a page, and the error arrives as a truncated screen instead of an error.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, page string, data PageData) {
	lang := httpx.LangFrom(req.Context())
	set, ok := r.sets[lang][page]
	if !ok {
		r.log.Error("render: unknown page", page)
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}

	data.User = httpx.UserFrom(req.Context())
	data.Perms = httpx.PermissionsFrom(req.Context())
	data.Lang = lang
	// RequestURI rather than Path, so switching language on a filtered list
	// keeps the filter. Sanitised again on the way back in, because this is
	// caller supplied and will be posted back.
	data.Path = req.URL.RequestURI()
	data.Version = r.version
	if data.Data == nil {
		data.Data = map[string]any{}
	}

	var buf bytes.Buffer
	if err := set.Execute(&buf, data); err != nil {
		r.log.Error("render: template failed", err.Error(), "page", page)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}

// Partial renders one named template, for HTMX fragment responses.
//
// It takes the request so the fragment is rendered in the same language as the
// page it is being swapped into. Without it a filtered table would come back in
// English underneath a Turkish heading.
func (r *Renderer) Partial(w http.ResponseWriter, req *http.Request, page, name string, data any) {
	set, ok := r.sets[httpx.LangFrom(req.Context())][page]
	if !ok {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, data); err != nil {
		r.log.Error("render: partial failed", err.Error(), "page", page, "partial", name)
		http.Error(w, "the fragment could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (r *Renderer) funcs(lang i18n.Lang) template.FuncMap {
	return template.FuncMap{
		// t translates. The English source is the key, so an untranslated
		// string renders as itself rather than as a placeholder.
		"t": func(text string, args ...any) string { return i18n.T(lang, text, args...) },
		// lang is the current language, for the picker and the html lang
		// attribute.
		"lang":      func() string { return string(lang) },
		"languages": func() []i18n.Lang { return i18n.Supported },
		"langName":  func(l i18n.Lang) string { return i18n.Name[l] },
		"datetime":  func(t any) string { return r.formatTime(t, "2006-01-02 15:04:05") },
		"date":      func(t any) string { return r.formatTime(t, "2006-01-02") },
		"clock":     func(t any) string { return r.formatTime(t, "15:04:05") },
		"short":     func(t any) string { return r.formatTime(t, "02 Jan 15:04") },
		"ago":       func(v any) string { return r.ago(lang, v) },
		"duration":  humanDuration,
		"bytes":     humanBytes,
		"span":      func(seconds any) string { return humanSpanSeconds(seconds) },
		// barWidth and usageClass draw a usage bar. Trusted CSS because this
		// produces it: the value is clamped to a percentage here, so nothing
		// from outside the process reaches a style attribute.
		"barWidth":   barWidth,
		"usageClass": usageClass,
		"statusClass": func(status string) string {
			switch status {
			case domain.StatusSuccess:
				return "success"
			case domain.StatusFailed:
				return "danger"
			case domain.StatusTimeout:
				return "warning"
			case domain.StatusRunning:
				return "primary"
			case domain.StatusSkipped:
				return "secondary"
			case domain.StatusPending:
				return "info"
			}
			return "light"
		},
		"healthClass": func(h string) string {
			switch h {
			case "healthy":
				return "success"
			case "late":
				return "warning"
			case "down":
				return "danger"
			}
			return "secondary"
		},
		// tint gives a name a stable colour, so a project keeps the same badge
		// on every screen and in every session. Derived rather than stored:
		// Cronicle gives each category a colour an administrator picks, which is
		// a settings screen, a column and a migration to solve a problem that a
		// hash solves for free. The palette is small on purpose; the point is to
		// tell six brands apart at a glance, not to be decorative.
		"tint": func(s string) string {
			var sum uint32
			for _, r := range s {
				sum = sum*31 + uint32(r)
			}
			return fmt.Sprintf("tint-%d", sum%8)
		},
		"json": func(v any) template.JS {
			out, err := json.Marshal(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(out)
		},
		"truncate": func(n int, s string) string {
			runes := []rune(s)
			if len(runes) <= n {
				return s
			}
			return string(runes[:n]) + "..."
		},
		"join":      strings.Join,
		"lower":     strings.ToLower,
		"upper":     strings.ToUpper,
		"add":       func(a, b int) int { return a + b },
		"sub":       func(a, b int) int { return a - b },
		"percent":   percent,
		"dict":      dict,
		"list":      func(values ...any) []any { return values },
		"deref":     deref,
		"hasPrefix": strings.HasPrefix,
		"pageRange": pageRange,
		// qs reads one query parameter. Templates cannot index a url.Values
		// key that is absent without erroring, and a filter form is mostly
		// absent keys.
		"qs": func(values url.Values, key string) string { return values.Get(key) },
		"eqs": func(values url.Values, key, want string) bool {
			return values.Get(key) == want
		},
	}
}

func (r *Renderer) formatTime(v any, layout string) string {
	t, ok := asTime(v)
	if !ok {
		return ""
	}
	return t.In(r.location).Format(layout)
}

// ago renders a relative time. The dashboard is read at a glance, and "3
// minutes ago" answers "is this current" in a way a timestamp does not.
// ago renders a relative time.
//
// The number keeps its unit suffix in every language: "3m" is read the same by
// a Turkish operator, and translating the units would make the column widths
// jump between languages on a screen that is scanned rather than read. Only the
// framing around it is translated, because "3m ago" and "3m önce" put the words
// on opposite sides of the number.
func (r *Renderer) ago(lang i18n.Lang, v any) string {
	t, ok := asTime(v)
	if !ok {
		return i18n.T(lang, "never")
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return i18n.T(lang, "in %s", humanSpan(-d))
	case d < 10*time.Second:
		return i18n.T(lang, "just now")
	default:
		return i18n.T(lang, "%s ago", humanSpan(d))
	}
}

func humanSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// humanDuration renders milliseconds. Sub-second values keep their precision
// because most jobs finish there, and rounding them all to "0s" would hide
// the difference between a fast job and a broken one.
func humanDuration(v any) string {
	ms, ok := asInt(v)
	if !ok {
		return ""
	}
	switch {
	case ms < 1000:
		return fmt.Sprintf("%d ms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1f s", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm %ds", ms/60000, (ms%60000)/1000)
	}
}

// humanBytes renders a byte count the way an operator reads one.
//
// Powers of 1024 with decimal names, which is what free, df and every hosting
// invoice show. Being pedantic about GiB here would only make the figure
// disagree with the one the machine reports beside it.
func humanBytes(v any) string {
	value, ok := asUint64(v)
	if !ok {
		return ""
	}
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	size, exp := float64(value)/unit, 0
	for size >= unit && exp < 3 {
		size /= unit
		exp++
	}
	// One decimal below ten, none above: "9.4 GB" is worth the digit and
	// "512.3 GB" is noise on a screen read at a glance.
	if size < 10 {
		return fmt.Sprintf("%.1f %cB", size, "KMGT"[exp])
	}
	return fmt.Sprintf("%.0f %cB", size, "KMGT"[exp])
}

// humanSpanSeconds renders a number of seconds as an approximate span, for
// uptime. Same units as ago, so the two read alike on the same screen.
func humanSpanSeconds(v any) string {
	seconds, ok := asUint64(v)
	if !ok {
		return ""
	}
	return humanSpan(time.Duration(seconds) * time.Second)
}

// barWidth is the width of a usage bar, clamped to a percentage.
//
// Trusted CSS because the value is produced here and bounded here. An unknown
// reading (negative) draws nothing rather than a full bar.
func barWidth(v any) template.CSS {
	value, ok := asFloat(v)
	if !ok || value < 0 {
		return template.CSS("0%")
	}
	if value > 100 {
		value = 100
	}
	return template.CSS(fmt.Sprintf("%.1f%%", value))
}

// usageClass colours a usage bar.
//
// Quiet until it is worth looking at. The thresholds are where a machine stops
// having room to absorb a spike, not where it is full: a disk noticed at 95% is
// noticed too late to move anything off it calmly.
func usageClass(v any) string {
	value, ok := asFloat(v)
	if !ok || value < 0 {
		return "bg-secondary"
	}
	switch {
	case value >= 90:
		return "bg-danger"
	case value >= 75:
		return "bg-warning"
	}
	return ""
}

func percent(part, total int) int {
	if total == 0 {
		return 0
	}
	return part * 100 / total
}

func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return time.Time{}, false
		}
		return t, true
	case *time.Time:
		if t == nil || t.IsZero() {
			return time.Time{}, false
		}
		return *t, true
	}
	return time.Time{}, false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case *int:
		if n == nil {
			return 0, false
		}
		return *n, true
	}
	return 0, false
}

func asUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func deref(v any) any {
	switch n := v.(type) {
	case *int:
		if n == nil {
			return 0
		}
		return *n
	case *int64:
		if n == nil {
			return int64(0)
		}
		return *n
	case *string:
		if n == nil {
			return ""
		}
		return *n
	}
	return v
}

// dict builds a map inside a template, so a partial can be given more than one
// value without inventing a struct for every call site.
func dict(values ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			continue
		}
		out[key] = values[i+1]
	}
	return out
}

// pageRange produces the page numbers to show around the current one.
func pageRange(current, total int) []int {
	if total < 1 {
		return nil
	}
	start := current - 2
	if start < 1 {
		start = 1
	}
	end := start + 4
	if end > total {
		end = total
		start = end - 4
		if start < 1 {
			start = 1
		}
	}
	out := make([]int, 0, end-start+1)
	for i := start; i <= end; i++ {
		out = append(out, i)
	}
	return out
}

// StaticFS exposes the bundled static files.
//
// Embedded rather than read from disk so the binary is the whole deployment:
// a container that was built without its assets directory fails at build time,
// not when somebody opens the interface.
//
//go:embed all:static
var staticFS embed.FS

// StaticHandler serves the bundled static files.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("handler: static assets missing: %v", err))
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}
