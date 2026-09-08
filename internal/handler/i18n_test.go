package handler

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

// tCall matches {{ t "..." }} and {{ t "..." $x }} in a template, capturing the
// English source that is also the catalogue key.
//
// Only the double-quoted form, which is the only one the templates use. A
// backtick string would need escaping rules of its own and there is no reason
// to have two ways of writing the same call.
var tCall = regexp.MustCompile(`\bt\s+"((?:[^"\\]|\\.)*)"`)

// usedKeys walks every template and collects what the interface asks to
// translate.
func usedKeys(t *testing.T) []string {
	t.Helper()

	seen := map[string]struct{}{}
	err := fs.WalkDir(templateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".gohtml") {
			return err
		}
		raw, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		for _, m := range tCall.FindAllStringSubmatch(string(raw), -1) {
			// Go template source escapes are the same as Go's, and the parser
			// unquotes them before the func sees the string.
			seen[strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(m[1])] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking templates: %v", err)
	}

	// The screens also translate from Go: a flash after a save, the message on
	// a refused action. Scanning for tr(r, "...") keeps those under the same
	// guarantee as the templates, rather than relying on somebody remembering
	// to list them.
	//
	// Handlers only. api_handler.go does not call tr at all, because the API
	// keeps its messages in English on purpose.
	//
	// The scan is textual, so a tr call written inside a comment counts as a
	// key and this test reports it as missing. That is the signal: reword the
	// comment rather than adding the phantom to a catalogue.
	goCalls := regexp.MustCompile(`\btr\(r, "((?:[^"\\]|\\.)*)"`)
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing handlers: %v", err)
	}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, m := range goCalls.FindAllStringSubmatch(string(raw), -1) {
			seen[strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1])] = struct{}{}
		}
	}

	// Keys reached through {{ t . }} carry no literal for the scan to find.
	// They are declared instead, so an orphan check does not report them and a
	// missing one is still caught.
	for _, k := range i18n.Dynamic {
		seen[k] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCataloguesMatchTheTemplates(t *testing.T) {
	// This test is what makes "the English source is the key" a safe design.
	// Without it, editing a sentence in a template silently orphans its
	// translation and the screen quietly reverts to English in one place.
	used := usedKeys(t)
	if len(used) == 0 {
		t.Skip("no translated strings yet")
	}

	for _, lang := range i18n.Supported {
		if lang == i18n.Default {
			continue // the source language needs no catalogue
		}

		if missing := i18n.Missing(lang, used); len(missing) > 0 {
			t.Errorf("%s is missing %d translation(s); the screen shows English for these:\n  %s",
				lang, len(missing), strings.Join(missing, "\n  "))
		}
		// Orphans are the other half, and the more insidious one: they are what
		// an edit to the English source leaves behind, and they make the
		// catalogue look complete while the screen is not.
		if orphans := i18n.Orphans(lang, used); len(orphans) > 0 {
			t.Errorf("%s translates %d string(s) no template asks for; the source text was probably edited:\n  %s",
				lang, len(orphans), strings.Join(orphans, "\n  "))
		}
	}
}

func TestEveryPageRendersInEveryLanguage(t *testing.T) {
	// A missing template set is a blank screen rather than an error, because
	// the renderer looks up sets[lang][page] and a missing language yields a
	// missing page. Compiling every page for every language at boot is what
	// prevents that; this checks the boot actually did it.
	r, err := NewRenderer(nil, nil, "test")
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}

	english := r.sets[i18n.Default]
	if len(english) == 0 {
		t.Fatal("no pages compiled for the default language")
	}
	for _, lang := range i18n.Supported {
		if got := len(r.sets[lang]); got != len(english) {
			t.Errorf("%s compiled %d pages, want %d", lang, got, len(english))
		}
		for page := range english {
			if r.sets[lang][page] == nil {
				t.Errorf("%s is missing the %q page", lang, page)
			}
		}
	}
}

func TestTranslationFallsBackToEnglish(t *testing.T) {
	// An untranslated string must render as itself. The alternative, an empty
	// string or a raw key, turns a lagging catalogue into a broken screen.
	const notTranslated = "a string no catalogue will ever contain"
	if got := i18n.T(i18n.TR, notTranslated); got != notTranslated {
		t.Errorf("T(tr, %q) = %q, want the source text back", notTranslated, got)
	}
	if got := i18n.T(i18n.EN, notTranslated); got != notTranslated {
		t.Errorf("T(en, %q) = %q, want the source text back", notTranslated, got)
	}
}

func TestLanguageResolution(t *testing.T) {
	cases := []struct {
		header string
		want   i18n.Lang
	}{
		{"", i18n.EN},
		{"tr", i18n.TR},
		{"tr-TR,tr;q=0.9,en;q=0.8", i18n.TR},
		{"en-GB,en;q=0.9", i18n.EN},
		{"de-DE,de;q=0.9", i18n.EN}, // unsupported falls back rather than failing
		{"fr,tr;q=0.5", i18n.TR},    // first SUPPORTED tag wins
	}
	for _, tc := range cases {
		if got := i18n.FromAcceptLanguage(tc.header); got != tc.want {
			t.Errorf("FromAcceptLanguage(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
