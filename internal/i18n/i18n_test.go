package i18n

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, s := range []string{"en", "tr"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false", s)
		}
	}
	// The cookie value reaches a map lookup that selects a template set, so
	// anything not on the list has to be refused rather than carried.
	for _, s := range []string{"", "EN", "de", "en-GB", "../../etc/passwd", "en\x00"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
}

func TestParseFallsBackRatherThanFailing(t *testing.T) {
	if got := Parse("tr"); got != TR {
		t.Errorf("Parse(tr) = %q", got)
	}
	for _, s := range []string{"", "de", "nonsense"} {
		if got := Parse(s); got != Default {
			t.Errorf("Parse(%q) = %q, want the default", s, got)
		}
	}
}

func TestFromAcceptLanguage(t *testing.T) {
	cases := []struct {
		header string
		want   Lang
		why    string
	}{
		{"", Default, "nothing asked for"},
		{"tr", TR, "the plain tag"},
		{"tr-TR", TR, "a region is still that language"},
		{"TR-tr", TR, "case does not matter in the header"},
		{"tr-TR,tr;q=0.9,en;q=0.8", TR, "the first supported tag wins"},
		{"de,fr,tr", TR, "unsupported tags are skipped"},
		{"de,fr", Default, "nothing supported was asked for"},
		{"en-GB,en;q=0.9", EN, "English regions"},
		{"  tr  ", TR, "whitespace around a tag"},
		{"*", Default, "the wildcard is not a language"},
		{";q=0.9", Default, "a quality value with no tag"},
	}

	for _, c := range cases {
		if got := FromAcceptLanguage(c.header); got != c.want {
			t.Errorf("FromAcceptLanguage(%q) = %q, want %q (%s)", c.header, got, c.want, c.why)
		}
	}
}

func TestFromAcceptLanguageIgnoresQualityOrder(t *testing.T) {
	// Documented behaviour, pinned so it is a decision rather than a surprise:
	// with two languages the first supported tag mentioned is the answer, and
	// parsing q-values to reach the same result would be code nobody can check
	// by reading. If a third language is ever added, revisit this.
	if got := FromAcceptLanguage("tr;q=0.1,en;q=0.9"); got != TR {
		t.Errorf("FromAcceptLanguage = %q; the first supported tag should win", got)
	}
}

func TestTranslates(t *testing.T) {
	if got := T(TR, "Dashboard"); got == "Dashboard" {
		t.Error("a key that is in the Turkish catalogue came back untranslated")
	}
	// English is the source language and has no catalogue, so it returns
	// itself. That is the whole point of source-as-key.
	if got := T(EN, "Dashboard"); got != "Dashboard" {
		t.Errorf("T(en) = %q, want the source text", got)
	}
}

func TestAnUntranslatedStringRendersAsItself(t *testing.T) {
	// The failure mode this design avoids: a missing entry shows readable
	// English rather than a raw key like jobs.form.save_button on the screen.
	const unknown = "This string is not in any catalogue."
	if got := T(TR, unknown); got != unknown {
		t.Errorf("T = %q, want the source text", got)
	}
	if got := T(Lang("de"), unknown); got != unknown {
		t.Errorf("T for an unknown language = %q, want the source text", got)
	}
}

func TestArgumentsAreApplied(t *testing.T) {
	if got := T(EN, "%d of %d", 2, 5); got != "2 of 5" {
		t.Errorf("T = %q, want \"2 of 5\"", got)
	}
	// And through a translation, which is why a translated string has to keep
	// the same verbs in the same order as its source.
	if got := T(TR, "%d timeout", 3); !strings.Contains(got, "3") {
		t.Errorf("T = %q, want the argument to appear", got)
	}
}

func TestNoArgumentsMeansNoFormatting(t *testing.T) {
	// A string containing a percent sign must survive when nothing is being
	// substituted, or "100% done" renders as "100%!d(MISSING)one".
	const literal = "100% done"
	if got := T(EN, literal); got != literal {
		t.Errorf("T = %q, want %q", got, literal)
	}
}

