package service

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/repository"
)

// This file is the whole of the host override decision: what may be stored, and
// what the dialer does with it.
//
// Read the security note in cronsole.sql before changing anything here. A
// hostname-to-address map is an SSRF primitive, and the only reason it is safe
// is that it is platform-administrator only, the address is an IP literal, and
// a deployment that forbids private targets also forbids private routes.

// hostnamePattern is what may be redirected.
//
// Deliberately a plain DNS name: labels of letters, digits and hyphens,
// separated by dots. No wildcard, no suffix match, no regular expression from
// the operator. A wildcard would let one row capture hosts nobody enumerated,
// and the point of this table is that every route is a line somebody can read.
var hostnamePattern = regexp.MustCompile(
	`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// HostOverrideService owns the routes and the cache the dialer reads.
type HostOverrideService struct {
	repo   domain.HostOverrideRepository
	policy TargetPolicy
	log    *applog.Logger

	resolver *HostResolver
}

// NewHostOverrideService wires the service.
func NewHostOverrideService(repo domain.HostOverrideRepository, policy TargetPolicy,
	log *applog.Logger) *HostOverrideService {

	return &HostOverrideService{
		repo:     repo,
		policy:   policy,
		log:      log,
		resolver: NewHostResolver(),
	}
}

// Resolver is the table the dialer consults.
func (s *HostOverrideService) Resolver() *HostResolver { return s.resolver }

// List returns every route, disabled ones included.
func (s *HostOverrideService) List(ctx context.Context) ([]domain.HostOverride, error) {
	return s.repo.List(ctx)
}

// HostOverrideInput is what the screen submits.
type HostOverrideInput struct {
	ID       int64
	Hostname string
	Address  string
	// Port empty means every port.
	Port   string
	Note   string
	Active bool
	// Actor is the account making the change, recorded on the row. A route is
	// a change to where traffic goes and must not be anonymous.
	Actor *int64
}

// Save creates or updates a route.
//
// Every rule that makes this safe is enforced here rather than at the screen,
// because the screen is one caller and the next one will not remember.
func (s *HostOverrideService) Save(ctx context.Context, in HostOverrideInput) (*domain.HostOverride, error) {
	v := &ValidationError{}

	hostname := strings.ToLower(strings.TrimSpace(in.Hostname))
	switch {
	case hostname == "":
		v.Add("hostname", "is required")
	case len(hostname) > 253:
		v.Add("hostname", "is too long")
	case net.ParseIP(hostname) != nil:
		// Redirecting an address to an address is not a route, it is a way to
		// make a target policy decision unreadable.
		v.Add("hostname", "must be a host name, not an address")
	case !hostnamePattern.MatchString(hostname):
		v.Add("hostname", "must be a plain host name, without a scheme, port or path")
	}

	address := strings.TrimSpace(in.Address)
	ip := net.ParseIP(address)
	switch {
	case address == "":
		v.Add("address", "is required")
	case ip == nil:
		// A name here would be resolved through DNS, which is the round trip
		// this exists to avoid, and would make the route mean something
		// different tomorrow.
		v.Add("address", "must be an IP address, not a host name")
	case ip.IsUnspecified():
		v.Add("address", "cannot be the unspecified address")
	case ip.IsMulticast():
		v.Add("address", "cannot be a multicast address")
	case !s.policy.AllowPrivate && !isPublicIP(ip):
		// OUTBOUND_ALLOW_PRIVATE=false says this deployment's jobs are all
		// public. A route to a private address would walk straight past that,
		// which is the SSRF the setting exists to prevent.
		v.Add("address", "is private, and this deployment does not permit private targets")
	}

	var port *int
	if raw := strings.TrimSpace(in.Port); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			v.Add("port", "must be between 1 and 65535, or empty for every port")
		} else {
			port = &n
		}
	}

	if len(in.Note) > 500 {
		v.Add("note", "is too long")
	}

	if !v.OK() {
		return nil, v
	}

	row := &domain.HostOverride{
		ID:        in.ID,
		Hostname:  hostname,
		Address:   ip.String(),
		Port:      port,
		Note:      strings.TrimSpace(in.Note),
		Active:    in.Active,
		CreatedBy: in.Actor,
	}

	var err error
	if row.ID == 0 {
		row.ID, err = s.repo.Create(ctx, row)
	} else {
		err = s.repo.Update(ctx, row)
	}
	switch {
	case errors.Is(err, repository.ErrDuplicate):
		return nil, (&ValidationError{}).Add("hostname",
			"already has a route for that port")
	case errors.Is(err, repository.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}

	// The route is live from here. Reloading before returning means the screen
	// never shows a row that is not yet in force.
	s.Reload(ctx)
	return row, nil
}

// Delete removes a route and takes it out of the table.
func (s *HostOverrideService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.Reload(ctx)
	return nil
}

// Reload replaces the dialer's table from the database.
//
// It never leaves the table empty on a failure: an unreachable database must
// not quietly send every job back out through the public address, which is the
// behaviour these rows exist to stop and would look like an unrelated outage.
func (s *HostOverrideService) Reload(ctx context.Context) {
	rows, err := s.repo.ListActive(ctx)
	if err != nil {
		s.log.Error("host overrides: could not be reloaded, keeping the routes in force", err.Error())
		return
	}
	s.resolver.Replace(rows)
}

// Watch keeps the table current until the context is cancelled.
//
// A write reloads immediately, so this is not how a change reaches the dialer
// on the machine that made it. It is how a change reaches the OTHER replicas,
// which have no idea a row was written; the interval is therefore the bound on
// how long a route can be stale across the deployment.
func (s *HostOverrideService) Watch(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Bounded on its own, not on the process's shutdown context: a
				// reload that hangs must not hold the ticker.
				readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				s.Reload(readCtx)
				cancel()
			}
		}
	}()
}

// HostResolver is the lookup the dialer performs on every connection.
//
// Read far more often than written, so a RWMutex over two prepared maps: the
// dial path takes a read lock and does two map lookups, and a reload swaps the
// maps under a write lock. Rebuilding on read, or holding a full mutex, would
// put the cost on the connection rather than on the change.
type HostResolver struct {
	mu sync.RWMutex
	// exact is keyed "hostname:port" and wins, so one service can be redirected
	// without moving everything else on the same name.
	exact map[string]string
	// any is keyed "hostname" and applies to every port.
	any map[string]string
}

// NewHostResolver builds an empty resolver, which routes nothing.
func NewHostResolver() *HostResolver {
	return &HostResolver{exact: map[string]string{}, any: map[string]string{}}
}

// Replace swaps in a new table.
func (r *HostResolver) Replace(rows []domain.HostOverride) {
	exact := make(map[string]string, len(rows))
	general := make(map[string]string, len(rows))

	for _, row := range rows {
		if !row.Active {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(row.Hostname))
		if host == "" || net.ParseIP(row.Address) == nil {
			continue
		}
		if row.Port != nil {
			exact[host+":"+itoa(*row.Port)] = row.Address
			continue
		}
		general[host] = row.Address
	}

	r.mu.Lock()
	r.exact, r.any = exact, general
	r.mu.Unlock()
}

// Lookup returns the address to dial instead of addr, and whether one applies.
//
// addr is what the transport hands the dialer, always "host:port". Only the
// address changes: the Host header and the TLS server name are set from the URL
// long before this, so the certificate still has to match the hostname. That is
// the whole reason this is done here and not by rewriting the job's URL.
func (r *HostResolver) Lookup(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, false
	}
	host = strings.ToLower(host)

	r.mu.RLock()
	replacement, ok := r.exact[host+":"+port]
	if !ok {
		replacement, ok = r.any[host]
	}
	r.mu.RUnlock()

	if !ok {
		return addr, false
	}
	return net.JoinHostPort(replacement, port), true
}

// Len is how many routes are in force, for the screen and for tests.
func (r *HostResolver) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.exact) + len(r.any)
}

// isPublicIP reports whether an address is outside the ranges a target policy
// calls private. Loopback, link local, private and unique local all count as
// private here, which matches TargetPolicy.
func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified())
}
