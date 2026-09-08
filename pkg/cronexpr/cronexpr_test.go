package cronexpr

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Expr {
	t.Helper()
	parsed, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", expr, err)
	}
	return parsed
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04", value)
	if err != nil {
		t.Fatalf("bad test time %q: %v", value, err)
	}
	return parsed
}

func TestParseRejectsMalformed(t *testing.T) {
	// Every one of these has to be an ERROR, not an expression that quietly
	// never matches. A job that silently never runs is the failure nobody
	// notices, which is the whole reason Parse returns an error at all.
	cases := []string{
		"",
		"* * * *",      // four fields
		"* * * * * *",  // six fields, a seconds dialect we do not accept
		"@daily",       // descriptor
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // day of month starts at 1
		"* * * 13 *",   // month out of range
		"* * * * 8",    // day of week is 0-7
		"5-1 * * * *",  // reversed range
		"*/0 * * * *",  // zero step
		"*/x * * * *",  // non numeric step
		"a * * * *",    // non numeric value
		"1,,2 * * * *", // empty element
		"* * * * mon",  // names are not accepted
		"0 0 L * *",    // Quartz extension
	}
	for _, expr := range cases {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted an invalid expression", expr)
		}
	}
}

func TestParseAcceptsRealExpressions(t *testing.T) {
	// Shapes taken from real crontabs. If any of these stops parsing, jobs
	// stop being queued.
	for _, expr := range []string{
		"* * * * *",
		"*/5 * * * *",
		"0 3 * * *",
		"58,59 15 * * *",
		"0-10,28,29 16 * * *",
		"0 9 * * 1-5",
		"30 8 1 * *",
		"0 */6 * * *",
		"5/10 * * * *",
		"0 0 * * 7",
	} {
		if _, err := Parse(expr); err != nil {
			t.Errorf("Parse(%q) rejected a valid expression: %v", expr, err)
		}
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		expr string
		when string
		want bool
	}{
		{"* * * * *", "2026-03-04 10:11", true},
		{"*/5 * * * *", "2026-03-04 10:10", true},
		{"*/5 * * * *", "2026-03-04 10:11", false},
		{"0 3 * * *", "2026-03-04 03:00", true},
		{"0 3 * * *", "2026-03-04 03:01", false},
		{"58,59 15 * * *", "2026-03-04 15:58", true},
		{"58,59 15 * * *", "2026-03-04 15:57", false},
		{"0-10,28,29 16 * * *", "2026-03-04 16:28", true},
		{"0-10,28,29 16 * * *", "2026-03-04 16:11", false},
		{"5/10 * * * *", "2026-03-04 10:25", true},
		{"5/10 * * * *", "2026-03-04 10:20", false},
	}
	for _, c := range cases {
		got := mustParse(t, c.expr).Matches(at(t, c.when))
		if got != c.want {
			t.Errorf("%q.Matches(%s) = %v, want %v", c.expr, c.when, got, c.want)
		}
	}
}

func TestSundayIsBothZeroAndSeven(t *testing.T) {
	// 2026-03-01 is a Sunday.
	sunday := at(t, "2026-03-01 00:00")
	for _, expr := range []string{"0 0 * * 0", "0 0 * * 7"} {
		if !mustParse(t, expr).Matches(sunday) {
			t.Errorf("%q did not match a Sunday", expr)
		}
	}
}

func TestVixieDayRule(t *testing.T) {
	// The rule reads as a bug and is the standard: when day-of-month and
	// day-of-week are BOTH restricted they are joined by OR, and when either
	// is "*" the other decides alone. Getting this wrong fires jobs on the
	// wrong days, quietly.
	//
	// 2026-03-01 is a Sunday, 2026-03-02 a Monday, 2026-03-15 a Sunday.
	cases := []struct {
		name string
		expr string
		when string
		want bool
	}{
		{"both restricted, day of month hits", "0 0 15 * 1", "2026-03-15 00:00", true},
		{"both restricted, weekday hits", "0 0 15 * 1", "2026-03-02 00:00", true},
		{"both restricted, neither hits", "0 0 15 * 1", "2026-03-03 00:00", false},
		{"weekday star, day of month decides", "0 0 15 * *", "2026-03-15 00:00", true},
		{"weekday star, other day", "0 0 15 * *", "2026-03-16 00:00", false},
		{"day of month star, weekday decides", "0 0 * * 1", "2026-03-02 00:00", true},
		{"day of month star, other weekday", "0 0 * * 1", "2026-03-03 00:00", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustParse(t, c.expr).Matches(at(t, c.when)); got != c.want {
				t.Errorf("%q on %s = %v, want %v", c.expr, c.when, got, c.want)
			}
		})
	}
}

