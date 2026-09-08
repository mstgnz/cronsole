package service

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// --- an in-memory store -----------------------------------------------------

type memHostOverrideRepo struct {
	mu   sync.Mutex
	rows map[int64]domain.HostOverride
	next int64
	// listErr makes ListActive fail, so the reload path can be exercised.
	listErr error
	// listed counts reloads, so the watcher can be observed.
	listed int
}

func newMemHostOverrideRepo() *memHostOverrideRepo {
	return &memHostOverrideRepo{rows: map[int64]domain.HostOverride{}, next: 1}
}

func (m *memHostOverrideRepo) all(activeOnly bool) []domain.HostOverride {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := []domain.HostOverride{}
	for _, row := range m.rows {
		if activeOnly && !row.Active {
			continue
		}
		out = append(out, row)
	}
	return out
}

func (m *memHostOverrideRepo) List(context.Context) ([]domain.HostOverride, error) {
	return m.all(false), nil
}

func (m *memHostOverrideRepo) ListActive(context.Context) ([]domain.HostOverride, error) {
	m.mu.Lock()
	m.listed++
	err := m.listErr
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return m.all(true), nil
}

func (m *memHostOverrideRepo) reloads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listed
}

func (m *memHostOverrideRepo) failReads(err error) {
	m.mu.Lock()
	m.listErr = err
	m.mu.Unlock()
}

func (m *memHostOverrideRepo) Get(_ context.Context, id int64) (*domain.HostOverride, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return &row, nil
}

// key mirrors the unique index, which is what makes the duplicate test a test
// of the same rule the database enforces.
func hostKey(hostname string, port *int) string {
	p := 0
	if port != nil {
		p = *port
	}
	return strings.ToLower(hostname) + "/" + itoa(p)
}

func (m *memHostOverrideRepo) Create(_ context.Context, o *domain.HostOverride) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, row := range m.rows {
		if hostKey(row.Hostname, row.Port) == hostKey(o.Hostname, o.Port) {
			return 0, repository.ErrDuplicate
		}
	}
	id := m.next
	m.next++
	row := *o
	row.ID = id
	row.CreatedAt, row.UpdatedAt = time.Now(), time.Now()
	m.rows[id] = row
	return id, nil
}

func (m *memHostOverrideRepo) Update(_ context.Context, o *domain.HostOverride) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.rows[o.ID]; !ok {
		return repository.ErrNotFound
	}
	for id, row := range m.rows {
		if id != o.ID && hostKey(row.Hostname, row.Port) == hostKey(o.Hostname, o.Port) {
			return repository.ErrDuplicate
		}
	}
	row := *o
	row.UpdatedAt = time.Now()
	m.rows[o.ID] = row
	return nil
}

func (m *memHostOverrideRepo) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[id]; !ok {
		return repository.ErrNotFound
	}
	delete(m.rows, id)
	return nil
}

func newHostOverrideService(t *testing.T, allowPrivate bool) (*HostOverrideService, *memHostOverrideRepo) {
	t.Helper()
	repo := newMemHostOverrideRepo()
	svc := NewHostOverrideService(repo, TargetPolicy{AllowPrivate: allowPrivate}, applog.New())
	return svc, repo
}

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	out := make([]string, 0, len(v.Errors))
	for _, e := range v.Errors {
		out = append(out, e.Field)
	}
	return out
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// --- what may be stored -----------------------------------------------------

