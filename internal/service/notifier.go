package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/domain"
)

// MailSender is the transport for outgoing mail. The service knows nothing
// about SMTP; an adapter supplies this.
type MailSender interface {
	Send(to []string, subject, body string) error
}

// Notifier turns a finished run into a message.
//
// Sending happens off the run's own path. An SMTP handshake takes seconds and
// can hang, and a run that has already finished must not be held open waiting
// for a mail server: the row is written first, the message goes afterwards.
type Notifier struct {
	sender MailSender
	log    *applog.Logger
	queue  chan mailJob

	baseURL string

	stopOnce sync.Once
	done     chan struct{}
}

type mailJob struct {
	to      []string
	subject string
	body    string
}

// notifyQueueSize bounds the backlog. A storm of failures must not turn into
// an unbounded mail queue holding the whole outage in memory.
const notifyQueueSize = 256

// NewNotifier builds a notifier and starts its sender goroutine. A nil sender
// is allowed and means nothing is sent.
func NewNotifier(sender MailSender, log *applog.Logger, baseURL string) *Notifier {
	n := &Notifier{
		sender:  sender,
		log:     log,
		queue:   make(chan mailJob, notifyQueueSize),
		baseURL: strings.TrimRight(baseURL, "/"),
		done:    make(chan struct{}),
	}
	go n.run()
	return n
}

func (n *Notifier) run() {
	for job := range n.queue {
		if n.sender == nil {
			continue
		}
		if err := n.sender.Send(job.to, job.subject, job.body); err != nil {
			n.log.Warn("notifier: send failed", err.Error(), "subject", job.subject)
		}
	}
	close(n.done)
}

// Close drains the queue, bounded by ctx.
func (n *Notifier) Close(ctx context.Context) {
	n.stopOnce.Do(func() { close(n.queue) })
	select {
	case <-n.done:
	case <-ctx.Done():
	}
}

func (n *Notifier) enqueue(to []string, subject, body string) {
	if len(to) == 0 {
		return
	}
	select {
	case n.queue <- mailJob{to: to, subject: subject, body: body}:
	default:
		n.log.Warn("notifier: queue full, message dropped", subject)
	}
}

// RunFinished reports the outcome of one run.
func (n *Notifier) RunFinished(to []string, row *domain.RunTarget, result domain.RunResult) {
	verb := "succeeded"
	if result.Status != domain.StatusSuccess {
		verb = strings.ToUpper(result.Status)
	}
	subject := fmt.Sprintf("[cron] %s / %s %s", row.ProjectSlug, row.Code, verb)

	var b strings.Builder
	fmt.Fprintf(&b, "Job:      %s (%s)\n", row.Name, row.Code)
	fmt.Fprintf(&b, "Project:  %s\n", row.ProjectSlug)
	fmt.Fprintf(&b, "Status:   %s\n", result.Status)
	fmt.Fprintf(&b, "Duration: %d ms\n", result.DurationMs)
	if result.HTTPStatus > 0 {
		fmt.Fprintf(&b, "HTTP:     %d\n", result.HTTPStatus)
	}
	if result.RequestURL != "" {
		fmt.Fprintf(&b, "Target:   %s\n", result.RequestURL)
	}
	if result.Error != "" {
		fmt.Fprintf(&b, "\nError:\n%s\n", result.Error)
	}
	if trimmed := strings.TrimSpace(result.Output); trimmed != "" {
		fmt.Fprintf(&b, "\nResponse (first 1000 characters):\n%s\n", firstRunes(trimmed, 1000))
	}
	if n.baseURL != "" {
		fmt.Fprintf(&b, "\n%s/runs?job_id=%d\n", n.baseURL, row.JobID)
	}

	n.enqueue(to, subject, b.String())
}

// Alert sends a watchdog warning.
func (n *Notifier) Alert(to []string, subject, body string) {
	if n.baseURL != "" {
		body += "\n" + n.baseURL + "\n"
	}
	n.enqueue(to, subject, body)
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ..."
}

// newOutboundClient builds the client used to call job targets.
//
// One client, not one per request: every construction opens a new connection
// pool, and dozens of jobs run per minute.
//
// Redirects are not followed. A cron target answering 302 is a fact worth
// recording, not something to silently resolve somewhere else, and following
// one is also how a target on an allowed host lands on a host that is not.
//
// routes may be nil, which dials exactly what DNS says.
func newOutboundClient(routes *HostResolver) *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// The host override is applied HERE and nowhere else, on the dial address
	// alone. The request's Host header and the TLS server name were set from
	// the URL long before this, so a redirected host still has to present a
	// certificate for its own name: this is `curl --resolve`, not a rewrite of
	// the target. Doing it by editing the job's URL would lose both.
	dial := dialer.DialContext
	if routes != nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if replaced, ok := routes.Lookup(addr); ok {
				addr = replaced
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &http.Client{
		// The real limit is the per attempt context deadline. This is only a
		// backstop, set above the highest per job timeout the schema allows.
		Timeout: time.Duration(domain.MaxTimeoutSec+30) * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext:           dial,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: time.Duration(domain.MaxTimeoutSec) * time.Second,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   10,
		},
	}
}
