package auth

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- the limiter ------------------------------------------------------------

func TestLimiterAllowsUpToTheLimit(t *testing.T) {
	l := NewLimiter(3, time.Minute)

	for i := 1; i <= 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("attempt %d was refused, the limit is 3", i)
		}
	}
	if l.Allow("a") {
		t.Error("the fourth attempt was allowed")
	}
}

func TestLimiterCountsEachKeySeparately(t *testing.T) {
	// One address exhausting its bucket must not lock out everybody else, which
	// is what a single global counter would do.
	l := NewLimiter(1, time.Minute)

	if !l.Allow("a") || l.Allow("a") {
		t.Fatal("the first key did not behave")
	}
	if !l.Allow("b") {
		t.Error("a second key was refused because the first was exhausted")
	}
}

func TestLimiterWindowExpires(t *testing.T) {
	l := NewLimiter(1, 20*time.Millisecond)

	if !l.Allow("a") {
		t.Fatal("the first attempt was refused")
	}
	if l.Allow("a") {
		t.Fatal("the second attempt inside the window was allowed")
	}

	time.Sleep(40 * time.Millisecond)
	if !l.Allow("a") {
		t.Error("the window did not expire")
	}
}

func TestResetClearsTheCount(t *testing.T) {
	// Called after a successful sign in, so somebody who eventually types the
	// right password is not still being counted for the tries before it.
	l := NewLimiter(2, time.Minute)

	l.Allow("a")
	l.Allow("a")
	if l.Allow("a") {
		t.Fatal("the bucket was not exhausted")
	}

	l.Reset("a")
	if !l.Allow("a") {
		t.Error("Reset did not clear the count")
	}
}

func TestLimiterDoesNotGrowWithoutBound(t *testing.T) {
	// Every distinct key allocates. Without the sweep, a spray from many
	// addresses is a memory leak that outlives the attack.
	l := NewLimiter(5, 10*time.Millisecond)

	for i := 0; i < 500; i++ {
		l.Allow(string(rune('a' + i%26)))
	}

	// Past the window, the next call sweeps what expired.
	time.Sleep(30 * time.Millisecond)
	l.Allow("trigger the sweep")

	l.mu.Lock()
	held := len(l.hits)
	l.mu.Unlock()

	if held > 26 {
		t.Errorf("the limiter is holding %d windows after they expired", held)
	}
}

func TestLimiterIsSafeUnderConcurrentUse(t *testing.T) {
	// Every rate-limited route calls this from its own goroutine. Under -race
	// this is the test that the mutex covers the counter as well as the map.
	const goroutines, each = 16, 50
	l := NewLimiter(goroutines*each, time.Minute)

	var wg sync.WaitGroup
	allowed := make(chan bool, goroutines*each)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				allowed <- l.Allow("shared")
			}
		}()
	}
	wg.Wait()
	close(allowed)

	// Exactly the limit was permitted: no attempt was lost and none was
	// double-counted.
	count := 0
	for ok := range allowed {
		if ok {
			count++
		}
	}
	if count != goroutines*each {
		t.Errorf("%d of %d attempts were allowed", count, goroutines*each)
	}
	if l.Allow("shared") {
		t.Error("the limit was exceeded")
	}
}

// --- which address a request is attributed to -------------------------------