func TestHostOverrideRefusesWhatWouldMakeItUnreadableOrUnsafe(t *testing.T) {
	svc, _ := newHostOverrideService(t, true)

	cases := []struct {
		name  string
		in    HostOverrideInput
		field string
		why   string
	}{
		{
			name:  "no hostname",
			in:    HostOverrideInput{Address: "10.0.0.5"},
			field: "hostname",
		},
		{
			name:  "a URL rather than a host name",
			in:    HostOverrideInput{Hostname: "https://shop.example.com/x", Address: "10.0.0.5"},
			field: "hostname",
			why:   "a scheme or path here would never match a dial address, so the row would silently do nothing",
		},
		{
			name:  "a host and port in the hostname",
			in:    HostOverrideInput{Hostname: "shop.example.com:8443", Address: "10.0.0.5"},
			field: "hostname",
			why:   "the port is its own field; accepting it here would create a row that matches nothing",
		},
		{
			name:  "a wildcard",
			in:    HostOverrideInput{Hostname: "*.example.com", Address: "10.0.0.5"},
			field: "hostname",
			why:   "one row would capture hosts nobody enumerated, and every route has to be a line somebody can read",
		},
		{
			name:  "an address as the hostname",
			in:    HostOverrideInput{Hostname: "10.0.0.9", Address: "10.0.0.5"},
			field: "hostname",
			why:   "redirecting an address to an address makes a target policy decision unreadable",
		},
		{
			name:  "a name as the address",
			in:    HostOverrideInput{Hostname: "shop.example.com", Address: "internal.example.com"},
			field: "address",
			why:   "a name means a second DNS lookup, which is the round trip this exists to avoid",
		},
		{
			name:  "the unspecified address",
			in:    HostOverrideInput{Hostname: "shop.example.com", Address: "0.0.0.0"},
			field: "address",
		},
		{
			name:  "a multicast address",
			in:    HostOverrideInput{Hostname: "shop.example.com", Address: "224.0.0.1"},
			field: "address",
		},
		{
			name:  "a port outside the range",
			in:    HostOverrideInput{Hostname: "shop.example.com", Address: "10.0.0.5", Port: "70000"},
			field: "port",
		},
		{
			name:  "a port that is not a number",
			in:    HostOverrideInput{Hostname: "shop.example.com", Address: "10.0.0.5", Port: "https"},
			field: "port",
		},
		{
			name:  "a note longer than the column",
			in:    HostOverrideInput{Hostname: "shop.example.com", Address: "10.0.0.5", Note: strings.Repeat("x", 501)},
			field: "note",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Save(context.Background(), c.in)
			if err == nil {
				t.Fatalf("accepted; %s", c.why)
			}
			if fields := fieldsOf(t, err); !hasField(fields, c.field) {
				t.Errorf("refused on %v, want a failure on %q", fields, c.field)
			}
		})
	}
}

func TestHostOverrideRefusesAPrivateRouteWhenPrivateTargetsAreForbidden(t *testing.T) {
	// OUTBOUND_ALLOW_PRIVATE=false says this deployment's jobs are all public.
	// A route to a private address walks straight past that, which is the SSRF
	// the setting exists to prevent, so the two settings must agree.
	svc, _ := newHostOverrideService(t, false)

	for _, address := range []string{"10.0.0.5", "192.168.1.1", "172.16.0.1", "127.0.0.1", "169.254.169.254"} {
		_, err := svc.Save(context.Background(), HostOverrideInput{
			Hostname: "shop.example.com", Address: address, Active: true,
		})
		if err == nil {
			t.Errorf("%s was accepted while private targets are forbidden", address)
			continue
		}
		if fields := fieldsOf(t, err); !hasField(fields, "address") {
			t.Errorf("%s refused on %v, want a failure on address", address, fields)
		}
	}
}

func TestHostOverrideAllowsAPrivateRouteWhenPrivateTargetsAreAllowed(t *testing.T) {
	// Which is the normal case: the whole point of this service is calling
	// internal endpoints that have no public address.
	svc, _ := newHostOverrideService(t, true)

	row, err := svc.Save(context.Background(), HostOverrideInput{
		Hostname: "shop.example.com", Address: "10.0.0.5", Active: true,
	})
	if err != nil {
		t.Fatalf("Save = %v", err)
	}
	if row.ID == 0 {
		t.Error("the row was not given an id")
	}
}

