package dockerinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// answer serves one canned list and records what was asked for.
type answer struct {
	mu     sync.Mutex
	method string
	query  string
	calls  int

	status int
	body   string
}

func (a *answer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.method = r.Method
	a.query = r.URL.RawQuery
	a.calls++
	status, body := a.status, a.body
	a.mu.Unlock()

	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (a *answer) set(status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.body = status, body
}

func (a *answer) seen() (method, query string, calls int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.method, a.query, a.calls
}

// list builds a Docker style answer from (name, image, state, status) rows.
func list(rows ...[4]string) string {
	type entry struct {
		ID     string   `json:"Id"`
		Names  []string `json:"Names"`
		Image  string   `json:"Image"`
		State  string   `json:"State"`
		Status string   `json:"Status"`
	}
	out := make([]entry, 0, len(rows))
	for i, r := range rows {
		out = append(out, entry{
			ID:     fmt.Sprintf("%064d", i),
			Names:  []string{"/" + r[0]},
			Image:  r[1],
			State:  r[2],
			Status: r[3],
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func serve(t *testing.T, a *answer) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(a)
	t.Cleanup(s.Close)
	return s
}

func TestTheListIsReadAndTheBrokenSortFirst(t *testing.T) {
	// The panel is glanced at. A stopped container below thirty running ones is
	// not on the screen, so the order is the feature, not a detail.
	a := &answer{body: list(
		[4]string{"zebra", "img:1", "running", "Up 2 hours"},
		[4]string{"alpha", "img:2", "running", "Up 3 days (healthy)"},
		[4]string{"queue", "img:3", "exited", "Exited (1) 5 minutes ago"},
		[4]string{"api", "img:4", "running", "Up 10 seconds (unhealthy)"},
		[4]string{"warm", "img:5", "running", "Up 1 second (health: starting)"},
	)}
	s := serve(t, a)

	r := NewReader(s.URL, "")
	r.sample(context.Background())
	snap := r.Read()

	if snap.Err != "" {
		t.Fatalf("a good answer reported an error: %s", snap.Err)
	}
	var order []string
	for _, c := range snap.Containers {
		order = append(order, c.Name)
	}
	want := []string{"api", "queue", "warm", "alpha", "zebra"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}

	if snap.Total() != 5 {
		t.Errorf("Total() = %d, want 5", snap.Total())
	}
	if snap.Up() != 4 {
		t.Errorf("Up() = %d, want 4", snap.Up())
	}
	if snap.Pending() {
		t.Error("a completed sample still reports pending")
	}
}

func TestTheNameLosesItsSlashAndTheIDIsShort(t *testing.T) {
	a := &answer{body: list([4]string{"cronsole", "img:1", "running", "Up 1 day"})}
	s := serve(t, a)

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	c := r.Read().Containers[0]
	if c.Name != "cronsole" {
		t.Errorf("name = %q, want cronsole", c.Name)
	}
	if len(c.ID) != 12 {
		t.Errorf("id = %q, want the 12 character prefix Docker itself prints", c.ID)
	}
}

func TestANamelessContainerFallsBackToItsID(t *testing.T) {
	s := serve(t, &answer{body: `[{"Id":"abcdef0123456789","Names":[],"Image":"i","State":"running","Status":"Up"}]`})

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	if got := r.Read().Containers[0].Name; got != "abcdef012345" {
		t.Errorf("name = %q, want the short id", got)
	}
}

func TestHealthComesFromTheFieldWhenTheDaemonSendsOne(t *testing.T) {
	// Verified against a real daemon: API 1.55 puts a Health object on the list
	// endpoint, and the status line there carries no suffix at all.
	s := serve(t, &answer{body: `[
		{"Id":"a","Names":["/api"],"State":"running","Status":"Up 2 hours","Health":{"Status":"unhealthy"}},
		{"Id":"b","Names":["/web"],"State":"running","Status":"Up 2 hours","Health":{"Status":"none"}}
	]`})

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	got := map[string]string{}
	for _, c := range r.Read().Containers {
		got[c.Name] = c.Health
	}
	if got["api"] != Unhealthy {
		t.Errorf("api health = %q, want %q", got["api"], Unhealthy)
	}
	// An image declaring no health check is silence, not a verdict. Drawn as a
	// green badge it would claim something nobody checked.
	if got["web"] != "" {
		t.Errorf("web health = %q, want empty for a container with no health check", got["web"])
	}
}

func TestHealthFallsBackToTheStatusLineOnAnOlderDaemon(t *testing.T) {
	// The daemon on this machine accepts API 1.40, which predates the field.
	s := serve(t, &answer{body: `[{"Id":"a","Names":["/api"],"State":"running","Status":"Up 2 hours (unhealthy)"}]`})

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	if got := r.Read().Containers[0].Health; got != Unhealthy {
		t.Errorf("health = %q, want %q from the status line", got, Unhealthy)
	}
}

func TestHealthIsReadFromTheStatusLine(t *testing.T) {
	// It is a display string and could change. A parse that does not match must
	// yield no health rather than a wrong one: the state beside it is still
	// correct, and a green badge on an unchecked container is a lie.
	cases := map[string]string{
		"Up 2 hours (healthy)":            Healthy,
		"Up 10 seconds (unhealthy)":       Unhealthy,
		"Up 1 second (health: starting)":  Starting,
		"Up 3 days":                       "",
		"Exited (0) 4 minutes ago":        "",
		"Up 2 hours (something new here)": "",
	}
	for status, want := range cases {
		if got := health(status); got != want {
			t.Errorf("health(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestAFailedSampleKeepsTheLastListAndSaysItIsOld(t *testing.T) {
	// An unreachable proxy says nothing about whether the containers are still
	// there. Emptying the panel would say they are gone.
	a := &answer{body: list([4]string{"cronsole", "img:1", "running", "Up 1 day"})}
	s := serve(t, a)

	r := NewReader(s.URL, "")
	r.sample(context.Background())
	first := r.Read()

	a.set(http.StatusInternalServerError, "boom")
	r.sample(context.Background())
	after := r.Read()

	if after.Err == "" {
		t.Fatal("a 500 was reported as a successful reading")
	}
	if len(after.Containers) != 1 || after.Containers[0].Name != "cronsole" {
		t.Errorf("the failed sample emptied the list: %+v", after.Containers)
	}
	if !after.OKAt.Equal(first.OKAt) {
		t.Error("a failed sample moved the time of the last successful one")
	}
	if !after.Stale() {
		t.Error("the panel was not told the list it is showing is old")
	}
	if !after.ReadAt.After(first.ReadAt) {
		t.Error("the attempt itself was not recorded")
	}
}

func TestNothingToShowWhenNoSampleHasEverSucceeded(t *testing.T) {
	s := serve(t, &answer{status: http.StatusForbidden, body: "no"})

	r := NewReader(s.URL, "")
	r.sample(context.Background())
	snap := r.Read()

	if snap.Err == "" {
		t.Fatal("a 403 was reported as a successful reading")
	}
	if !snap.OKAt.IsZero() {
		t.Error("a reading that never succeeded claims a successful one")
	}
	if snap.Stale() {
		t.Error("there is no old list, so nothing can be stale")
	}
	if len(snap.Containers) != 0 {
		t.Error("containers appeared out of a failed reading")
	}
}

func TestTheProxyBodyIsNotShownOnTheError(t *testing.T) {
	// A misconfigured proxy answers with a page of HTML. That reaches an
	// administrator's screen, so only the status line is reported.
	s := serve(t, &answer{status: http.StatusBadGateway, body: "<html><body>nginx</body></html>"})

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	if got := r.Read().Err; strings.Contains(got, "nginx") {
		t.Errorf("the proxy's body reached the panel: %q", got)
	}
}

func TestCredentialsInTheAddressNeverReachThePanel(t *testing.T) {
	// A transport error embeds the whole URL, and DOCKER_API is a value an
	// operator may have put a password into. The error is shown on screen, so
	// the address is stripped down to what the underlying failure says: a host
	// and a port, which is what an administrator needs to diagnose it.
	r := NewReader("http://dockeruser:s3cret-token@127.0.0.1:1", "")
	r.sample(context.Background())

	got := r.Read().Err
	if got == "" {
		t.Fatal("a dead address reported no error")
	}
	for _, secret := range []string{"s3cret-token", "dockeruser"} {
		if strings.Contains(got, secret) {
			t.Errorf("the error published %q: %q", secret, got)
		}
	}
	// And it still says something an operator can act on.
	if !strings.Contains(got, "connect") && !strings.Contains(got, "refused") {
		t.Errorf("err = %q, want it to say what actually failed", got)
	}
}

func TestABrokenAnswerIsAnErrorRatherThanAnEmptyList(t *testing.T) {
	s := serve(t, &answer{body: "{not json"})

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	if r.Read().Err == "" {
		t.Error("unparseable JSON was reported as a host with no containers")
	}
}

func TestTheAnswerIsBounded(t *testing.T) {
	// A broken or hostile endpoint must not be able to grow this process. The
	// cut lands inside the JSON, so it surfaces as a read error.
	s := serve(t, &answer{body: "[" + strings.Repeat(`{"Id":"a","Names":["/x"],"State":"running"},`, 60000)})

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	if r.Read().Err == "" {
		t.Error("an answer past the limit was accepted whole")
	}
}

func TestOnlyTheContainerListIsEverAsked(t *testing.T) {
	// Read-only by construction: one endpoint, one method. Anything else needs
	// write access to the Docker API, which is what the proxy withholds.
	a := &answer{body: "[]"}
	s := serve(t, a)

	r := NewReader(s.URL, "")
	r.sample(context.Background())

	method, query, _ := a.seen()
	if method != http.MethodGet {
		t.Errorf("method = %s, want GET", method)
	}
	if !strings.Contains(r.url, "/containers/json") {
		t.Errorf("url = %q, want the container list endpoint", r.url)
	}
	// Stopped containers are the reason the panel is worth having.
	if !strings.Contains(query, "all=1") {
		t.Errorf("query = %q, want all=1", query)
	}
}

func TestTheLabelBecomesDockersOwnFilter(t *testing.T) {
	// Docker does the matching, so a label that selects nothing is Docker's
	// answer rather than a second implementation of matching here.
	a := &answer{body: "[]"}
	s := serve(t, a)

	r := NewReader(s.URL, "cronsole.watch=true")
	r.sample(context.Background())

	_, query, _ := a.seen()
	if !strings.Contains(query, "filters=") {
		t.Fatalf("query = %q, want a filters parameter", query)
	}
	if !strings.Contains(query, "cronsole.watch") {
		t.Errorf("query = %q, does not carry the label", query)
	}
}

func TestNoLabelAsksForEverything(t *testing.T) {
	a := &answer{body: "[]"}
	s := serve(t, a)

	r := NewReader(s.URL, "  ")
	r.sample(context.Background())

	if _, query, _ := a.seen(); strings.Contains(query, "filters") {
		t.Errorf("query = %q, want no filter at all", query)
	}
}

func TestNothingIsClaimedBeforeTheFirstSample(t *testing.T) {
	// "Not read yet" and "no containers" are different facts, and the panel
	// draws them differently.
	r := NewReader("http://127.0.0.1:1", "")

	snap := r.Read()
	if !snap.Pending() {
		t.Error("an unsampled reader did not report pending")
	}
	if snap.Err != "" {
		t.Error("an unsampled reader invented an error")
	}
}

func TestStartSamplesImmediately(t *testing.T) {
	// Otherwise the panel is blank for whoever opens the dashboard first and
	// fills in on their second look.
	a := &answer{body: list([4]string{"cronsole", "img:1", "running", "Up 1 day"})}
	s := serve(t, a)

	r := NewReader(s.URL, "")
	// The sampler stops when the test's context is cancelled. The interval is
	// long enough that only the first sample can run.
	r.Start(t.Context(), time.Hour)

	deadline := time.After(2 * time.Second)
	for {
		if !r.Read().Pending() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no sample was taken when the reader started")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := r.Read().Containers; len(got) != 1 {
		t.Errorf("the first sample read %d container(s), want 1", len(got))
	}
}

func TestTheCutOnlyEverHidesRunningContainers(t *testing.T) {
	// The list is sorted with whatever is wrong at the top, so the panel's
	// ceiling can never hide the one container that needs attention.
	rows := make([][4]string, 0, maxShown+5)
	rows = append(rows, [4]string{"aaa-broken", "img", "exited", "Exited (1) ago"})
	for i := 0; i < maxShown+4; i++ {
		rows = append(rows, [4]string{fmt.Sprintf("svc-%03d", i), "img", "running", "Up 1 day"})
	}
	s := serve(t, &answer{body: list(rows...)})

	r := NewReader(s.URL, "")
	r.sample(context.Background())
	snap := r.Read()

	if len(snap.Visible()) != maxShown {
		t.Errorf("Visible() = %d, want %d", len(snap.Visible()), maxShown)
	}
	if snap.Hidden() != 5 {
		t.Errorf("Hidden() = %d, want 5", snap.Hidden())
	}
	if snap.Visible()[0].Name != "aaa-broken" {
		t.Errorf("the broken container is not first: %q", snap.Visible()[0].Name)
	}
	for _, c := range snap.Containers[maxShown:] {
		if !c.Running() {
			t.Errorf("the cut hid %q, which is %s", c.Name, c.State)
		}
	}
}

func TestAShortListIsNotCut(t *testing.T) {
	s := serve(t, &answer{body: list([4]string{"one", "img", "running", "Up"})})

	r := NewReader(s.URL, "")
	r.sample(context.Background())
	snap := r.Read()

	if len(snap.Visible()) != 1 || snap.Hidden() != 0 {
		t.Errorf("Visible() = %d, Hidden() = %d, want 1 and 0", len(snap.Visible()), snap.Hidden())
	}
}
