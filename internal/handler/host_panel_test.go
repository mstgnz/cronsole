package handler

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/hostinfo"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

// renderHostPanel executes the panel on its own, which is what the dashboard
// does with it.
func renderHostPanel(t *testing.T, lang i18n.Lang, stats hostinfo.Stats) string {
	t.Helper()

	r, err := NewRenderer(time.UTC, applog.New(), "test")
	if err != nil {
		t.Fatalf("templates did not compile: %v", err)
	}
	set, ok := r.sets[lang]["dashboard"]
	if !ok {
		t.Fatalf("no dashboard set for %q", lang)
	}

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "host-panel", stats); err != nil {
		t.Fatalf("host-panel failed: %v", err)
	}
	return buf.String()
}

func TestHostPanelSaysWhatTheFiguresAreMeasuredAgainst(t *testing.T) {
	// "80% of 512 MB (container limit)" and "80% of 64 GB (host)" are different
	// facts. A pod reads the node's memory out of /proc and looks idle while it
	// is about to be killed, so the panel is only honest if it labels which one
	// it read.
	cases := []struct {
		source hostinfo.Source
		want   string
		avoid  string
	}{
		{hostinfo.SourceCgroup, "container limit", "host"},
		{hostinfo.SourceHost, "host", "container limit"},
	}

	for _, c := range cases {
		html := renderHostPanel(t, i18n.EN, hostinfo.Stats{
			Supported: true, Source: c.source,
			CPUPercent: 12, CPUCores: 4,
			MemUsedBytes: 1 << 30, MemTotalBytes: 4 << 30,
			DiskUsedBytes: 1 << 30, DiskTotalBytes: 8 << 30, DiskPath: "/app",
		})
		if !strings.Contains(html, c.want) {
			t.Errorf("source %q did not render %q", c.source, c.want)
		}
		if strings.Contains(html, ">"+c.avoid+"<") {
			t.Errorf("source %q also rendered %q", c.source, c.avoid)
		}
	}
}

func TestHostPanelRendersTheFigures(t *testing.T) {
	html := renderHostPanel(t, i18n.EN, hostinfo.Stats{
		Supported: true, Source: hostinfo.SourceHost,
		CPUPercent: 42.4, CPUCores: 8,
		MemUsedBytes: 6 << 30, MemTotalBytes: 8 << 30,
		DiskUsedBytes: 20 << 30, DiskTotalBytes: 100 << 30, DiskPath: "/srv/cronsole",
		Load1: 0.42, Load5: 1.5, Load15: 2,
		UptimeSec: 3 * 24 * 3600,
	})

	for _, want := range []string{
		"42",              // CPU, rounded
		"75",              // memory, 6 of 8
		"6.0 GB / 8.0 GB", // and the absolute figures beside it
		"20 GB / 100 GB",  // disk
		"/srv/cronsole",   // which filesystem, so it is not guessed at
		"8 core(s)",       // what 100% would mean
		"3d",              // uptime
		"0.42",            // load, unrounded: a load average is read precisely
		"width: 42.4%",    // the bar matches the number
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the panel does not show %q", want)
		}
	}
}

func TestHostPanelShowsUnknownRatherThanZero(t *testing.T) {
	// A bar at zero says the machine is idle. The first reading has no CPU
	// delta yet, and that has to look different from "nothing is happening".
	html := renderHostPanel(t, i18n.EN, hostinfo.Stats{
		Supported: true, Source: hostinfo.SourceHost,
		CPUPercent: -1, CPUCores: 2,
	})

	if !strings.Contains(html, "&mdash;") {
		t.Error("an unread CPU figure did not render as a dash")
	}
	if !strings.Contains(html, "width: 0%") {
		t.Error("an unread CPU figure drew a bar")
	}
	// An unread figure is indeterminate, which for a progressbar means no
	// aria-valuenow at all. Announcing "0 percent" would tell a screen reader
	// user the machine is idle, which is the same lie as drawing an empty bar.
	if strings.Contains(html, "aria-valuenow") {
		t.Error("an unread figure was announced as a value")
	}
	if strings.Contains(html, ">-1") || strings.Contains(html, "\"-1") {
		t.Error("the sentinel leaked onto the screen")
	}
}

func TestHostPanelWarnsBeforeTheMachineIsFull(t *testing.T) {
	// The thresholds are where a machine stops having room to absorb a spike,
	// not where it is already full. A disk noticed at 95% is noticed too late.
	cases := []struct {
		percent float64
		want    string
	}{
		{40, ""},           // quiet
		{80, "bg-warning"}, // worth looking at
		{96, "bg-danger"},  // act now
	}

	for _, c := range cases {
		used := uint64(c.percent * 10)
		html := renderHostPanel(t, i18n.EN, hostinfo.Stats{
			Supported: true, Source: hostinfo.SourceHost,
			CPUPercent: -1, CPUCores: 1,
			DiskUsedBytes: used, DiskTotalBytes: 1000, DiskPath: "/",
		})
		if c.want == "" {
			if strings.Contains(html, "bg-warning") || strings.Contains(html, "bg-danger") {
				t.Errorf("%.0f%% full raised an alarm", c.percent)
			}
			continue
		}
		if !strings.Contains(html, c.want) {
			t.Errorf("%.0f%% full did not render %s", c.percent, c.want)
		}
	}
}

func TestHostPanelIsTranslated(t *testing.T) {
	html := renderHostPanel(t, i18n.TR, hostinfo.Stats{
		Supported: true, Source: hostinfo.SourceCgroup,
		CPUPercent: 10, CPUCores: 1,
		MemUsedBytes: 1 << 20, MemTotalBytes: 2 << 20,
	})
	if !strings.Contains(html, "Sunucu") {
		t.Error("the Turkish panel is in English")
	}
	if !strings.Contains(html, "konteyner limiti") {
		t.Error("the source label was not translated")
	}
}
