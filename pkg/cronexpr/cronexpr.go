// Package cronexpr parses and evaluates five field cron expressions.
//
// Why not a library. The dispatcher does not ask "when does this fire next",
// it asks "does this expression fall on this exact minute", and it asks that
// for a minute it may be replaying after an outage. Schedule.Next() from a
// scheduling library answers a different question, and building Matches on top
// of it (Next(minute-1s) == minute) is a subtle way of getting the day-of-week
// rule wrong. The set based form here answers both questions from one parse.
//
// Grammar, deliberately narrow:
//
//   - five fields: minute hour day-of-month month day-of-week
//   - each field: *  |  n  |  a-b  |  a-b/s  |  */s  |  n/s, comma separated
//   - day-of-week accepts 0-7, where both 0 and 7 mean Sunday
//   - NOT supported, on purpose: @daily style descriptors, a seconds field,
//     and the Quartz extensions L, W and #
//
// An invalid expression returns an error. It never degrades into an expression
// that simply never matches: a job that silently never runs is the one failure
// nobody notices.
package cronexpr

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type fieldBounds struct {
	min, max int
	name     string
}

var bounds = [5]fieldBounds{
	{0, 59, "minute"},
	{0, 23, "hour"},
	{1, 31, "day of month"},
	{1, 12, "month"},
	{0, 7, "day of week"},
}

const (
	fieldMinute = iota
	fieldHour
	fieldDayOfMonth
	fieldMonth
	fieldDayOfWeek
)

// Expr is a parsed expression. Parsing is paid once and Matches is called
// every minute for every job, so the parsed form is what gets cached.
type Expr struct {
	raw  [5]string
	sets [5]map[int]bool
	// star records whether a field was written as exactly "*". The Vixie day
	// rule turns on that literal form: "*/1" covers every value but does not
	// count as "*" for the purpose of the rule.
	star [5]bool
}

// Parse turns an expression into its evaluated form.
func Parse(expr string) (*Expr, error) {
	normalized := strings.Join(strings.Fields(expr), " ")
	if normalized == "" {
		return nil, fmt.Errorf("expression is empty")
	}

	fields := strings.Split(normalized, " ")
	if len(fields) != 5 {
		return nil, fmt.Errorf("expression must have 5 fields, found %d", len(fields))
	}

	out := &Expr{}
	for i, field := range fields {
		set, err := parseField(field, bounds[i])
		if err != nil {
			return nil, err
		}
		out.raw[i] = field
		out.sets[i] = set
		out.star[i] = field == "*"
	}

	// 7 and 0 are the same day; fold so Matches can index by time.Weekday.
	if out.sets[fieldDayOfWeek][7] {
		delete(out.sets[fieldDayOfWeek], 7)
		out.sets[fieldDayOfWeek][0] = true
	}
	return out, nil
}

// Validate reports whether an expression can be parsed. It is what the form
// calls before saving.
func Validate(expr string) error {
	_, err := Parse(expr)
	return err
}

func parseField(field string, b fieldBounds) (map[int]bool, error) {
	if field == "" {
		return nil, fmt.Errorf("%s field is empty", b.name)
	}
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		if err := parsePart(part, b, set); err != nil {
			return nil, err
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("%s field %q matches no value", b.name, field)
	}
	return set, nil
}

func parsePart(part string, b fieldBounds, set map[int]bool) error {
	if part == "" {
		return fmt.Errorf("%s field has an empty element", b.name)
	}

	step := 1
	if i := strings.Index(part, "/"); i >= 0 {
		stepText := part[i+1:]
		part = part[:i]
		n, err := strconv.Atoi(stepText)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s field has an invalid step: %q", b.name, stepText)
		}
		step = n
	}

	var start, end int
	switch {
	case part == "*":
		start, end = b.min, b.max

	case strings.Contains(part, "-"):
		side := strings.SplitN(part, "-", 2)
		a, err1 := strconv.Atoi(side[0])
		z, err2 := strconv.Atoi(side[1])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("%s field has an invalid range: %q", b.name, part)
		}
		if a > z {
			return fmt.Errorf("%s field has a reversed range: %q", b.name, part)
		}
		start, end = a, z

	default:
		n, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("%s field has an invalid value: %q", b.name, part)
		}
		// A bare value with a step ("5/10") runs to the end of the range.
		start = n
		if step > 1 {
			end = b.max
		} else {
			end = n
		}
	}

	if start < b.min || end > b.max {
		return fmt.Errorf("%s field must be within %d-%d: %q", b.name, b.min, b.max, part)
	}

	for v := start; v <= end; v += step {
		set[v] = true
	}
	return nil
}

