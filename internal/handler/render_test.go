package handler

import (
	"html/template"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

// The template helpers, tested directly.
//
// They are pure functions and they decide what every number on the screen
// actually says: whether an unknown reading draws an empty bar or a full one,
// whether a duration reads as "0s" or "table 840 ms", whether a nil pointer
// prints as a blank or as the word nil. A wrong answer here is not a crash, it
// is a panel that quietly says something untrue, which is worse.

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{uint64(0), "0 B"},
		{uint64(512), "512 B"},
		{uint64(1024), "1.0 KB"},
		{uint64(1536), "1.5 KB"},
		{uint64(10 * 1024), "10 KB"},
		{uint64(1024 * 1024), "1.0 MB"},
		{uint64(9_500_000_000), "8.8 GB"},
		// Above a terabyte the exponent stops climbing, so a very large disk
		// still reads in TB rather than in a unit with no letter.
		{uint64(5 << 40), "5.0 TB"},
		{int64(2048), "2.0 KB"},
		{int(4096), "4.0 KB"},
		// Not a byte count at all: blank, never a zero that reads as measured.
		{int64(-1), ""},
		{"1024", ""},
		{nil, ""},
	}

	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanDurationKeepsSubSecondPrecision(t *testing.T) {
	// Most jobs finish under a second. Rounding them all to "0s" would hide the
	// difference between a fast job and a broken one.
	cases := []struct {
		in   any
		want string
	}{
		{0, "0 ms"},
		{840, "840 ms"},
		{999, "999 ms"},
		{1000, "1.0 s"},
		{59999, "60.0 s"},
		{60000, "1m 0s"},
		{3_725_000, "62m 5s"},
		{int64(1500), "1.5 s"},
		{nil, ""},
		{"1500", ""},
	}

	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanSpan(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{3 * time.Second, "3s"},
		{59 * time.Second, "59s"},
		{90 * time.Second, "1m"},
		{2 * time.Hour, "2h"},
		{47 * time.Hour, "47h"},
		// Two days is where hours stop being readable at a glance.
		{50 * time.Hour, "2d"},
		{20 * 24 * time.Hour, "20d"},
	}

	for _, c := range cases {
		if got := humanSpan(c.in); got != c.want {
			t.Errorf("humanSpan(%v) = %q, want %q", c.in, got, c.want)
		}
	}

	if got := humanSpanSeconds(uint64(3600)); got != "1h" {
		t.Errorf("humanSpanSeconds(3600) = %q, want 1h", got)
	}
	if got := humanSpanSeconds("3600"); got != "" {
		t.Errorf("humanSpanSeconds of a non-number = %q, want blank", got)
	}
}

func TestAgoIsRelativeAndTranslated(t *testing.T) {
	r := &Renderer{location: time.UTC}

	if got := r.ago(i18n.EN, nil); got != "never" {
		t.Errorf("ago(nil) = %q, want never", got)
	}
	if got := r.ago(i18n.EN, time.Now()); got != "just now" {
		t.Errorf("ago(now) = %q, want just now", got)
	}
	if got := r.ago(i18n.EN, time.Now().Add(-5*time.Minute)); got != "5m ago" {
		t.Errorf("ago(5 minutes back) = %q, want 5m ago", got)
	}
	// A scheduled next run is in the future, and reading "-3m ago" would look
	// like a bug in the scheduler rather than in the phrasing.
	if got := r.ago(i18n.EN, time.Now().Add(3*time.Minute)); got != "in 2m" && got != "in 3m" {
		t.Errorf("ago(3 minutes ahead) = %q, want an \"in\" phrase", got)
	}
	// The number keeps its unit; only the framing moves.
	if got := r.ago(i18n.TR, time.Now().Add(-5*time.Minute)); got == "5m ago" || got == "" {
		t.Errorf("ago in Turkish = %q, want the translated framing around 5m", got)
	}
}

func TestFormatTimeUsesTheConfiguredZone(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Skipf("no zone data: %v", err)
	}
	r := &Renderer{location: istanbul}

	at := time.Date(2026, 9, 8, 9, 30, 0, 0, time.UTC)
	if got := r.formatTime(at, "15:04"); got != "12:30" {
		t.Errorf("formatTime = %q, want 12:30 in Istanbul", got)
	}
	if got := r.formatTime(&at, "2006-01-02"); got != "2026-09-08" {
		t.Errorf("formatTime through a pointer = %q, want the date", got)
	}
	// A zero or missing time is blank, not the year 1.
	if got := r.formatTime(time.Time{}, "15:04"); got != "" {
		t.Errorf("formatTime of a zero time = %q, want blank", got)
	}
	if got := r.formatTime(nil, "15:04"); got != "" {
		t.Errorf("formatTime of nil = %q, want blank", got)
	}
}