func TestKeys(t *testing.T) {
	keys := Keys(TR)
	if len(keys) == 0 {
		t.Fatal("the Turkish catalogue is empty")
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("Keys is not sorted: %q before %q", keys[i-1], keys[i])
		}
	}
	// English has no catalogue, by design.
	if got := Keys(EN); got != nil {
		t.Errorf("Keys(en) returned %d entries, want none", len(got))
	}
}

func TestMissingReportsWhatIsNotTranslated(t *testing.T) {
	used := []string{"Dashboard", "a string nobody has translated", "another one"}
	missing := Missing(TR, used)

	if len(missing) != 2 {
		t.Fatalf("Missing = %v, want the two untranslated strings", missing)
	}
	for _, key := range missing {
		if key == "Dashboard" {
			t.Error("a translated key was reported as missing")
		}
	}
	// Sorted, so the test output reads the same way twice.
	if missing[0] > missing[1] {
		t.Errorf("Missing is not sorted: %v", missing)
	}
}

func TestAnEmptyTranslationCountsAsMissing(t *testing.T) {
	// "key": "" in a catalogue is a translation somebody started and did not
	// finish. Rendering it would blank the label rather than fall back.
	for _, key := range Keys(TR) {
		if T(TR, key) == "" {
			t.Errorf("%q translates to an empty string", key)
		}
	}
}

func TestOrphansReportsEntriesNothingAsksFor(t *testing.T) {
	// What an edit to the English source leaves behind. Source-as-key is only
	// safe because this is checked in both directions.
	orphans := Orphans(TR, []string{"Dashboard"})
	if len(orphans) == 0 {
		t.Fatal("Orphans reported nothing against a single used key")
	}
	for _, key := range orphans {
		if key == "Dashboard" {
			t.Error("a key that IS in use was reported as an orphan")
		}
	}

	// And nothing is an orphan when everything is in use.
	if got := Orphans(TR, Keys(TR)); len(got) != 0 {
		t.Errorf("Orphans = %v, want none", got)
	}
}

func TestMissingAndOrphansOnALanguageWithNoCatalogue(t *testing.T) {
	// English is the source. Every key is "missing" because there is nothing to
	// look up, and nothing can be orphaned.
	used := []string{"Dashboard", "Jobs"}
	if got := Missing(EN, used); len(got) != len(used) {
		t.Errorf("Missing(en) = %v, want every key", got)
	}
	if got := Orphans(EN, used); len(got) != 0 {
		t.Errorf("Orphans(en) = %v, want none", got)
	}
}

func TestEverySupportedLanguageIsNamedInItsOwnWords(t *testing.T) {
	// A reader looking for Turkish is looking for "Türkçe", not for "Turkish".
	for _, lang := range Supported {
		name, ok := Name[lang]
		if !ok || strings.TrimSpace(name) == "" {
			t.Errorf("%q has no name for the picker", lang)
		}
	}
}

func TestDefaultIsSupported(t *testing.T) {
	if !Valid(string(Default)) {
		t.Fatalf("the default language %q is not in Supported", Default)
	}
}

func TestDynamicKeysAreDeclaredOnce(t *testing.T) {
	// The sync test unions this list with what it scans out of the templates. A
	// duplicate is harmless there and is a sign the list is being appended to
	// without being read, which is how a stale entry survives.
	seen := map[string]bool{}
	for _, key := range Dynamic {
		if seen[key] {
			t.Errorf("%q is listed twice in Dynamic", key)
		}
		seen[key] = true
	}
}

func TestEveryDynamicKeyIsTranslated(t *testing.T) {
	// These are reached through {{ t . }} and are invisible to a scan of the
	// templates, so nothing else would notice them going untranslated.
	if missing := Missing(TR, Dynamic); len(missing) != 0 {
		t.Errorf("dynamic keys with no Turkish translation: %v", missing)
	}
}
