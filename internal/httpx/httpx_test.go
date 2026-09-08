package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

// --- context values ---------------------------------------------------------

func TestContextValuesRoundTrip(t *testing.T) {
	ctx := context.Background()

	if UserFrom(ctx) != nil {
		t.Error("an empty context produced a user")
	}
	if ProjectFrom(ctx) != nil {
		t.Error("an empty context produced a project")
	}

	user := &domain.User{ID: 7, Email: "a@b.c"}
	project := &domain.Project{ID: 3, Slug: "shop"}

	ctx = WithUser(ctx, user)
	ctx = WithProject(ctx, project)

	if got := UserFrom(ctx); got == nil || got.ID != 7 {
		t.Errorf("UserFrom = %+v, want the user that was attached", got)
	}
	if got := ProjectFrom(ctx); got == nil || got.ID != 3 {
		t.Errorf("ProjectFrom = %+v, want the project that was attached", got)
	}
}

func TestContextKeysDoNotCollideWithPlainStrings(t *testing.T) {
	// The keys are an unexported type for exactly this reason. A handler or a
	// third-party middleware storing context.WithValue(ctx, "user", …) must not
	// be able to become the authenticated operator.
	//lint:ignore SA1029 that is the point of the test
	ctx := context.WithValue(context.Background(), "user", &domain.User{ID: 99, IsAdmin: true}) //nolint:staticcheck

	if got := UserFrom(ctx); got != nil {
		t.Errorf("a plain string key was read as the operator: %+v", got)
	}
}

func TestPermissionsAreNeverNil(t *testing.T) {
	// The layout indexes this map to draw the navigation. A nil map would be
	// readable in a template, but the guard exists so nothing downstream has to
	// know that.
	if got := PermissionsFrom(context.Background()); got == nil {
		t.Fatal("PermissionsFrom returned nil")
	}
	if len(PermissionsFrom(context.Background())) != 0 {
		t.Error("an empty context produced permissions")
	}

	ctx := WithPermissions(context.Background(), map[string]bool{"jobs.create": true})
	if !PermissionsFrom(ctx)["jobs.create"] {
		t.Error("the permission that was attached is not readable")
	}
}

func TestLangFromDefaultsRatherThanReturningEmpty(t *testing.T) {
	// Lang("") selects no template set, and the page would render as nothing.
	if got := LangFrom(context.Background()); got != i18n.Default {
		t.Errorf("LangFrom on an empty context = %q, want %q", got, i18n.Default)
	}
	ctx := WithLang(context.Background(), i18n.TR)
	if got := LangFrom(ctx); got != i18n.TR {
		t.Errorf("LangFrom = %q, want tr", got)
	}
}

