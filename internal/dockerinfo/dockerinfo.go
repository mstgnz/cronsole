// Package dockerinfo reports the containers running beside this service.
//
// It is the companion to hostinfo and deliberately not part of it: hostinfo
// reads files under /proc and cannot fail in a way worth showing, while this
// crosses a network boundary and its failure is itself something the screen has
// to say.
//
// # Why an address and never the socket
//
// The obvious implementation mounts /var/run/docker.sock and talks to it. That
// is refused here. Anything able to reach that socket can start a privileged
// container with the host's filesystem mounted, so a web service holding it
// turns any remote flaw into ownership of the machine, and `:ro` on the mount
// does not help: it makes the socket file read-only, not the API behind it.
//
// So this speaks plain HTTP to an address the operator sets, which is expected
// to be a proxy in front of the socket publishing the container list and
// nothing else. Cronsole then needs no privilege at all, and the blast radius
// of this panel is "somebody learns the names of the containers", which is
// where a dashboard belongs.
//
// # Read-only by construction
//
// Only one endpoint is ever called, with GET. There is no start, stop or
// restart here and adding one is not a small change: it needs write access to
// the Docker API, which is the thing the proxy exists to withhold.
package dockerinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Timeouts. Every one of them is set: a panel refreshed on a timer must not be
// able to accumulate goroutines waiting on an address that stopped answering.
const (
	requestTimeout = 3 * time.Second
	dialTimeout    = 2 * time.Second
	headerTimeout  = 3 * time.Second
)

// maxBodyBytes bounds what is read from the proxy. Fifty containers answer in a
// few kilobytes; a megabyte is far past any real host and stops a broken or
// hostile endpoint from growing this process.
const maxBodyBytes = 1 << 20

// maxShown bounds what the panel draws. The list is sorted with the containers
// in trouble first, so the cut can only ever hide healthy ones.
const maxShown = 25

// Health values, as they come out of the status line.
const (
	Healthy   = "healthy"
	Unhealthy = "unhealthy"
	Starting  = "starting"
)

// Container is one container, reduced to what a glance needs.
type Container struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	// State is Docker's own word: running, exited, paused, restarting, created,
	// removing or dead.
	State string `json:"state"`
	// Status is the sentence Docker writes beside it, "Up 3 days (healthy)" or
	// "Exited (1) 2 hours ago". It carries the uptime, so none is computed here.
	Status string `json:"status"`
	// Health is empty when the image declares no health check, which is not the
	// same as healthy and must not be drawn as it.
	Health string `json:"health"`
}

// Running reports whether the container is up. Anything else is worth looking
// at, including paused and restarting.
func (c Container) Running() bool { return c.State == "running" }

// Snapshot is one sample.
//
// A failed sample keeps the containers from the last successful one rather than
// emptying the list, because an unreachable proxy says nothing about whether
// the containers are still there, and an emptied panel says they are gone.
// OKAt is what makes that honest: the screen can show how old the list is.
type Snapshot struct {
	Containers []Container `json:"containers"`
	// ReadAt is when the last attempt finished, successful or not.
	ReadAt time.Time `json:"read_at"`
	// OKAt is when the last successful attempt finished. Zero means no sample
	// has ever succeeded, so there is nothing to show but the error.
	OKAt time.Time `json:"ok_at"`
	// Err is why the last attempt failed, for the screen. Empty when it worked.
	Err string `json:"err,omitempty"`
}

// Pending reports that no attempt has finished yet. Distinct from "no
// containers": one is a panel that has not filled in, the other is a statement
// about the machine.
func (s Snapshot) Pending() bool { return s.ReadAt.IsZero() }

// Stale reports that the list on screen is older than the last attempt, which
// is true exactly when the last attempt failed and an earlier one had not.
func (s Snapshot) Stale() bool { return s.Err != "" && !s.OKAt.IsZero() }

// Visible is the part of the list the panel draws.
func (s Snapshot) Visible() []Container {
	if len(s.Containers) > maxShown {
		return s.Containers[:maxShown]
	}
	return s.Containers
}

// Hidden is how many the cut left out, so the panel can say so rather than
// quietly showing a subset.
func (s Snapshot) Hidden() int {
	if n := len(s.Containers) - maxShown; n > 0 {
		return n
	}
	return 0
}

// Total is how many containers the last successful sample found.
func (s Snapshot) Total() int { return len(s.Containers) }

// Up is how many of them are running.
func (s Snapshot) Up() int {
	n := 0
	for _, c := range s.Containers {
		if c.Running() {
			n++
		}
	}
	return n
}

// Reader samples the Docker API on a timer.
//
// The timer is the point. Sampling inside the request would put the Docker
// API's latency in front of the whole dashboard, and the dashboard reloads
// itself every thirty seconds on somebody's wall.
type Reader struct {
	mu   sync.Mutex
	last Snapshot

	client *http.Client
	url    string
}

// NewReader builds a reader over the base address of a Docker API proxy.
//
// label, when set, is passed to Docker's own filter, so a label that matches
// nothing is Docker's answer rather than a second implementation of matching
// here. Empty lists every container on the host.
func NewReader(base, label string) *Reader {
	return &Reader{client: newClient(), url: listURL(base, label)}
}

func newClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   headerTimeout,
			ResponseHeaderTimeout: headerTimeout,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConns:          2,
			MaxIdleConnsPerHost:   2,
		},
	}
}

// listURL builds the one address this package ever calls.
//
// No API version prefix: unversioned resolves to whatever the daemon speaks,
// where a pinned version fails outright against an older one.
func listURL(base, label string) string {
	q := url.Values{}
	// Stopped containers are the reason to have this panel at all, so all=1.
	q.Set("all", "1")
	if label = strings.TrimSpace(label); label != "" {
		if raw, err := json.Marshal(map[string][]string{"label": {label}}); err == nil {
			q.Set("filters", string(raw))
		}
	}
	return strings.TrimRight(base, "/") + "/containers/json?" + q.Encode()
}

// Start samples until the context is cancelled.
//
// The first sample is taken immediately, so the panel is filled in for whoever
// opens the dashboard first rather than on their second look.
func (r *Reader) Start(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		r.sample(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sample(ctx)
			}
		}
	}()
}

// Read returns the last sample. It never issues a request of its own: a page
// that waited on the Docker API would be as slow as the slowest thing on the
// machine, which is precisely the machine this is worth looking at.
func (r *Reader) Read() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// sample takes one reading and folds it into the last.
func (r *Reader) sample(ctx context.Context) {
	containers, err := r.fetch(ctx)
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.last.ReadAt = now
	if err != nil {
		// The list stays as it was. See the Snapshot doc: an unreachable proxy
		// is not evidence that the containers went away.
		r.last.Err = reason(err)
		return
	}
	r.last.Err = ""
	r.last.OKAt = now
	r.last.Containers = containers
}

// apiContainer is the part of Docker's answer this reads. Everything else in
// that object is ignored on purpose: fewer fields is less to break when the
// daemon's version moves.
type apiContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
	// Health is a pointer because its absence is the thing that matters: a
	// daemon old enough not to send it is why parsing the status line still
	// exists. Verified present on API 1.55, and that daemon still accepts 1.40.
	Health *apiHealth `json:"Health"`
}

type apiHealth struct {
	// Status is one of healthy, unhealthy, starting, or none for an image that
	// declares no health check.
	Status string `json:"Status"`
}

func (r *Reader) fetch(ctx context.Context) ([]Container, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The body of an error is Docker's own message and is not shown: on a
		// misconfigured proxy it is a page of HTML, and this ends up on screen.
		return nil, fmt.Errorf("the docker api answered %s", resp.Status)
	}

	var raw []apiContainer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("the docker api answer could not be read: %w", err)
	}

	out := make([]Container, 0, len(raw))
	for _, c := range raw {
		out = append(out, Container{
			ID:     shortID(c.ID),
			Name:   name(c.Names, c.ID),
			Image:  c.Image,
			State:  c.State,
			Status: c.Status,
			Health: healthOf(c),
		})
	}
	sortByAttention(out)
	return out, nil
}

// reason keeps the address out of the message.
//
// A transport error embeds the whole URL, which carries credentials when
// somebody has put them there. The panel is not a place to publish those, and
// the underlying error says what actually went wrong anyway.
func reason(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err.Error()
	}
	return err.Error()
}

// shortID is the prefix Docker itself prints. The full 64 characters identify
// nothing better on a screen.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// name takes the first of Docker's names and drops its leading slash. A
// container can be nameless, and then the id is the only thing to call it.
func name(names []string, id string) string {
	for _, n := range names {
		if n = strings.TrimPrefix(strings.TrimSpace(n), "/"); n != "" {
			return n
		}
	}
	return shortID(id)
}

// healthOf prefers the field and falls back to the sentence.
//
// A recent daemon publishes a Health object on the list endpoint and that is
// the answer. An older one sends nothing, and then the only health available
// without inspecting every container one by one is the suffix on the status
// line. "none" is an image that declares no health check, which is silence
// rather than a verdict, so it becomes empty here.
func healthOf(c apiContainer) string {
	if c.Health != nil {
		switch c.Health.Status {
		case Healthy, Unhealthy, Starting:
			return c.Health.Status
		}
		return ""
	}
	return health(c.Status)
}

// health reads the health out of the status line, "Up 2 hours (healthy)", for
// daemons that do not send the field.
//
// This is a display string and could change. A parse that does not match yields
// no health rather than a wrong one, and the state and status beside it are
// still correct either way.
func health(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return Healthy
	case strings.Contains(status, "(unhealthy)"):
		return Unhealthy
	case strings.Contains(status, "(health: starting)"):
		return Starting
	}
	return ""
}

// sortByAttention puts what is wrong at the top.
//
// A panel is glanced at, and the one stopped container below thirty running
// ones is not on the screen. Within a rank the order is by name, so a container
// does not move around between refreshes.
func sortByAttention(list []Container) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := rank(list[i]), rank(list[j])
		if a != b {
			return a < b
		}
		return list[i].Name < list[j].Name
	})
}

func rank(c Container) int {
	switch {
	case c.Health == Unhealthy:
		return 0
	case !c.Running():
		return 1
	case c.Health == Starting:
		return 2
	}
	return 3
}
