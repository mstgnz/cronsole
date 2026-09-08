// Package i18n translates the interface.
//
// The English source text is the key. `{{ t "Save changes" }}` looks up that
// exact string in the target catalogue and falls back to itself when there is
// no entry, so English needs no catalogue at all and a missing translation
// degrades to readable English rather than to a raw key like
// `jobs.form.save_button` on the screen.
//
// The cost of that choice is that editing English copy orphans its translation
// silently. That is real, and it is why `sync_test.go` walks every template,
// collects every key, and fails when the catalogue and the templates disagree
// in either direction. The test is what makes source-as-key safe; without it
// this would be the wrong design.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed locales/*.json
var catalogues embed.FS

// Lang is a supported interface language.
type Lang string

// The languages this build ships. English is the source and has no catalogue.
const (
	EN Lang = "en"
	TR Lang = "tr"
)

// Default is what an unrecognised or absent preference resolves to.
const Default = EN

// Supported lists the languages in the order a picker should offer them.
var Supported = []Lang{EN, TR}

// Name is the language's own name, for the picker. A language listed in
// another language's words is a language the reader may not recognise.
var Name = map[Lang]string{
	EN: "English",
	TR: "Türkçe",
}

// Dynamic lists the keys reached through a non-literal call, `{{ t . }}`.
//
// The sync test finds keys by scanning templates for `t "..."`, so a value
// translated at runtime is invisible to it: without this list those entries
// would be reported as orphans and, worse, a missing one would never be
// reported at all. Declaring them keeps the test honest in both directions.
//
// Statuses and triggers are domain values, not prose. They come out of the
// database and go out over the API as they are; only the screen translates
// them. Page titles are set in Go and translated by the layout.
var Dynamic = []string{
	// domain.Status*
	"pending", "running", "success", "failed", "timeout", "skipped",
	// domain.Trigger*
	"schedule", "manual", "chain", "api",
	// Docker's own container states, rendered by the container panel. "running"
	// is above, under the run statuses, and means the same thing here.
	"created", "restarting", "paused", "exited", "removing", "dead",
	// page titles that are not already navigation entries
	"New job", "Members", "Error", "Set up Cronsole",
	// relative times, formatted by the renderer's `ago` helper
	"never", "just now", "in %s", "%s ago",
}

// catalogue holds one language's translations.
type catalogue map[string]string

var loaded = map[Lang]catalogue{}

func init() {
	for _, lang := range Supported {
		if lang == EN {
			continue // the source language is the fallback
		}
		raw, err := catalogues.ReadFile("locales/" + string(lang) + ".json")
		if err != nil {
			// A build that ships a language without its file would show English
			// everywhere and look like a bug in the switcher. Fail loudly at
			// start rather than confusingly at render.
			panic(fmt.Sprintf("i18n: catalogue for %q is missing: %v", lang, err))
		}
		var c catalogue
		if err := json.Unmarshal(raw, &c); err != nil {
			panic(fmt.Sprintf("i18n: catalogue for %q is not valid JSON: %v", lang, err))
		}
		loaded[lang] = c
	}
}

// Valid reports whether a string names a language this build ships.
func Valid(s string) bool {
	for _, lang := range Supported {
		if string(lang) == s {
			return true
		}
	}
	return false
}

// Parse turns a stored preference into a language, falling back to the default.
func Parse(s string) Lang {
	if Valid(s) {
		return Lang(s)
	}
	return Default
}

// FromAcceptLanguage picks the best supported language from the browser's
// header. Quality values are ignored: with two languages the first supported
// tag mentioned is the answer, and parsing q-values to get the same result
// would be code nobody can check by reading.
func FromAcceptLanguage(header string) Lang {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		// "tr-TR" and "tr" both mean Turkish here.
		if i := strings.IndexByte(tag, '-'); i >= 0 {
			tag = tag[:i]
		}
		if Valid(strings.ToLower(tag)) {
			return Lang(strings.ToLower(tag))
		}
	}
	return Default
}

// T translates one string.
//
// An untranslated string returns itself, so a catalogue that is behind the
// templates shows English rather than a gap. Arguments are applied with
// Sprintf, which is why a translated string keeps the same verbs in the same
// order as its source.
func T(lang Lang, text string, args ...any) string {
	out := text
	if c, ok := loaded[lang]; ok {
		if translated, ok := c[text]; ok && translated != "" {
			out = translated
		}
	}
	if len(args) == 0 {
		return out
	}
	return fmt.Sprintf(out, args...)
}

// Keys returns a language's catalogue keys, sorted. Used by the sync test.
func Keys(lang Lang) []string {
	c, ok := loaded[lang]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Missing reports the keys a language does not translate, given the full set
// the interface uses.
func Missing(lang Lang, used []string) []string {
	c := loaded[lang]
	var out []string
	for _, key := range used {
		if translated, ok := c[key]; !ok || translated == "" {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Orphans reports catalogue entries no template asks for any more, which is
// what an edit to the English source leaves behind.
func Orphans(lang Lang, used []string) []string {
	inUse := make(map[string]struct{}, len(used))
	for _, key := range used {
		inUse[key] = struct{}{}
	}
	var out []string
	for key := range loaded[lang] {
		if _, ok := inUse[key]; !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