// Matches reports whether the given moment falls on this expression. Seconds
// and below are ignored; cron has minute resolution.
func (e *Expr) Matches(t time.Time) bool {
	if !e.sets[fieldMinute][t.Minute()] {
		return false
	}
	if !e.sets[fieldHour][t.Hour()] {
		return false
	}
	if !e.sets[fieldMonth][int(t.Month())] {
		return false
	}
	return e.dayMatches(t.Day(), int(t.Weekday()))
}

// dayMatches applies the Vixie rule. When day-of-month and day-of-week are
// both restricted the two are joined by OR ("the 1st of the month, or any
// Monday"); when either is "*" the other one decides alone.
//
// It reads as a bug and is the standard. Both Matches and NextRun go through
// here so the "next run" shown on screen can never disagree with the day the
// job actually fires.
func (e *Expr) dayMatches(dayOfMonth, dayOfWeek int) bool {
	domMatch := e.sets[fieldDayOfMonth][dayOfMonth]
	dowMatch := e.sets[fieldDayOfWeek][dayOfWeek]
	domStar := e.star[fieldDayOfMonth]
	dowStar := e.star[fieldDayOfWeek]

	switch {
	case domStar && dowStar:
		return true
	case domStar:
		return dowMatch
	case dowStar:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

// DefaultScanDays bounds how far NextRun looks ahead. Beyond a year an
// expression that has not matched never will: "30 February" is possible to
// write and impossible to reach.
const DefaultScanDays = 366

// NextRun returns the first firing strictly after from.
//
// The second value reports whether one was found inside the scan window.
// Returning a zero time for an unreachable expression would be read as "runs
// at midnight", which is how an unreachable schedule stays unnoticed.
func (e *Expr) NextRun(from time.Time, maxDays int) (time.Time, bool) {
	if maxDays <= 0 {
		maxDays = DefaultScanDays
	}

	start := time.Date(from.Year(), from.Month(), from.Day(),
		from.Hour(), from.Minute(), 0, 0, from.Location()).Add(time.Minute)

	hours := sortedKeys(e.sets[fieldHour])
	minutes := sortedKeys(e.sets[fieldMinute])

	day := start
	for i := 0; i <= maxDays; i++ {
		if e.sets[fieldMonth][int(day.Month())] && e.dayMatches(day.Day(), int(day.Weekday())) {
			for _, hour := range hours {
				for _, minute := range minutes {
					candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())
					if !candidate.Before(start) {
						return candidate, true
					}
				}
			}
		}
		// Step to the start of the next day rather than adding 24 hours, so a
		// daylight saving transition does not shift the clock.
		next := day.AddDate(0, 0, 1)
		day = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, day.Location())
	}

	return time.Time{}, false
}

// NextRuns returns up to n upcoming firings. The job form shows these while
// the expression is being typed, which is the only moment a wrong expression
// is cheap to notice.
func (e *Expr) NextRuns(from time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	cursor := from
	for i := 0; i < n; i++ {
		next, ok := e.NextRun(cursor, DefaultScanDays)
		if !ok {
			break
		}
		out = append(out, next)
		cursor = next
	}
	return out
}

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// Raw returns the expression with whitespace normalised.
func (e *Expr) Raw() string { return strings.Join(e.raw[:], " ") }

// Describe renders a short human reading of an expression. It covers the
// shapes people actually write and falls back to the raw text rather than
// inventing a sentence for an exotic one; a wrong description is worse than
// none on a screen whose whole job is to catch mistakes.
func Describe(expr string) string {
	e, err := Parse(expr)
	if err != nil {
		return expr
	}

	min, hour, dom, month, dow := e.raw[0], e.raw[1], e.raw[2], e.raw[3], e.raw[4]
	everyDay := dom == "*" && month == "*" && dow == "*"

	switch {
	case min == "*" && hour == "*" && everyDay:
		return "every minute"
	case strings.HasPrefix(min, "*/") && hour == "*" && everyDay:
		return "every " + strings.TrimPrefix(min, "*/") + " minutes"
	case hour == "*" && everyDay && isNumber(min):
		return "hourly at minute " + min
	case everyDay && isNumber(min) && isNumber(hour):
		return "daily at " + clock(hour, min)
	case dom == "*" && month == "*" && isNumber(min) && isNumber(hour):
		return "at " + clock(hour, min) + " on " + weekdayNames(dow)
	case month == "*" && dow == "*" && isNumber(min) && isNumber(hour) && isNumber(dom):
		return "at " + clock(hour, min) + " on day " + dom + " of the month"
	}
	return e.Raw()
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func clock(hour, minute string) string {
	h, _ := strconv.Atoi(hour)
	m, _ := strconv.Atoi(minute)
	return fmt.Sprintf("%02d:%02d", h, m)
}

var dayNames = [8]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

func weekdayNames(dow string) string {
	parts := strings.Split(dow, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 7 {
			return dow
		}
		out = append(out, dayNames[n])
	}
	return strings.Join(out, ", ")
}