func TestResolveLang(t *testing.T) {
	cases := []struct {
		name   string
		cookie string
		header string
		want   i18n.Lang
	}{
		{"nothing asked for", "", "", i18n.Default},
		{"an explicit choice wins", "tr", "en-GB,en;q=0.9", i18n.TR},
		{"the browser is used when there is no choice", "", "tr-TR,tr;q=0.9", i18n.TR},
		{"an unsupported cookie falls through to the header", "de", "tr", i18n.TR},
		{"an unsupported cookie and header fall back", "de", "fr", i18n.Default},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.cookie != "" {
				r.AddCookie(&http.Cookie{Name: LangCookie, Value: c.cookie})
			}
			if c.header != "" {
				r.Header.Set("Accept-Language", c.header)
			}
			if got := ResolveLang(r); got != c.want {
				t.Errorf("ResolveLang = %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveLangIgnoresAnInvalidCookie(t *testing.T) {
	// The cookie is caller supplied and reaches a map lookup that selects a
	// template set. Anything not on the supported list is discarded.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: LangCookie, Value: "../../etc/passwd"})
	if got := ResolveLang(r); got != i18n.Default {
		t.Errorf("ResolveLang honoured a made up cookie: %q", got)
	}
}

// --- responses --------------------------------------------------------------

func TestJSONResponsesCarryTheirHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	OK(w, map[string]int{"count": 2})

	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	// Without nosniff a JSON body containing HTML can be rendered as a page by
	// a browser that guesses the type.
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}

	var body Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if !body.Status {
		t.Error("OK wrote status false")
	}
}

func TestFailOmitsTheEmptyFields(t *testing.T) {
	w := httptest.NewRecorder()
	Fail(w, http.StatusForbidden, "not allowed")

	if w.Code != http.StatusForbidden {
		t.Errorf("code = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `"data"`) || strings.Contains(body, `"errors"`) {
		t.Errorf("a failure carried empty envelope fields: %s", body)
	}
	if !strings.Contains(body, `"status":false`) {
		t.Errorf("a failure did not report status false: %s", body)
	}
}

func TestFailWithCarriesFieldDetail(t *testing.T) {
	w := httptest.NewRecorder()
	FailWith(w, http.StatusUnprocessableEntity, "invalid", map[string]string{"code": "required"})

	var body Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	errs, ok := body.Errors.(map[string]any)
	if !ok || errs["code"] != "required" {
		t.Errorf("Errors = %#v, want the field detail", body.Errors)
	}
}

// --- request bodies ---------------------------------------------------------

func decodeInto(t *testing.T, body string, v any) error {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return DecodeJSON(httptest.NewRecorder(), r, v)
}

func TestDecodeJSON(t *testing.T) {
	type payload struct {
		Code string `json:"code"`
	}

	t.Run("a well formed body", func(t *testing.T) {
		var got payload
		if err := decodeInto(t, `{"code":"daily"}`, &got); err != nil {
			t.Fatalf("DecodeJSON = %v", err)
		}
		if got.Code != "daily" {
			t.Errorf("Code = %q", got.Code)
		}
	})

	t.Run("an unknown field is refused", func(t *testing.T) {
		// A caller who writes "schedule" instead of "schedules" would otherwise
		// register a job with no schedule and be told it worked.
		var got payload
		if err := decodeInto(t, `{"code":"daily","scheduels":["* * * * *"]}`, &got); err == nil {
			t.Error("an unknown field was accepted")
		}
	})

	t.Run("a second JSON value is refused", func(t *testing.T) {
		var got payload
		err := decodeInto(t, `{"code":"a"}{"code":"b"}`, &got)
		if err == nil {
			t.Fatal("trailing content was accepted")
		}
		if err != errTrailingContent {
			t.Errorf("err = %v, want the trailing content error", err)
		}
	})

	t.Run("malformed JSON is refused", func(t *testing.T) {
		var got payload
		if err := decodeInto(t, `{"code":`, &got); err == nil {
			t.Error("a truncated body was accepted")
		}
	})

	t.Run("an oversized body is refused", func(t *testing.T) {
		// Without the reader cap a single POST is read into memory in full.
		big := `{"code":"` + strings.Repeat("x", MaxBodyBytes+1) + `"}`
		var got payload
		if err := decodeInto(t, big, &got); err == nil {
			t.Error("a body over the cap was accepted")
		}
	})
}

// --- query and form parsing -------------------------------------------------

func requestWithQuery(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/?"+query, nil)
}

func TestQueryInt(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"", 24},
		{"hours=6", 6},
		{"hours=%2072%20", 72},
		{"hours=abc", 24},
		{"hours=", 24},
		{"hours=-5", -5}, // the caller clamps; parsing does not invent a policy
	}
	for _, c := range cases {
		if got := QueryInt(requestWithQuery(c.query), "hours", 24); got != c.want {
			t.Errorf("QueryInt(%q) = %d, want %d", c.query, got, c.want)
		}
	}
}