func TestVixieStarIsLiteral(t *testing.T) {
	// "*/1" covers every value but is NOT "*" for the Vixie rule, and the
	// difference is observable: with a restricted weekday, "*/1" in the
	// day-of-month field makes the two fields OR together, so every day
	// matches.
	expr := mustParse(t, "0 0 */1 * 1")
	if !expr.Matches(at(t, "2026-03-03 00:00")) {
		t.Error(`"*/1" in day-of-month should not behave as "*", so a Tuesday should still match through the OR`)
	}

	star := mustParse(t, "0 0 * * 1")
	if star.Matches(at(t, "2026-03-03 00:00")) {
		t.Error(`"*" in day-of-month should let the weekday decide alone`)
	}
}

func TestNextRun(t *testing.T) {
	from := at(t, "2026-03-04 10:07")

	cases := []struct {
		expr string
		want string
	}{
		{"* * * * *", "2026-03-04 10:08"},
		{"*/5 * * * *", "2026-03-04 10:10"},
		{"0 3 * * *", "2026-03-05 03:00"},
		{"0 9 * * 1-5", "2026-03-05 09:00"},
		{"0 0 1 * *", "2026-04-01 00:00"},
	}
	for _, c := range cases {
		got, ok := mustParse(t, c.expr).NextRun(from, DefaultScanDays)
		if !ok {
			t.Errorf("%q found no next run", c.expr)
			continue
		}
		if want := at(t, c.want); !got.Equal(want) {
			t.Errorf("%q.NextRun = %s, want %s", c.expr, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

func TestNextRunIsStrictlyAfter(t *testing.T) {
	// The current minute is never the answer. If it were, the "next run" line
	// on the form would show the minute that is already firing.
	now := at(t, "2026-03-04 10:10")
	got, ok := mustParse(t, "*/5 * * * *").NextRun(now, DefaultScanDays)
	if !ok {
		t.Fatal("no next run found")
	}
	if !got.After(now) {
		t.Errorf("NextRun returned %s, which is not after %s", got, now)
	}
}

func TestNextRunUnreachable(t *testing.T) {
	// "30 February" parses and can never happen. Reporting false is what stops
	// the interface printing a zero time and reading it as midnight.
	if _, ok := mustParse(t, "0 0 30 2 *").NextRun(at(t, "2026-03-04 10:00"), DefaultScanDays); ok {
		t.Error("an unreachable expression reported a next run")
	}
}

func TestNextRunsAreOrderedAndDistinct(t *testing.T) {
	runs := mustParse(t, "*/15 * * * *").NextRuns(at(t, "2026-03-04 10:07"), 4)
	if len(runs) != 4 {
		t.Fatalf("got %d runs, want 4", len(runs))
	}
	for i := 1; i < len(runs); i++ {
		if !runs[i].After(runs[i-1]) {
			t.Errorf("run %d (%s) is not after run %d (%s)", i, runs[i], i-1, runs[i-1])
		}
	}
}

func TestMatchesAgreesWithNextRun(t *testing.T) {
	// The two answers come from one day rule on purpose. If they ever disagree
	// the screen shows a day the job does not actually run on, which is worse
	// than showing nothing.
	from := at(t, "2026-01-01 00:00")
	for _, text := range []string{
		"*/7 * * * *", "0 0 15 * 1", "0 9 * * 1-5", "30 8 1 * *", "0 0 * * 0",
	} {
		expr := mustParse(t, text)
		next, ok := expr.NextRun(from, DefaultScanDays)
		if !ok {
			t.Errorf("%q: no next run", text)
			continue
		}
		if !expr.Matches(next) {
			t.Errorf("%q: NextRun returned %s, which Matches rejects", text, next)
		}
	}
}

func TestRawNormalises(t *testing.T) {
	if got := mustParse(t, "  */5   *  * *   * ").Raw(); got != "*/5 * * * *" {
		t.Errorf("Raw() = %q, want %q", got, "*/5 * * * *")
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"* * * * *":   "every minute",
		"*/5 * * * *": "every 5 minutes",
		"0 * * * *":   "hourly at minute 0",
		"0 3 * * *":   "daily at 03:00",
		"30 8 * * 1":  "at 08:30 on Monday",
		"0 2 1 * *":   "at 02:00 on day 1 of the month",
	}
	for expr, want := range cases {
		if got := Describe(expr); got != want {
			t.Errorf("Describe(%q) = %q, want %q", expr, got, want)
		}
	}

	// An expression it cannot phrase falls back to the expression itself,
	// rather than inventing a sentence. A wrong description is worse than none
	// on a screen whose job is catching mistakes.
	if got := Describe("0-10,28,29 16 * * *"); got != "0-10,28,29 16 * * *" {
		t.Errorf("Describe fell back to %q, want the raw expression", got)
	}
	// An invalid expression is echoed back rather than panicking.
	if got := Describe("nonsense"); got != "nonsense" {
		t.Errorf("Describe(invalid) = %q, want the input back", got)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("*/5 * * * *"); err != nil {
		t.Errorf("Validate rejected a valid expression: %v", err)
	}
	if err := Validate("*/5 * * *"); err == nil {
		t.Error("Validate accepted a four field expression")
	}
}
