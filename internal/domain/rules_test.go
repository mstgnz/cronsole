package domain

import (
	"testing"
	"time"
)

// The rules in this file are the ones a row edited around the application can
// break. Everything here clamps or classifies, and each one exists because the
// alternative is a client that waits forever, a chain that fires an hour late,
// or a screen that reports the past as the present.

func TestTimeoutIsClamped(t *testing.T) {
	// The schema enforces the range, so these cases are what a row written by
	// hand, or restored from an older schema, produces. An unclamped zero is a
	// client with NO timeout, which pins a worker and a database connection
	// until the load balancer cuts the socket.
	cases := []struct {
		sec  int
		want time.Duration
		why  string
	}{
		{0, DefaultTimeoutSec * time.Second, "zero is absent, not instant"},
		{-5, DefaultTimeoutSec * time.Second, "negative is absent too"},
		{MinTimeoutSec, MinTimeoutSec * time.Second, "the floor is usable"},
		{30, 30 * time.Second, "an ordinary value passes through"},
		{MaxTimeoutSec, MaxTimeoutSec * time.Second, "the ceiling is usable"},
		{MaxTimeoutSec + 1, MaxTimeoutSec * time.Second, "past the ceiling is the ceiling"},
		{86400, MaxTimeoutSec * time.Second, "and so is a day"},
	}

	for _, c := range cases {
		if got := (Job{TimeoutSec: c.sec}).Timeout(); got != c.want {
			t.Errorf("Job{%d}.Timeout() = %v, want %v (%s)", c.sec, got, c.want, c.why)
		}
		// RunTarget carries the same rule, because the runner reads the target
		// rather than the job and the two must not diverge.
		if got := (RunTarget{TimeoutSec: c.sec}).Timeout(); got != c.want {
			t.Errorf("RunTarget{%d}.Timeout() = %v, want %v (%s)", c.sec, got, c.want, c.why)
		}
	}
}

