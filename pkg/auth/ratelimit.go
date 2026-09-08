package auth

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter is a fixed window counter keyed by an arbitrary string.
// It is in-process on purpose: the application is deployed as a single instance,
// and a shared store would be needed only once that stops being true.
type Limiter struct {
	mu      sync.Mutex
	hits    map[string]*window
	limit   int
	period  time.Duration
	lastGC  time.Time
	gcEvery time.Duration
}

type window struct {
	count     int
	expiresAt time.Time
}

// NewLimiter allows limit attempts per key within period.
func NewLimiter(limit int, period time.Duration) *Limiter {
	return &Limiter{
		hits:    make(map[string]*window),
		limit:   limit,
		period:  period,
		gcEvery: period,
	}
}

// Allow records an attempt and reports whether it stays within the limit.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.collect(now)

	w, ok := l.hits[key]
	if !ok || now.After(w.expiresAt) {
		l.hits[key] = &window{count: 1, expiresAt: now.Add(l.period)}
		// Compared rather than returning true outright, so a limit of zero
		// permits nothing. Returning true unconditionally made the first
		// attempt in every window free of the limit, which reads as an
		// off-by-one and is a way to configure a limiter that does nothing.
		return 1 <= l.limit
	}

	w.count++
	return w.count <= l.limit
}

// Reset drops the counter for a key, called after a successful attempt so a user
// who eventually types the right password is not punished for the earlier tries.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// collect removes expired windows so the map cannot grow without bound.
// The caller must hold the lock.
func (l *Limiter) collect(now time.Time) {
	if now.Sub(l.lastGC) < l.gcEvery {
		return
	}
	l.lastGC = now
	for key, w := range l.hits {
		if now.After(w.expiresAt) {
			delete(l.hits, key)
		}
	}
}

// TrustedProxy decides which address a request is attributed to.
//
// This is the foundation the rate limiter stands on, and getting it wrong
// removes the limiter entirely rather than weakening it. A forwarding header is
// just a request header: anything that reads one without knowing a trusted
// proxy put it there is letting the caller choose their own bucket, and a
// caller who can choose their own bucket has no limit at all.
//
// So the default trusts NOTHING and uses the socket address. A deployment
// behind a proxy names the header that proxy sets, and only then is it read.
// That default is right for this service specifically: Cronsole is installed on
// somebody's own VM, where being directly exposed is the normal case and being
// behind Cloudflare is the exception.
type TrustedProxy struct {
	// header is the canonical header name to trust. Empty trusts none.
	header string
	// hops is how many proxies append to X-Forwarded-For, so the client's own
	// entry can be counted from the right. Meaningless for the single-value
	// headers, which a proxy overwrites rather than appends to.
	hops int
}

// forwardedFor is the one header that accumulates rather than being overwritten,
// which is why it needs a hop count and the others do not.
const forwardedFor = "X-Forwarded-For"

// trustableHeaders are the headers a proxy may be configured to be trusted for.
//
// An allowlist rather than free text: a header name that reaches this from
// configuration decides who is rate limited, and a typo silently trusting
// nothing looks identical to a deployment that is working.
var trustableHeaders = map[string]bool{
	forwardedFor:             true,
	"X-Real-Ip":              true,
	"Cf-Connecting-Ip":       true,
	"True-Client-Ip":         true,
	"X-Vercel-Forwarded-For": true,
}

// NewTrustedProxy builds the resolver.
//
// An empty header trusts nothing, which is the default and the safe end. An
// unrecognised header is an error rather than a silent fallback: both would be
// secure, but only one of them tells the operator their configuration does
// nothing.
func NewTrustedProxy(header string, hops int) (TrustedProxy, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return TrustedProxy{}, nil
	}

	canonical := http.CanonicalHeaderKey(header)
	if !trustableHeaders[canonical] {
		return TrustedProxy{}, fmt.Errorf(
			"auth: %q is not a header this service will trust for the client address", header)
	}
	if canonical == forwardedFor {
		if hops < 1 {
			return TrustedProxy{}, fmt.Errorf(
				"auth: %s needs a hop count of at least 1, so the client's own entry can be counted from the right",
				forwardedFor)
		}
	} else {
		// A single-value header is overwritten by the proxy, so there is
		// nothing to count. Carrying a hop count would imply otherwise.
		hops = 0
	}
	return TrustedProxy{header: canonical, hops: hops}, nil
}

// ClientIP returns the address to attribute a request to.
//
// It never returns a value the caller invented. Anything that is not a valid IP
// address falls back to the socket, so a header full of rubbish buys a caller
// the bucket they were already in rather than a fresh one.
func (p TrustedProxy) ClientIP(r *http.Request) string {
	socket := socketIP(r)
	if p.header == "" {
		return socket
	}

	raw := r.Header.Get(p.header)
	if strings.TrimSpace(raw) == "" {
		return socket
	}

	if p.header != forwardedFor {
		if ip := parseIP(raw); ip != "" {
			return ip
		}
		return socket
	}

	// X-Forwarded-For accumulates left to right: "<client>, <proxy1>, <proxy2>".
	// Everything a proxy appended is trustworthy and everything to the left of
	// that is whatever the caller sent, so the client is counted from the RIGHT
	// by the number of proxies in front of this service.
	parts := strings.Split(raw, ",")
	index := len(parts) - p.hops
	if index < 0 || index >= len(parts) {
		// Fewer entries than there are proxies means the chain is not what the
		// configuration says it is. Trusting the leftmost here is exactly the
		// mistake this type exists to prevent.
		return socket
	}
	if ip := parseIP(parts[index]); ip != "" {
		return ip
	}
	return socket
}

// parseIP returns the address if it is one, and "" otherwise. A port is
// tolerated because some proxies append one.
func parseIP(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
			return ip.String()
		}
	}
	return ""
}

func socketIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
