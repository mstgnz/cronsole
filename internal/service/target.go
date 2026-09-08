package service

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// This file decides what address a job is allowed to call, and it is the whole
// of that decision. Two callers use it: the job form, so a bad target is
// refused before it is stored, and the runner, so a row edited around the
// application is refused before it is fired.
//
// The threat is not an anonymous attacker. Reaching either caller already
// requires an operator account or a project key. The threat is that a service
// whose entire purpose is issuing HTTP requests on a schedule is a very
// convenient thing to point somewhere it should not go, and containment is
// cheap here and expensive later.

// slugPattern is what a project slug and a job code may contain. Narrow on
// purpose: both end up in URLs, log lines and alert subjects.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)

// ValidSlug reports whether a project slug or job code is acceptable.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// Slugify turns a name into a usable slug, so the form can offer one rather
// than asking for it.
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '_':
			b.WriteRune('_')
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-_")
	}
	return out
}

// TargetPolicy decides which addresses may be called.
type TargetPolicy struct {
	// AllowPrivate permits loopback, link local and private ranges.
	//
	// It defaults to true in this service, and that is correct rather than
	// careless: the reason the thing exists is calling internal endpoints that
	// have no public address. A deployment whose jobs are all public turns it
	// off and gets the ordinary SSRF guard back.
	AllowPrivate bool
}

// Resolve turns a project base address and a job target into the URL to call.
//
// When the project declares a base address, the target must be relative and
// the host comes from the project. That is the containment mechanism: an
// account that can edit jobs still cannot move one to a different host,
// because the host is not a field it can write.
func (p TargetPolicy) Resolve(baseURL, target string) (string, error) {
	target = strings.TrimSpace(target)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")

	if target == "" {
		return "", fmt.Errorf("target address is empty")
	}
	// A backslash is rejected outright. Some clients normalise it to a slash
	// and some do not, which is the entire basis of the "\\evil.com" trick.
	if strings.ContainsAny(target, "\\ \t\r\n") {
		return "", fmt.Errorf("target address contains an illegal character")
	}

	if baseURL != "" {
		if hasScheme(target) {
			return "", fmt.Errorf("this project has a base address, so the target must be a path such as /cron/report")
		}
		if !strings.HasPrefix(target, "/") {
			target = "/" + target
		}
		// A protocol relative path would escape the base address entirely.
		if strings.HasPrefix(target, "//") {
			return "", fmt.Errorf("target path may not start with //")
		}
		return p.check(baseURL + target)
	}

	if !hasScheme(target) {
		return "", fmt.Errorf("target must be a full address starting with http:// or https://, or the project must declare a base address")
	}
	return p.check(target)
}

func hasScheme(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// check validates a fully formed address.
func (p TargetPolicy) check(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("target address cannot be parsed: %w", err)
	}
	// Scheme is an allowlist, never a denylist. file://, gopher:// and the
	// rest are not enumerated here; anything that is not http or https is out.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("only http and https targets are allowed, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("target address has no host")
	}
	// Credentials in the address would be written into every run row.
	if parsed.User != nil {
		return "", fmt.Errorf("target address may not carry credentials")
	}

	if !p.AllowPrivate {
		if err := rejectPrivate(parsed.Hostname()); err != nil {
			return "", err
		}
	}
	return parsed.String(), nil
}

// rejectPrivate refuses addresses on ranges that reach the infrastructure
// rather than a service.
//
// It is a literal check, not a DNS resolution: resolving here and connecting
// later is a time of check to time of use gap, and the honest containment for
// this service is the project base address above, not a name lookup.
func rejectPrivate(host string) error {
	if host == "" {
		return fmt.Errorf("target address has no host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("target address %q is not reachable from this deployment", host)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// A name, not an address. Left to DNS and to the network the service
		// runs on.
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("target address %q is on a private range", host)
	}
	// The cloud metadata endpoint is link local and already caught above; it
	// is named separately because it is the one that matters and a reader
	// should be able to find it.
	if ip.String() == "169.254.169.254" {
		return fmt.Errorf("target address %q is the instance metadata endpoint", host)
	}
	return nil
}

// ValidHeaderName reports whether a header name is safe to send.
//
// Newlines are what makes this a check rather than a formality: a name or
// value carrying CR or LF splits the request and lets a second one be smuggled
// after it.
func ValidHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r <= 32 || r >= 127 {
			return false
		}
		if strings.ContainsRune(":()<>@,;\\\"/[]?={}", r) {
			return false
		}
	}
	return true
}

// ValidHeaderValue reports whether a header value is safe to send.
func ValidHeaderValue(value string) bool {
	if len(value) > 4096 {
		return false
	}
	return !strings.ContainsAny(value, "\r\n\x00")
}

// MaskSecret renders a secret value for display without revealing it.
func MaskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 4 {
		return strings.Repeat("*", len(v))
	}
	return strings.Repeat("*", len(v)-4) + v[len(v)-4:]
}