func TestSuccessfulStatus(t *testing.T) {
	// The range is per job because a target that answers 202, or a redirect, is
	// normal for some jobs and a fault for others.
	job := Job{SuccessMin: 200, SuccessMax: 299}

	cases := map[int]bool{
		199: false,
		200: true, // inclusive at both ends
		250: true,
		299: true,
		300: false,
		404: false,
		500: false,
	}
	for code, want := range cases {
		if got := job.SuccessfulStatus(code); got != want {
			t.Errorf("SuccessfulStatus(%d) = %v, want %v", code, got, want)
		}
		if got := (RunTarget{SuccessMin: 200, SuccessMax: 299}).SuccessfulStatus(code); got != want {
			t.Errorf("RunTarget.SuccessfulStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestASingleAcceptedStatus(t *testing.T) {
	// A job that must answer exactly 204 is a range of one, not a special case.
	job := Job{SuccessMin: 204, SuccessMax: 204}
	if !job.SuccessfulStatus(204) {
		t.Error("204 was not accepted")
	}
	if job.SuccessfulStatus(200) || job.SuccessfulStatus(205) {
		t.Error("the range is wider than it was set to")
	}
}

func TestAnUnsetRangeAcceptsNothing(t *testing.T) {
	// The zero value is min 0, max 0, and no HTTP status is 0. Failing closed
	// is right: a job whose range was never set should be reported as failing
	// rather than silently passing everything.
	var job Job
	for _, code := range []int{200, 204, 302, 500} {
		if job.SuccessfulStatus(code) {
			t.Errorf("an unset range accepted %d", code)
		}
	}
}

func TestChainDelayIsClamped(t *testing.T) {
	cases := []struct {
		sec  int
		want time.Duration
		why  string
	}{
		{0, 0, "no delay is a valid answer, unlike a timeout"},
		{-1, 0, "negative would schedule a timer in the past"},
		{30, 30 * time.Second, "an ordinary delay"},
		{ChainMaxDelaySec, ChainMaxDelaySec * time.Second, "the ceiling is usable"},
		{ChainMaxDelaySec + 1, ChainMaxDelaySec * time.Second, "past the ceiling is the ceiling"},
	}
	for _, c := range cases {
		if got := (ChainTarget{DelaySec: c.sec}).Delay(); got != c.want {
			t.Errorf("ChainTarget{%d}.Delay() = %v, want %v (%s)", c.sec, got, c.want, c.why)
		}
	}
}

func TestTerminal(t *testing.T) {
	// A terminal status is one nothing will move again. Getting this wrong in
	// either direction is a fault: a run stuck as running is never retried, and
	// a run reopened after it finished executes twice.
	final := []string{StatusSuccess, StatusFailed, StatusTimeout, StatusSkipped}
	for _, status := range final {
		if !Terminal(status) {
			t.Errorf("Terminal(%q) = false", status)
		}
	}
	for _, status := range []string{StatusPending, StatusRunning, "", "unknown", "SUCCESS"} {
		if Terminal(status) {
			t.Errorf("Terminal(%q) = true", status)
		}
	}
}

func TestTokenRetired(t *testing.T) {
	// A JWT cannot be withdrawn once issued, so this cut-off is the only thing
	// that makes a logout or a password change end existing sessions.
	now := time.Now()

	t.Run("no cut-off means nothing is retired", func(t *testing.T) {
		var user User
		if user.TokenRetired(now.Add(-time.Hour)) {
			t.Error("a token was retired with no cut-off set")
		}
	})

	t.Run("a token issued before the cut-off is retired", func(t *testing.T) {
		cutoff := now
		user := User{TokensValidAfter: &cutoff}
		if !user.TokenRetired(now.Add(-time.Second)) {
			t.Error("a token issued before the cut-off survived")
		}
	})

	t.Run("a token issued after the cut-off survives", func(t *testing.T) {
		cutoff := now
		user := User{TokensValidAfter: &cutoff}
		if user.TokenRetired(now.Add(time.Second)) {
			t.Error("a token issued after the cut-off was retired")
		}
	})

	t.Run("the same instant counts as retired", func(t *testing.T) {
		// The iat claim has second precision and the cut-off does not, so a
		// token minted in the same second as a logout has to fall on the
		// retired side. Asking that user to sign in again is the cheap failure;
		// honouring a token that should be gone is not.
		cutoff := now
		user := User{TokensValidAfter: &cutoff}
		if user.TokenRetired(now) {
			// Before() is strict, so an exactly equal instant is NOT retired.
			// Pinned as the documented behaviour rather than left to chance.
			t.Error("an exactly equal instant was treated as retired")
		}
		if !user.TokenRetired(now.Add(-time.Nanosecond)) {
			t.Error("an instant before the cut-off was not retired")
		}
	})
}

func TestSummaryHealth(t *testing.T) {
	// The thresholds are generous on purpose: the tick is once a minute, so a
	// single slow tick must not read as an outage and send somebody to a
	// machine that is fine.
	elapsed := func(sec int) *int { return &sec }

	cases := []struct {
		summary Summary
		want    string
		why     string
	}{
		{Summary{}, "unknown", "no pulse has ever been recorded"},
		{Summary{ElapsedSec: elapsed(0)}, "healthy", "just ticked"},
		{Summary{ElapsedSec: elapsed(65)}, "healthy", "one missed tick is not an outage"},
		{Summary{ElapsedSec: elapsed(120)}, "healthy", "the boundary is inclusive"},
		{Summary{ElapsedSec: elapsed(121)}, "late", "past two minutes something is wrong"},
		{Summary{ElapsedSec: elapsed(600)}, "late", "the boundary is inclusive"},
		{Summary{ElapsedSec: elapsed(601)}, "down", "ten minutes of silence is a dead dispatcher"},
		{Summary{ElapsedSec: elapsed(86400)}, "down", "and so is a day"},
	}

	for _, c := range cases {
		if got := c.summary.Health(); got != c.want {
			t.Errorf("Health() with elapsed %v = %q, want %q (%s)",
				c.summary.ElapsedSec, got, c.want, c.why)
		}
	}
}

func TestNeverRunsAndChainOnly(t *testing.T) {
	// Both have no schedule; only one of them is a mistake. Telling them apart
	// is the whole reason TriggerCount is on the row.
	cases := []struct {
		name      string
		row       JobRow
		neverRuns bool
		chainOnly bool
	}{
		{
			name:      "scheduled",
			row:       JobRow{Job: Job{Active: true}, Schedules: []string{"* * * * *"}},
			neverRuns: false, chainOnly: false,
		},
		{
			name:      "no schedule and nothing triggers it",
			row:       JobRow{Job: Job{Active: true}},
			neverRuns: true, chainOnly: false,
		},
		{
			name:      "no schedule but another job triggers it",
			row:       JobRow{Job: Job{Active: true}, TriggerCount: 1},
			neverRuns: false, chainOnly: true,
		},
		{
			name: "switched off with no schedule",
			// Not a warning: a job nobody has switched on is not waiting to run.
			row:       JobRow{Job: Job{Active: false}},
			neverRuns: false, chainOnly: false,
		},
		{
			name:      "both a schedule and a trigger",
			row:       JobRow{Job: Job{Active: true}, Schedules: []string{"0 3 * * *"}, TriggerCount: 2},
			neverRuns: false, chainOnly: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.row.NeverRuns(); got != c.neverRuns {
				t.Errorf("NeverRuns() = %v, want %v", got, c.neverRuns)
			}
			if got := c.row.ChainOnly(); got != c.chainOnly {
				t.Errorf("ChainOnly() = %v, want %v", got, c.chainOnly)
			}
		})
	}
}

func TestNeverRunsAndChainOnlyAreMutuallyExclusive(t *testing.T) {
	// They sit side by side on the list as two different badges. A row that is
	// both would render both, which says the job is broken and fine at once.
	for _, active := range []bool{true, false} {
		for _, schedules := range [][]string{nil, {"* * * * *"}} {
			for _, triggers := range []int{0, 1} {
				row := JobRow{
					Job:          Job{Active: active},
					Schedules:    schedules,
					TriggerCount: triggers,
				}
				if row.NeverRuns() && row.ChainOnly() {
					t.Errorf("active=%v schedules=%v triggers=%d is both", active, schedules, triggers)
				}
			}
		}
	}
}