func TestOptionalQueryParameters(t *testing.T) {
	t.Run("int64", func(t *testing.T) {
		if QueryInt64Ptr(requestWithQuery(""), "project") != nil {
			t.Error("an absent parameter produced a filter")
		}
		if QueryInt64Ptr(requestWithQuery("project=nope"), "project") != nil {
			t.Error("an unparseable parameter produced a filter")
		}
		got := QueryInt64Ptr(requestWithQuery("project=42"), "project")
		if got == nil || *got != 42 {
			t.Errorf("QueryInt64Ptr = %v, want 42", got)
		}
	})

	t.Run("string", func(t *testing.T) {
		if QueryStringPtr(requestWithQuery("q=%20%20"), "q") != nil {
			t.Error("whitespace produced a filter")
		}
		got := QueryStringPtr(requestWithQuery("q=%20daily%20"), "q")
		if got == nil || *got != "daily" {
			t.Errorf("QueryStringPtr = %v, want trimmed", got)
		}
	})

	t.Run("bool", func(t *testing.T) {
		if QueryBoolPtr(requestWithQuery(""), "active") != nil {
			t.Error("an absent parameter produced a filter")
		}
		if QueryBoolPtr(requestWithQuery("active=maybe"), "active") != nil {
			t.Error("an unparseable parameter produced a filter")
		}
		for query, want := range map[string]bool{
			"active=true": true, "active=1": true,
			"active=false": false, "active=0": false,
		} {
			got := QueryBoolPtr(requestWithQuery(query), "active")
			if got == nil || *got != want {
				t.Errorf("QueryBoolPtr(%q) = %v, want %v", query, got, want)
			}
		}
	})
}