func TestHostOverrideNormalisesWhatItStores(t *testing.T) {
	svc, _ := newHostOverrideService(t, true)

	row, err := svc.Save(context.Background(), HostOverrideInput{
		Hostname: "  SHOP.Example.COM  ",
		Address:  " 2001:DB8::0001 ",
		Note:     "  through the private network  ",
		Active:   true,
	})
	if err != nil {
		t.Fatalf("Save = %v", err)
	}
	// Lower-cased, because a dial address is matched case-insensitively and a
	// stored mixed-case name would look like a second, different route.
	if row.Hostname != "shop.example.com" {
		t.Errorf("hostname stored as %q", row.Hostname)
	}
	// Canonical form, so two spellings of one address are one row.
	if row.Address != "2001:db8::1" {
		t.Errorf("address stored as %q", row.Address)
	}
	if row.Note != "through the private network" {
		t.Errorf("note stored as %q", row.Note)
	}
}

func TestHostOverrideRefusesAnAddressWithLeadingZeros(t *testing.T) {
	// "010.0.0.1" is 8.0.0.1 to anything that reads the leading zero as octal
	// and 10.0.0.1 to anything that does not. Go's parser refuses it outright,
	// and this pins that: a route whose destination depends on who is reading
	// it is the one kind of route that must never be storable.
	svc, _ := newHostOverrideService(t, true)

	_, err := svc.Save(context.Background(), HostOverrideInput{
		Hostname: "shop.example.com", Address: "010.0.0.1", Active: true,
	})
	if err == nil {
		t.Fatal("an ambiguous address was accepted")
	}
	if fields := fieldsOf(t, err); !hasField(fields, "address") {
		t.Errorf("refused on %v, want a failure on address", fields)
	}
}

func TestHostOverrideAcceptsAnIPv6Route(t *testing.T) {
	// The dial address for IPv6 is "[::1]:443", so JoinHostPort has to be doing
	// the bracketing rather than a string concatenation somewhere.
	svc, _ := newHostOverrideService(t, true)

	if _, err := svc.Save(context.Background(), HostOverrideInput{
		Hostname: "shop.example.com", Address: "fd00::5", Active: true,
	}); err != nil {
		t.Fatalf("Save = %v", err)
	}

	got, ok := svc.Resolver().Lookup("shop.example.com:443")
	if !ok || got != "[fd00::5]:443" {
		t.Errorf("Lookup = %q, %v; want [fd00::5]:443", got, ok)
	}
}

func TestHostOverrideRefusesASecondRouteForTheSameHostAndPort(t *testing.T) {
	svc, _ := newHostOverrideService(t, true)
	ctx := context.Background()

	first := HostOverrideInput{Hostname: "shop.example.com", Address: "10.0.0.5", Active: true}
	if _, err := svc.Save(ctx, first); err != nil {
		t.Fatalf("the first route was refused: %v", err)
	}

	// Same host, same (absent) port, different address: which of the two would
	// apply is undefined, so it is refused rather than resolved by luck.
	_, err := svc.Save(ctx, HostOverrideInput{
		Hostname: "SHOP.example.com", Address: "10.0.0.6", Active: true,
	})
	if err == nil {
		t.Fatal("a duplicate route was accepted")
	}
	if fields := fieldsOf(t, err); !hasField(fields, "hostname") {
		t.Errorf("refused on %v, want a failure on hostname", fields)
	}

	// The same host on a DIFFERENT port is a different route and is allowed.
	if _, err := svc.Save(ctx, HostOverrideInput{
		Hostname: "shop.example.com", Address: "10.0.0.6", Port: "8443", Active: true,
	}); err != nil {
		t.Errorf("a route for a specific port was refused: %v", err)
	}
}

// --- the table the dialer reads --------------------------------------------