func TestBarWidthIsBoundedAndSafe(t *testing.T) {
	cases := []struct {
		in   any
		want template.CSS
	}{
		{0.0, "0.0%"},
		{42.5, "42.5%"},
		{100.0, "100.0%"},
		// A reading over the quota can happen across a period boundary. The bar
		// stops at full rather than overflowing its container.
		{130.0, "100.0%"},
		// Unknown draws nothing. A full bar would read as a machine in trouble.
		{-1.0, "0%"},
		{nil, "0%"},
		{"75", "0%"},
		{75, "75.0%"},
	}

	for _, c := range cases {
		if got := barWidth(c.in); got != c.want {
			t.Errorf("barWidth(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUsageClassIsQuietUntilItMatters(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{10.0, ""},
		{74.9, ""},
		{75.0, "bg-warning"},
		{89.9, "bg-warning"},
		{90.0, "bg-danger"},
		{100.0, "bg-danger"},
		// Unknown is grey, not green: "we could not read this" must not look
		// like "this is fine".
		{-1.0, "bg-secondary"},
		{nil, "bg-secondary"},
	}

	for _, c := range cases {
		if got := usageClass(c.in); got != c.want {
			t.Errorf("usageClass(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPercentRefusesToDivideByZero(t *testing.T) {
	if got := percent(3, 0); got != 0 {
		t.Errorf("percent(3, 0) = %d, want 0", got)
	}
	if got := percent(25, 200); got != 12 {
		t.Errorf("percent(25, 200) = %d, want 12", got)
	}
}

func TestTheConverters(t *testing.T) {
	now := time.Now()
	var nilTime *time.Time
	var nilInt *int
	var nilInt64 *int64
	var nilString *string
	seven, seven64, word := 7, int64(7), "seven"

	if _, ok := asTime(now); !ok {
		t.Error("asTime rejected a real time")
	}
	if _, ok := asTime(&now); !ok {
		t.Error("asTime rejected a pointer to a real time")
	}
	for _, v := range []any{nilTime, time.Time{}, "2026-01-01", nil} {
		if _, ok := asTime(v); ok {
			t.Errorf("asTime(%v) accepted something that is not a time", v)
		}
	}

	if n, ok := asInt(int64(9)); !ok || n != 9 {
		t.Errorf("asInt(int64 9) = (%d, %v)", n, ok)
	}
	if n, ok := asInt(&seven); !ok || n != 7 {
		t.Errorf("asInt(*int 7) = (%d, %v)", n, ok)
	}
	if _, ok := asInt(nilInt); ok {
		t.Error("asInt accepted a nil pointer")
	}
	if _, ok := asInt("9"); ok {
		t.Error("asInt accepted a string")
	}

	if n, ok := asUint64(int(5)); !ok || n != 5 {
		t.Errorf("asUint64(5) = (%d, %v)", n, ok)
	}
	if n, ok := asUint64(uint64(5)); !ok || n != 5 {
		t.Errorf("asUint64(uint64 5) = (%d, %v)", n, ok)
	}
	// Negative is not a byte count, and wrapping it would print sixteen
	// exabytes.
	for _, v := range []any{int(-1), int64(-1), "5", nil} {
		if _, ok := asUint64(v); ok {
			t.Errorf("asUint64(%v) accepted a value that is not a size", v)
		}
	}

	if f, ok := asFloat(int64(3)); !ok || f != 3 {
		t.Errorf("asFloat(int64 3) = (%v, %v)", f, ok)
	}
	if f, ok := asFloat(2.5); !ok || f != 2.5 {
		t.Errorf("asFloat(2.5) = (%v, %v)", f, ok)
	}
	if _, ok := asFloat("2.5"); ok {
		t.Error("asFloat accepted a string")
	}

	// deref is what keeps a nil pointer from rendering as the word nil in the
	// middle of a table.
	if got := deref(&seven); got != 7 {
		t.Errorf("deref(*int) = %v, want 7", got)
	}
	if got := deref(nilInt); got != 0 {
		t.Errorf("deref(nil *int) = %v, want 0", got)
	}
	if got := deref(&seven64); got != int64(7) {
		t.Errorf("deref(*int64) = %v, want 7", got)
	}
	if got := deref(nilInt64); got != int64(0) {
		t.Errorf("deref(nil *int64) = %v, want 0", got)
	}
	if got := deref(&word); got != "seven" {
		t.Errorf("deref(*string) = %v, want seven", got)
	}
	if got := deref(nilString); got != "" {
		t.Errorf("deref(nil *string) = %v, want a blank string", got)
	}
	if got := deref(42); got != 42 {
		t.Errorf("deref of a plain value = %v, want it unchanged", got)
	}
}
