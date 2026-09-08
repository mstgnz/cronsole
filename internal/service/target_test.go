package service

import (
	"strings"
	"testing"
)

func TestResolveWithBaseURL(t *testing.T) {
	// A project with a base address is the containment mechanism: the host
	// comes from the project, and a job may only choose a path under it.
	policy := TargetPolicy{AllowPrivate: true}

	got, err := policy.Resolve("https://service.example.com", "/cron/report")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://service.example.com/cron/report" {
		t.Errorf("got %q", got)
	}

	// A missing leading slash is a typo, not a different meaning.
	got, err = policy.Resolve("https://service.example.com/", "cron/report")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://service.example.com/cron/report" {
		t.Errorf("got %q", got)
	}
}

func TestResolveRefusesEscapingTheBase(t *testing.T) {
	// These are the ways a path escapes the host it was supposed to be
	// confined to. Each has to be refused, or the base address protects
	// nothing.
	policy := TargetPolicy{AllowPrivate: true}

	cases := []struct {
		name   string
		target string
	}{
		{"absolute address", "https://evil.example.com/steal"},
		{"protocol relative", "//evil.example.com/steal"},
		{"backslash", "/\\evil.example.com"},
		{"embedded newline", "/cron/report\nX-Injected: 1"},
		{"embedded carriage return", "/cron/report\rX: 1"},
		{"space", "/cron/ report"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := policy.Resolve("https://service.example.com", c.target); err == nil {
				t.Errorf("Resolve accepted %q against a base address", c.target)
			}
		})
	}
}

func TestResolveWithoutBaseURL(t *testing.T) {
	policy := TargetPolicy{AllowPrivate: true}

	got, err := policy.Resolve("", "https://service.example.com/cron/report")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://service.example.com/cron/report" {
		t.Errorf("got %q", got)
	}

	// Without a base address a bare path is meaningless, and guessing a host
	// for it would be worse than refusing.
	if _, err := policy.Resolve("", "/cron/report"); err == nil {
		t.Error("Resolve accepted a bare path with no base address")
	}
}

func TestResolveSchemeIsAnAllowlist(t *testing.T) {
	// Not a denylist. Anything that is not http or https is out, so a scheme
	// nobody thought to enumerate is refused by default.
	policy := TargetPolicy{AllowPrivate: true}

	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/x",
		"javascript:alert(1)",
		"HTTPS+x://example.com/",
	} {
		if _, err := policy.Resolve("", target); err == nil {
			t.Errorf("Resolve accepted %q", target)
		}
	}
}

func TestResolveRefusesCredentials(t *testing.T) {
	// Credentials in the address would be copied into every run row and shown
	// on the runs screen.
	policy := TargetPolicy{AllowPrivate: true}
	if _, err := policy.Resolve("", "https://user:secret@example.com/cron"); err == nil {
		t.Error("Resolve accepted an address carrying credentials")
	}
}

func TestPrivateTargets(t *testing.T) {
	private := []string{
		"http://127.0.0.1/cron",
		"http://localhost/cron",
		"http://10.1.2.3/cron",
		"http://192.168.0.5/cron",
		"http://172.16.0.1/cron",
		"http://169.254.169.254/latest/meta-data/",
		"http://service.internal/cron",
	}

	// The default is to ALLOW these, and that is deliberate rather than an
	// oversight: the reason this service exists is calling internal endpoints
	// that have no public address.
	allowing := TargetPolicy{AllowPrivate: true}
	for _, target := range private {
		if _, err := allowing.Resolve("", target); err != nil {
			t.Errorf("AllowPrivate should have accepted %q: %v", target, err)
		}
	}

	// Turning it off is what a public-only deployment does, and then the
	// ordinary SSRF guard applies.
	blocking := TargetPolicy{AllowPrivate: false}
	for _, target := range private {
		if _, err := blocking.Resolve("", target); err == nil {
			t.Errorf("AllowPrivate=false should have refused %q", target)
		}
	}
	if _, err := blocking.Resolve("", "https://service.example.com/cron"); err != nil {
		t.Errorf("a public address should still be accepted: %v", err)
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{"ak", "ak-sonuc", "daily_report", "a1", strings.Repeat("a", 64)}
	for _, s := range valid {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"", "a", "-leading", "_leading", "UPPER", "with space", "dot.dot",
		"slash/slash", strings.Repeat("a", 65), "türkçe",
	}
	for _, s := range invalid {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Sovtaj Yeri":           "sovtaj-yeri",
		"  Daily Report  ":      "daily-report",
		"A//B":                  "a-b",
		"already_fine":          "already_fine",
		"---leading---":         "leading",
		"Ünlü Proje":            "nl-proje",
		strings.Repeat("a", 80): strings.Repeat("a", 64),
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}

	// Whatever it produces has to be acceptable to the validator, otherwise
	// the form suggests a value it then refuses.
	for _, in := range []string{"Sovtaj Yeri", "Daily Report", "A//B", "x9"} {
		if out := Slugify(in); !ValidSlug(out) {
			t.Errorf("Slugify(%q) produced %q, which ValidSlug rejects", in, out)
		}
	}
}

func TestHeaderValidation(t *testing.T) {
	// A newline in a name or a value splits the request and lets a second one
	// be smuggled after it. That is the whole point of this check.
	if ValidHeaderName("X-Bad\r\nInjected") {
		t.Error("a header name with CRLF was accepted")
	}
	if ValidHeaderValue("value\r\nX-Injected: 1") {
		t.Error("a header value with CRLF was accepted")
	}
	if ValidHeaderValue("value\x00") {
		t.Error("a header value with a null byte was accepted")
	}

	if !ValidHeaderName("X-Cron-Token") {
		t.Error("an ordinary header name was refused")
	}
	if !ValidHeaderValue("Bearer abc.def") {
		t.Error("an ordinary header value was refused")
	}
	for _, name := range []string{"", "with space", "colon:name", "brack[et]"} {
		if ValidHeaderName(name) {
			t.Errorf("ValidHeaderName(%q) = true, want false", name)
		}
	}
}

func TestMaskSecret(t *testing.T) {
	if got := MaskSecret("abcdefgh"); got != "****efgh" {
		t.Errorf("MaskSecret = %q", got)
	}
	if got := MaskSecret("abc"); got != "***" {
		t.Errorf("MaskSecret = %q", got)
	}
	if got := MaskSecret(""); got != "" {
		t.Errorf("MaskSecret = %q", got)
	}
}