func TestSavingARouteMakesItLiveImmediately(t *testing.T) {
	// The screen must never show a row that is not yet in force.
	svc, _ := newHostOverrideService(t, true)

	if got, _ := svc.Resolver().Lookup("shop.example.com:443"); got != "shop.example.com:443" {
		t.Fatal("an empty resolver rerouted something")
	}

	if _, err := svc.Save(context.Background(), HostOverrideInput{
		Hostname: "shop.example.com", Address: "10.0.0.5", Active: true,
	}); err != nil {
		t.Fatalf("Save = %v", err)
	}

	got, ok := svc.Resolver().Lookup("shop.example.com:443")
	if !ok || got != "10.0.0.5:443" {
		t.Errorf("Lookup = %q, %v; want 10.0.0.5:443", got, ok)
	}
}

func TestDeletingARouteTakesItOutOfForce(t *testing.T) {
	svc, _ := newHostOverrideService(t, true)
	ctx := context.Background()

	row, err := svc.Save(ctx, HostOverrideInput{
		Hostname: "shop.example.com", Address: "10.0.0.5", Active: true,
	})
	if err != nil {
		t.Fatalf("Save = %v", err)
	}
	if err := svc.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if _, ok := svc.Resolver().Lookup("shop.example.com:443"); ok {
		t.Error("a deleted route is still being applied")
	}
}

func TestDeletingSomethingThatIsNotThere(t *testing.T) {
	svc, _ := newHostOverrideService(t, true)
	if err := svc.Delete(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete = %v, want ErrNotFound", err)
	}
}

func TestADisabledRouteIsNotApplied(t *testing.T) {
	// Disabled rather than deleted is how somebody keeps a route they may need
	// again. It has to be visible on the screen and absent from the dialer.
	svc, _ := newHostOverrideService(t, true)
	ctx := context.Background()

	row, err := svc.Save(ctx, HostOverrideInput{
		Hostname: "shop.example.com", Address: "10.0.0.5", Active: false,
	})
	if err != nil {
		t.Fatalf("Save = %v", err)
	}
	if _, ok := svc.Resolver().Lookup("shop.example.com:443"); ok {
		t.Error("a disabled route is being applied")
	}

	rows, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(rows) != 1 || rows[0].ID != row.ID {
		t.Errorf("List returned %d row(s); a disabled route must still be visible", len(rows))
	}
}

func TestReloadKeepsTheRoutesInForceWhenTheDatabaseIsUnreachable(t *testing.T) {
	// Emptying the table on a failed read would send every job back out through
	// the public address. That is the behaviour these rows exist to stop, and
	// it would present as an unrelated outage.
	svc, repo := newHostOverrideService(t, true)
	ctx := context.Background()

	if _, err := svc.Save(ctx, HostOverrideInput{
		Hostname: "shop.example.com", Address: "10.0.0.5", Active: true,
	}); err != nil {
		t.Fatalf("Save = %v", err)
	}

	repo.failReads(errors.New("connection refused"))
	svc.Reload(ctx)

	got, ok := svc.Resolver().Lookup("shop.example.com:443")
	if !ok || got != "10.0.0.5:443" {
		t.Errorf("Lookup after a failed reload = %q, %v; the route should still be in force", got, ok)
	}
}

