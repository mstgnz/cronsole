package domain

import (
	"context"
	"time"
)

// HostOverride sends a hostname to a fixed address without changing the job
// that names it.
//
// Cronsole is installed on a private network and calls services on that
// network by their public names. Public DNS points those names at whatever sits
// in front of them, so the request leaves the machine and comes back through a
// CDN to reach a service one hop away. A row here dials the address directly
// while the Host header and the TLS server name stay the hostname, which is
// what `curl --resolve` does and why certificates still validate.
type HostOverride struct {
	ID       int64  `json:"id"`
	Hostname string `json:"hostname"`
	// Address is an IP literal. Never a name: a name here would mean a second
	// DNS lookup, which is the thing this exists to avoid.
	Address string `json:"address"`
	// Port nil means every port. A row naming a port wins over one that does
	// not, so a single service can be redirected without moving the rest.
	Port      *int      `json:"port,omitempty"`
	Note      string    `json:"note"`
	Active    bool      `json:"active"`
	CreatedBy *int64    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HostOverrideRepository stores the routes.
//
// ListActive is separate from List because the dialer reloads on a timer and
// wants only what applies, while the screen shows the disabled rows too so
// somebody can see what was turned off rather than deleted.
type HostOverrideRepository interface {
	List(ctx context.Context) ([]HostOverride, error)
	ListActive(ctx context.Context) ([]HostOverride, error)
	Get(ctx context.Context, id int64) (*HostOverride, error)
	Create(ctx context.Context, o *HostOverride) (int64, error)
	Update(ctx context.Context, o *HostOverride) error
	Delete(ctx context.Context, id int64) error
}