func requestFrom(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestTheDefaultTrustsNoHeaderAtAll(t *testing.T) {
	// THE test in this file. A forwarding header is an ordinary request header:
	// anything that reads one without knowing a trusted proxy set it lets the
	// caller pick their own bucket, and a caller who picks their own bucket has
	// no rate limit. Cronsole is installed on somebody's own VM, where directly
	// exposed is the normal case, so the default has to be to trust nothing.
	var p TrustedProxy

	forged := map[string]string{
		"X-Forwarded-For":        "1.2.3.4",
		"X-Real-IP":              "1.2.3.4",
		"CF-Connecting-IP":       "1.2.3.4",
		"True-Client-IP":         "1.2.3.4",
		"X-Vercel-Forwarded-For": "1.2.3.4",
	}
	for header, value := range forged {
		got := p.ClientIP(requestFrom("203.0.113.9:51000", map[string]string{header: value}))
		if got != "203.0.113.9" {
			t.Errorf("%s was trusted with no proxy configured: got %q", header, got)
		}
	}
}

func TestAForgedHeaderCannotBuyAFreshBucket(t *testing.T) {
	// The attack the default prevents, written out: without it, ten attempts
	// with ten different header values are ten different buckets and the login
	// form is an unlimited password oracle.
	var p TrustedProxy
	l := NewLimiter(3, time.Minute)

	allowed := 0
	for i := 0; i < 10; i++ {
		r := requestFrom("203.0.113.9:51000", map[string]string{
			"CF-Connecting-IP": "10.0.0." + string(rune('0'+i)),
		})
		if l.Allow(p.ClientIP(r)) {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("%d attempts got through a limit of 3", allowed)
	}
}

func TestNewTrustedProxyRefusesAHeaderItWillNotVouchFor(t *testing.T) {
	// An unrecognised name is an error rather than a silent fallback. Both are
	// secure; only one tells the operator their configuration does nothing.
	for _, header := range []string{"X-Client-IP", "Forwarded", "X-Custom", "Cookie"} {
		if _, err := NewTrustedProxy(header, 1); err == nil {
			t.Errorf("%q was accepted as a trusted header", header)
		}
	}
}

func TestNewTrustedProxyAcceptsTheKnownHeadersInAnyCasing(t *testing.T) {
	for _, header := range []string{
		"X-Forwarded-For", "x-forwarded-for", "X-FORWARDED-FOR",
		"X-Real-IP", "cf-connecting-ip", "True-Client-IP", "x-vercel-forwarded-for",
	} {
		if _, err := NewTrustedProxy(header, 1); err != nil {
			t.Errorf("NewTrustedProxy(%q) = %v", header, err)
		}
	}
}

func TestForwardedForNeedsAHopCount(t *testing.T) {
	// Without one there is no way to tell the client's entry from whatever the
	// caller prepended, and guessing means trusting the leftmost.
	for _, hops := range []int{0, -1} {
		if _, err := NewTrustedProxy("X-Forwarded-For", hops); err == nil {
			t.Errorf("X-Forwarded-For was accepted with %d hops", hops)
		}
	}
}

func TestSingleValueHeadersIgnoreTheHopCount(t *testing.T) {
	// A proxy overwrites these rather than appending, so there is nothing to
	// count and carrying a hop count would imply otherwise.
	p, err := NewTrustedProxy("CF-Connecting-IP", 5)
	if err != nil {
		t.Fatalf("NewTrustedProxy = %v", err)
	}
	if p.hops != 0 {
		t.Errorf("hops = %d, want 0", p.hops)
	}
	got := p.ClientIP(requestFrom("10.0.0.1:1000", map[string]string{"CF-Connecting-IP": "1.2.3.4"}))
	if got != "1.2.3.4" {
		t.Errorf("ClientIP = %q, want the header value", got)
	}
}

func TestForwardedForCountsFromTheRight(t *testing.T) {
	// "<client>, <proxy1>, <proxy2>": everything a proxy appended is
	// trustworthy and everything left of that is whatever the caller sent, so
	// the client is counted from the right by the number of proxies in front.
	cases := []struct {
		name   string
		hops   int
		header string
		want   string
	}{
		{
			name: "one proxy", hops: 1,
			header: "203.0.113.9", want: "203.0.113.9",
		},
		{
			name: "one proxy, and the caller prepended a lie", hops: 1,
			header: "1.2.3.4, 203.0.113.9", want: "203.0.113.9",
		},
		{
			name: "two proxies", hops: 2,
			header: "203.0.113.9, 10.0.0.1", want: "203.0.113.9",
		},
		{
			name: "two proxies, and the caller prepended two lies", hops: 2,
			header: "1.2.3.4, 5.6.7.8, 203.0.113.9, 10.0.0.1", want: "203.0.113.9",
		},
		{
			name: "spaces around the entries", hops: 1,
			header: "  1.2.3.4 ,  203.0.113.9  ", want: "203.0.113.9",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewTrustedProxy("X-Forwarded-For", c.hops)
			if err != nil {
				t.Fatalf("NewTrustedProxy = %v", err)
			}
			got := p.ClientIP(requestFrom("192.0.2.1:9000", map[string]string{"X-Forwarded-For": c.header}))
			if got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFallsBackToTheSocketWhenTheChainIsNotWhatWasConfigured(t *testing.T) {
	// Fewer entries than there are proxies means the request did not come
	// through the chain the configuration describes. Trusting the leftmost here
	// is exactly the mistake TrustedProxy exists to prevent.
	p, err := NewTrustedProxy("X-Forwarded-For", 3)
	if err != nil {
		t.Fatalf("NewTrustedProxy = %v", err)
	}

	got := p.ClientIP(requestFrom("192.0.2.1:9000", map[string]string{"X-Forwarded-For": "1.2.3.4"}))
	if got != "192.0.2.1" {
		t.Errorf("ClientIP = %q, want the socket address", got)
	}
}

func TestRubbishInAHeaderBuysNothing(t *testing.T) {
	// A value that is not an address must not become a bucket key of its own.
	// Falling back to the socket puts the caller in the bucket they were
	// already in, rather than a fresh one.
	p, err := NewTrustedProxy("X-Real-IP", 1)
	if err != nil {
		t.Fatalf("NewTrustedProxy = %v", err)
	}

	for _, value := range []string{"not-an-ip", "'; DROP TABLE users;--", "999.999.999.999", "  "} {
		got := p.ClientIP(requestFrom("192.0.2.1:9000", map[string]string{"X-Real-IP": value}))
		if got != "192.0.2.1" {
			t.Errorf("X-Real-IP %q produced %q, want the socket address", value, got)
		}
	}
}

func TestAnAbsentHeaderFallsBackToTheSocket(t *testing.T) {
	p, err := NewTrustedProxy("CF-Connecting-IP", 1)
	if err != nil {
		t.Fatalf("NewTrustedProxy = %v", err)
	}
	if got := p.ClientIP(requestFrom("192.0.2.1:9000", nil)); got != "192.0.2.1" {
		t.Errorf("ClientIP = %q, want the socket address", got)
	}
}

func TestAddressesAreCanonicalised(t *testing.T) {
	// Two spellings of one address must be one bucket, or the limit is per
	// spelling.
	p, err := NewTrustedProxy("X-Real-IP", 1)
	if err != nil {
		t.Fatalf("NewTrustedProxy = %v", err)
	}

	cases := map[string]string{
		"2001:DB8::0001":   "2001:db8::1",
		"[2001:db8::1]:80": "2001:db8::1",
		"203.0.113.9:4444": "203.0.113.9",
	}
	for value, want := range cases {
		if got := p.ClientIP(requestFrom("192.0.2.1:9000", map[string]string{"X-Real-IP": value})); got != want {
			t.Errorf("X-Real-IP %q = %q, want %q", value, got, want)
		}
	}
}

func TestASocketWithNoPortIsStillUsable(t *testing.T) {
	// Not every server hands RemoteAddr over with a port, and returning an
	// empty key would put every such request in one bucket.
	var p TrustedProxy
	if got := p.ClientIP(requestFrom("192.0.2.1", nil)); got != "192.0.2.1" {
		t.Errorf("ClientIP = %q, want 192.0.2.1", got)
	}
}