func TestWatchKeepsTheTableCurrent(t *testing.T) {
	// A write reloads on the machine that made it. The watcher is how the
	// change reaches the OTHER replicas, which never saw the request.
	svc, repo := newHostOverrideService(t, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Watch(ctx, 10*time.Millisecond)

	deadline := time.After(3 * time.Second)
	for repo.reloads() < 3 {
		select {
		case <-deadline:
			t.Fatalf("the watcher reloaded %d time(s) in three seconds", repo.reloads())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// And it stops when the context does, rather than outliving the process's
	// shutdown.
	cancel()
	time.Sleep(50 * time.Millisecond)
	settled := repo.reloads()
	time.Sleep(100 * time.Millisecond)
	if got := repo.reloads(); got != settled {
		t.Errorf("the watcher reloaded %d more time(s) after its context was cancelled", got-settled)
	}
}

// --- the lookup itself ------------------------------------------------------

func TestResolverLookup(t *testing.T) {
	r := NewHostResolver()
	port := 8443
	r.Replace([]domain.HostOverride{
		{Hostname: "shop.example.com", Address: "10.0.0.5", Active: true},
		{Hostname: "api.example.com", Address: "10.0.0.9", Port: &port, Active: true},
		{Hostname: "off.example.com", Address: "10.0.0.7", Active: false},
	})

	cases := []struct {
		name string
		addr string
		want string
		ok   bool
	}{
		{"every port", "shop.example.com:443", "10.0.0.5:443", true},
		{"and the port is preserved", "shop.example.com:8080", "10.0.0.5:8080", true},
		{"matched without regard to case", "SHOP.Example.com:443", "10.0.0.5:443", true},
		{"a route for one port", "api.example.com:8443", "10.0.0.9:8443", true},
		{"and not for another", "api.example.com:443", "api.example.com:443", false},
		{"a host with no route", "other.example.com:443", "other.example.com:443", false},
		{"a disabled route", "off.example.com:443", "off.example.com:443", false},
		{"an address dialled directly", "10.1.2.3:443", "10.1.2.3:443", false},
		{"something that is not an address and port", "not-an-address", "not-an-address", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := r.Lookup(c.addr)
			if got != c.want || ok != c.ok {
				t.Errorf("Lookup(%q) = %q, %v; want %q, %v", c.addr, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestAPortSpecificRouteWinsOverTheGeneralOne(t *testing.T) {
	// So one service on a shared name can be moved without moving the rest.
	r := NewHostResolver()
	port := 8443
	r.Replace([]domain.HostOverride{
		{Hostname: "example.com", Address: "10.0.0.1", Active: true},
		{Hostname: "example.com", Address: "10.0.0.2", Port: &port, Active: true},
	})

	if got, _ := r.Lookup("example.com:8443"); got != "10.0.0.2:8443" {
		t.Errorf("the port-specific route did not win: %q", got)
	}
	if got, _ := r.Lookup("example.com:443"); got != "10.0.0.1:443" {
		t.Errorf("the general route did not apply: %q", got)
	}
}

func TestResolverIgnoresRowsItCannotUse(t *testing.T) {
	// Defence in depth. The service refuses these, but the resolver is fed
	// straight from the database and a row written by hand must not produce a
	// dial address that is not one.
	r := NewHostResolver()
	r.Replace([]domain.HostOverride{
		{Hostname: "", Address: "10.0.0.5", Active: true},
		{Hostname: "bad.example.com", Address: "not-an-ip", Active: true},
		{Hostname: "  ", Address: "10.0.0.6", Active: true},
	})
	if got := r.Len(); got != 0 {
		t.Errorf("the resolver kept %d unusable route(s)", got)
	}
}

func TestResolverIsSafeUnderConcurrentUseAndReload(t *testing.T) {
	// The dial path reads this on every connection while an administrator may
	// be saving. Run under -race, this is the test that the RWMutex is doing
	// its job rather than merely being present.
	r := NewHostResolver()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				// Whatever comes back must be a dial address, never a torn read.
				addr, _ := r.Lookup("shop.example.com:443")
				if _, _, err := net.SplitHostPort(addr); err != nil {
					t.Errorf("Lookup produced %q, which is not a dial address", addr)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		port := 8443
		for i := 0; i < 500; i++ {
			r.Replace([]domain.HostOverride{
				{Hostname: "shop.example.com", Address: "10.0.0.5", Active: true},
				{Hostname: "api.example.com", Address: "10.0.0.9", Port: &port, Active: true},
			})
			r.Replace(nil)
		}
	}()

	// The writer finishes on its own; the readers stop when it does.
	done := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
		close(done)
	}()
	<-done
	wg.Wait()
}