func TestQueryDatePtr(t *testing.T) {
	loc := time.FixedZone("TRT", 3*3600)

	t.Run("absent", func(t *testing.T) {
		if QueryDatePtr(requestWithQuery(""), "from", loc, false) != nil {
			t.Error("an absent date produced a filter")
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		if QueryDatePtr(requestWithQuery("from=yesterday"), "from", loc, false) != nil {
			t.Error("an unparseable date produced a filter")
		}
	})

	t.Run("the form control's local datetime", func(t *testing.T) {
		got := QueryDatePtr(requestWithQuery("from=2026-09-08T14%3A30"), "from", loc, false)
		if got == nil {
			t.Fatal("a datetime-local value was not parsed")
		}
		// Read in the deployment's zone, not UTC. Getting this wrong shifts
		// every filtered list by the offset.
		if got.Hour() != 14 || got.Minute() != 30 {
			t.Errorf("parsed %s, want 14:30", got)
		}
		if _, offset := got.Zone(); offset != 3*3600 {
			t.Errorf("offset = %d, want the configured zone", offset)
		}
	})

	t.Run("a plain date as the end of a range covers the whole day", func(t *testing.T) {
		// "to=2026-09-08" means up to the end of the 8th. Without this a run at
		// 09:00 that day is outside a range that names that day.
		got := QueryDatePtr(requestWithQuery("to=2026-09-08"), "to", loc, true)
		if got == nil {
			t.Fatal("a date was not parsed")
		}
		if got.Hour() != 23 || got.Minute() != 59 {
			t.Errorf("end of day = %s, want 23:59:59.999…", got)
		}
		if got.Day() != 8 {
			t.Errorf("end of day rolled to day %d", got.Day())
		}
	})

	t.Run("a plain date as the start of a range is midnight", func(t *testing.T) {
		got := QueryDatePtr(requestWithQuery("from=2026-09-08"), "from", loc, false)
		if got == nil {
			t.Fatal("a date was not parsed")
		}
		if got.Hour() != 0 || got.Minute() != 0 {
			t.Errorf("start of day = %s, want midnight", got)
		}
	})
}

func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestFormBool(t *testing.T) {
	// An unchecked checkbox sends nothing at all, which is why presence is the
	// test and every falsy spelling has to be listed.
	cases := []struct {
		values url.Values
		want   bool
	}{
		{url.Values{}, false},
		{url.Values{"active": {""}}, false},
		{url.Values{"active": {"0"}}, false},
		{url.Values{"active": {"false"}}, false},
		{url.Values{"active": {"FALSE"}}, false},
		{url.Values{"active": {"off"}}, false},
		{url.Values{"active": {"no"}}, false},
		{url.Values{"active": {"on"}}, true},
		{url.Values{"active": {"1"}}, true},
		{url.Values{"active": {"true"}}, true},
	}
	for _, c := range cases {
		if got := FormBool(postForm(c.values), "active"); got != c.want {
			t.Errorf("FormBool(%v) = %v, want %v", c.values, got, c.want)
		}
	}
}

func TestFormInt(t *testing.T) {
	if got := FormInt(postForm(url.Values{}), "timeout", 30); got != 30 {
		t.Errorf("an absent field = %d, want the fallback", got)
	}
	if got := FormInt(postForm(url.Values{"timeout": {"nope"}}), "timeout", 30); got != 30 {
		t.Errorf("an unparseable field = %d, want the fallback", got)
	}
	if got := FormInt(postForm(url.Values{"timeout": {" 120 "}}), "timeout", 30); got != 120 {
		t.Errorf("FormInt = %d, want 120", got)
	}
}

func TestFormInt64Ptr(t *testing.T) {
	// Zero is the select element's "none" option, not an id.
	for _, raw := range []string{"", "0", "nope"} {
		if got := FormInt64Ptr(postForm(url.Values{"project": {raw}}), "project"); got != nil {
			t.Errorf("FormInt64Ptr(%q) = %v, want nil", raw, *got)
		}
	}
	got := FormInt64Ptr(postForm(url.Values{"project": {"9"}}), "project")
	if got == nil || *got != 9 {
		t.Errorf("FormInt64Ptr = %v, want 9", got)
	}
}

func TestPathInt64(t *testing.T) {
	// An id reaches a repository. Zero, negative and unparseable are all "not
	// an id" and must not become a query.
	for _, raw := range []string{"", "0", "-1", "abc", "1.5", "9999999999999999999999"} {
		if _, ok := PathInt64(raw); ok {
			t.Errorf("PathInt64(%q) was accepted", raw)
		}
	}
	n, ok := PathInt64(" 12 ")
	if !ok || n != 12 {
		t.Errorf("PathInt64 = %d, %v, want 12, true", n, ok)
	}
}

func TestIsHTMX(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsHTMX(r) {
		t.Error("a plain request was read as an HTMX swap")
	}
	r.Header.Set("HX-Request", "true")
	if !IsHTMX(r) {
		t.Error("an HTMX request was not recognised")
	}
	r.Header.Set("HX-Request", "1")
	if IsHTMX(r) {
		t.Error("only the literal true is the signal htmx sends")
	}
}

// --- redirects --------------------------------------------------------------

func TestSanitizeRedirect(t *testing.T) {
	const fallback = "/"

	rejected := []struct {
		value string
		why   string
	}{
		{"", "empty"},
		{"https://evil.com", "an absolute URL"},
		{"//evil.com", "protocol relative, which a browser reads as a host"},
		{"/\\evil.com", "a backslash some clients normalise to a slash"},
		{"\\/evil.com", "a leading backslash"},
		{"/path\nSet-Cookie: x=1", "a newline, which is header injection"},
		{"/path\rmore", "a carriage return"},
		{"/path\x00", "a null byte"},
		{"/path\x7f", "a delete character"},
		{"evil.com", "no leading slash, which resolves relative to the current page"},
	}
	for _, c := range rejected {
		if got := SanitizeRedirect(c.value, fallback); got != fallback {
			t.Errorf("SanitizeRedirect(%q) = %q, want the fallback (%s)", c.value, got, c.why)
		}
	}

	accepted := []string{
		"/",
		"/jobs",
		"/runs?status=failed&page=2",
		"/jobs/12#schedules",
		"/jobs?q=%2F%2Fevil.com", // encoded, so it stays a query value
	}
	for _, value := range accepted {
		if got := SanitizeRedirect(value, fallback); got != value {
			t.Errorf("SanitizeRedirect(%q) = %q, want it kept", value, got)
		}
	}
}
