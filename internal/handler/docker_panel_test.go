package handler

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/dockerinfo"
	"github.com/mstgnz/cronsole/v2/internal/i18n"
)

// renderDockerPanel executes the panel on its own, which is what the dashboard
// does with it.
func renderDockerPanel(t *testing.T, lang i18n.Lang, snap dockerinfo.Snapshot) string {
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
	if err := set.ExecuteTemplate(&buf, "docker-panel", snap); err != nil {
		t.Fatalf("docker-panel failed: %v", err)
	}
	return buf.String()
}

// sampled wraps containers in a snapshot that has been read successfully.
func sampled(containers ...dockerinfo.Container) dockerinfo.Snapshot {
	now := time.Now()
	return dockerinfo.Snapshot{Containers: containers, ReadAt: now, OKAt: now}
}

func TestDockerPanelSeparatesStateFromHealth(t *testing.T) {
	// A container can be up and failing its own health check, and that reads as
	// "running" everywhere else. No health check at all is silence rather than
	// a green badge: an image declaring none is not evidence that it works.
	cases := []struct {
		container dockerinfo.Container
		want      string
		avoid     string
	}{
		{
			dockerinfo.Container{Name: "api", State: "running", Health: "unhealthy"},
			"status-danger", "status-success",
		},
		{
			dockerinfo.Container{Name: "api", State: "running", Health: "healthy"},
			"status-success", "status-danger",
		},
		{
			dockerinfo.Container{Name: "api", State: "running"},
			"status-secondary", "status-success",
		},
		{
			dockerinfo.Container{Name: "api", State: "exited"},
			"status-warning", "status-success",
		},
		{
			dockerinfo.Container{Name: "api", State: "running", Health: "starting"},
			"status-info", "status-success",
		},
	}

	for _, c := range cases {
		html := renderDockerPanel(t, i18n.EN, sampled(c.container))
		if !strings.Contains(html, c.want) {
			t.Errorf("%s/%q did not render %s", c.container.State, c.container.Health, c.want)
		}
		if strings.Contains(html, c.avoid) {
			t.Errorf("%s/%q also rendered %s", c.container.State, c.container.Health, c.avoid)
		}
	}
}

func TestDockerPanelRendersWhatIsRunning(t *testing.T) {
	html := renderDockerPanel(t, i18n.EN, sampled(
		dockerinfo.Container{Name: "cronsole", Image: "cronsole:latest",
			State: "running", Status: "Up 3 days (healthy)", Health: "healthy"},
		dockerinfo.Container{Name: "queue", Image: "redis:7",
			State: "exited", Status: "Exited (1) 5 minutes ago"},
	))

	for _, want := range []string{
		"cronsole",        // the name
		"cronsole:latest", // and the image, so two of the same are told apart
		"Up 3 days",       // Docker's own uptime, not one computed here
		"Exited (1)",      // and its own words for why one is not running
		"1 of 2 running",  // the count in the header
		"exited",          // the state of the one that is not
		"just now",        // and how fresh the reading is, beside the count
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the panel does not show %q", want)
		}
	}
}

func TestDockerPanelSaysAFailedReadingIsAFailedReading(t *testing.T) {
	// An empty list would say the host has no containers, which is a different
	// and much calmer fact than "the console cannot see them".
	now := time.Now()
	html := renderDockerPanel(t, i18n.EN, dockerinfo.Snapshot{
		ReadAt: now,
		Err:    "connection refused",
	})

	if !strings.Contains(html, "could not be read") {
		t.Error("a failed reading was not stated")
	}
	if !strings.Contains(html, "connection refused") {
		t.Error("the reason was not shown, so there is nothing to act on")
	}
	if strings.Contains(html, "No containers are on this host") {
		t.Error("a failed reading was reported as a host with no containers")
	}
}

func TestDockerPanelKeepsTheOldListAndSaysItIsOld(t *testing.T) {
	html := renderDockerPanel(t, i18n.EN, dockerinfo.Snapshot{
		Containers: []dockerinfo.Container{{Name: "cronsole", State: "running", Status: "Up 3 days"}},
		ReadAt:     time.Now(),
		OKAt:       time.Now().Add(-4 * time.Minute),
		Err:        "connection refused",
	})

	if !strings.Contains(html, "cronsole") {
		t.Error("the last good list was thrown away when a later reading failed")
	}
	if !strings.Contains(html, "The list below is from") {
		t.Error("a stale list was shown without saying it is stale")
	}
}

func TestDockerPanelDoesNotClaimAnEmptyHostBeforeItHasLooked(t *testing.T) {
	html := renderDockerPanel(t, i18n.EN, dockerinfo.Snapshot{})

	if strings.Contains(html, "No containers are on this host") {
		t.Error("the panel described the machine before reading it")
	}
	if !strings.Contains(html, "reading") {
		t.Error("the panel did not say it is still reading")
	}
}

func TestDockerPanelSaysWhenNothingIsRunningOnTheHost(t *testing.T) {
	html := renderDockerPanel(t, i18n.EN, dockerinfo.Snapshot{ReadAt: time.Now(), OKAt: time.Now()})

	if !strings.Contains(html, "No containers are on this host") {
		t.Error("a host with no containers rendered as nothing at all")
	}
}

func TestDockerPanelSaysHowManyItLeftOut(t *testing.T) {
	// The ceiling is only safe because the broken ones sort first, and the line
	// is what stops the cut from looking like the whole truth.
	var many []dockerinfo.Container
	for i := 0; i < 30; i++ {
		many = append(many, dockerinfo.Container{Name: "svc", State: "running", Status: "Up 1 day"})
	}
	html := renderDockerPanel(t, i18n.EN, sampled(many...))

	if strings.Count(html, "<tr>") != 25 {
		t.Errorf("the panel drew %d rows, want 25", strings.Count(html, "<tr>"))
	}
	if !strings.Contains(html, "5 more") {
		t.Error("the panel did not say how many it left out")
	}
}

func TestDockerPanelIsTranslated(t *testing.T) {
	html := renderDockerPanel(t, i18n.TR, sampled(
		dockerinfo.Container{Name: "queue", State: "exited", Status: "Exited (1) ago"},
	))

	if !strings.Contains(html, "Konteynerler") {
		t.Error("the Turkish panel is in English")
	}
	if !strings.Contains(html, "çıktı") {
		t.Error("the container state was not translated")
	}
}
