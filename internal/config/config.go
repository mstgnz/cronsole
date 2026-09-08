// Package config reads the environment once, into a typed value.
//
// Every setting is read here and nowhere else. A package that calls
// os.Getenv itself is a setting that cannot be found, cannot be validated at
// boot, and behaves differently depending on which code path reached it first.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the container image has no zoneinfo of its own
)

// Config is the whole of the application's configuration.
type Config struct {
	App        App
	DB         DB
	Mail       Mail
	Scheduler  Scheduler
	Watchdog   Watchdog
	Outbound   Outbound
	Docker     Docker
	JWTSecret  string
	InstanceID string
}

// App is process level settings.
type App struct {
	Name     string
	Env      string
	Port     string
	Debug    bool
	BaseURL  string
	Location *time.Location
	// TrustedProxyHeader names the forwarding header this deployment sits
	// behind, and is EMPTY by default.
	//
	// Empty means the rate limiters key on the socket address and no request
	// header can change that. It has to default to off: a forwarding header is
	// an ordinary request header, so reading one without knowing a proxy set it
	// lets the caller choose their own bucket, and the login limiter stops
	// existing. Cronsole is installed on somebody's own VM, where directly
	// exposed is the normal case.
	//
	// Set it to X-Forwarded-For behind nginx or a load balancer, or to
	// CF-Connecting-IP behind Cloudflare.
	TrustedProxyHeader string
	// TrustedProxyHops is how many proxies append to X-Forwarded-For, so the
	// client's own entry can be counted from the right. Ignored for the
	// single-value headers, which a proxy overwrites.
	TrustedProxyHops int
}

// Production reports whether this is the production deployment. It gates the
// fail-fast checks that would be unhelpful on a laptop.
func (a App) Production() bool { return a.Env == "production" || a.Env == "prod" }

// DB is the database connection.
type DB struct {
	Host    string
	Port    string
	Name    string
	User    string
	Pass    string
	SSLMode string
	Zone    string
}

// DSN builds the lib/pq connection string.
func (d DB) DSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		d.Host, d.Port, d.User, d.Pass, d.Name, d.SSLMode, d.Zone)
}

// Mail is the SMTP sender.
type Mail struct {
	Host     string
	Port     string
	User     string
	Pass     string
	From     string
	FromName string
}

// Configured reports whether enough is set to attempt a send. Nothing is sent
// when it is not, and the reason is logged once rather than per message.
func (m Mail) Configured() bool { return m.Host != "" && m.From != "" }

// Scheduler is the dispatcher's own settings.
type Scheduler struct {
	// Enabled turns the dispatcher off entirely. A second instance running
	// only the interface, or a developer's copy pointed at production, must be
	// able to be certain it fires nothing.
	Enabled bool
	// DryRun computes the tick and writes nothing: no queued rows, no
	// dispatch, no heartbeat. For validating a configuration before it goes
	// live.
	DryRun bool
	// MaxConcurrent caps runs in flight across the whole service.
	MaxConcurrent int
	// QueueSize bounds the worker pool's backlog. A full queue is a real
	// backpressure signal and is logged, never silently dropped.
	QueueSize int
	// MissScanMin is how far back a restarted dispatcher will replay. A
	// dispatcher down for a week must not queue a week of work at once.
	MissScanMin int
	// Metrics serves /metrics for Prometheus.
	//
	// The endpoint is unauthenticated, as a scrape target normally is, and it
	// discloses project names, job names and run counts. That is fine inside a
	// cluster and not fine on a public ingress, so it can be turned off.
	Metrics bool
}

// Watchdog is the sweep's thresholds.
type Watchdog struct {
	// Every is the sweep interval.
	Every time.Duration
	// HeartbeatLimitMin: a pulse older than this means the dispatcher is down.
	HeartbeatLimitMin int
	// DriftLimitSec is the tolerated gap between the app and database clocks.
	DriftLimitSec int
	// RunRetentionDays is how long run rows are kept. Zero disables pruning.
	RunRetentionDays int
	// LogRetentionDays is how long application log rows are kept.
	LogRetentionDays int
	// FailureThreshold is how many failures in an hour raise an alert.
	FailureThreshold int
	// AlertTo receives watchdog alerts. Empty means no alert mail is sent.
	AlertTo []string
	// AlertRepeat is the shortest gap between two mails about the same set of
	// warnings.
	AlertRepeat time.Duration
}

// Outbound is the HTTP client used to call job targets.
type Outbound struct {
	// UserAgent identifies this service to the targets it calls.
	UserAgent string
	// MaxBodyBytes bounds how much of a response is read into memory. A job
	// answering with a large page must not be able to grow the process.
	MaxBodyBytes int64
	// AllowPrivateTargets permits addresses on private and loopback ranges.
	//
	// It defaults to ON, and that is the correct default here rather than an
	// oversight: the point of this service is calling internal endpoints that
	// have no public address. Set it false for a deployment whose jobs are all
	// public, which turns the SSRF guard back on.
	AllowPrivateTargets bool
}

// Docker turns on the container panel, and is off unless an address is given.
type Docker struct {
	// API is the base address of a READ-ONLY Docker API proxy, empty by
	// default.
	//
	// Deliberately an address and never the socket. Anything that can reach
	// /var/run/docker.sock can start a privileged container and own the host,
	// so a socket handed to a web service turns one remote flaw into the
	// machine. A proxy that publishes the container list and nothing else costs
	// this process no privilege at all. See internal/dockerinfo.
	//
	// Env-only for the same reason: a field on the settings screen would let
	// an administrator account point the console somewhere new, where this
	// takes access to the host.
	API string
	// Label filters the list through Docker's own filter, e.g.
	// "cronsole.watch=true". Empty lists every container on the host.
	Label string
}

// Configured reports whether the container panel has somewhere to read from.
// The panel is absent when it does not.
func (d Docker) Configured() bool { return d.API != "" }

// Load reads the environment and validates it.
//
// Validation is fatal by design: a service that starts with a missing signing
// secret and discovers it on the first login has already accepted forged
// tokens for however long that took.
func Load() (*Config, error) {
	cfg := &Config{
		App: App{
			Name:    env("APP_NAME", "cronsole"),
			Env:     env("APP_ENV", "development"),
			Port:    env("APP_PORT", env("PORT", "3333")),
			Debug:   envBool("APP_DEBUG", false),
			BaseURL: strings.TrimRight(env("APP_URL", ""), "/"),
			// Off by default. See the field comment: this is the foundation the
			// rate limiters stand on, and the wrong default removes them.
			TrustedProxyHeader: env("TRUSTED_PROXY_HEADER", ""),
			TrustedProxyHops:   envInt("TRUSTED_PROXY_HOPS", 1),
		},
		DB: DB{
			Host:    env("DB_HOST", "localhost"),
			Port:    env("DB_PORT", "5432"),
			Name:    env("DB_NAME", "cron"),
			User:    env("DB_USER", "postgres"),
			Pass:    env("DB_PASS", ""),
			SSLMode: env("DB_SSLMODE", "require"),
			Zone:    env("DB_ZONE", "Europe/Istanbul"),
		},
		Mail: Mail{
			Host:     env("MAIL_HOST", ""),
			Port:     env("MAIL_PORT", "587"),
			User:     env("MAIL_USER", ""),
			Pass:     env("MAIL_PASS", ""),
			From:     env("MAIL_FROM", ""),
			FromName: env("MAIL_FROM_NAME", "Cronsole"),
		},
		Scheduler: Scheduler{
			Enabled:       envBool("SCHEDULER_ENABLED", true),
			DryRun:        envBool("SCHEDULER_DRY_RUN", false),
			MaxConcurrent: envInt("SCHEDULER_MAX_CONCURRENT", 20),
			QueueSize:     envInt("SCHEDULER_QUEUE_SIZE", 512),
			MissScanMin:   envInt("SCHEDULER_MISS_SCAN_MIN", 60),
			Metrics:       envBool("METRICS_ENABLED", true),
		},
		Watchdog: Watchdog{
			Every:             time.Duration(envInt("WATCHDOG_EVERY_MIN", 5)) * time.Minute,
			HeartbeatLimitMin: envInt("WATCHDOG_HEARTBEAT_LIMIT_MIN", 5),
			DriftLimitSec:     envInt("WATCHDOG_DRIFT_LIMIT_SEC", 30),
			RunRetentionDays:  envInt("RUN_RETENTION_DAYS", 90),
			LogRetentionDays:  envInt("LOG_RETENTION_DAYS", 90),
			FailureThreshold:  envInt("WATCHDOG_FAILURE_THRESHOLD", 5),
			AlertTo:           envList("WATCHDOG_ALERT_TO"),
			AlertRepeat:       time.Duration(envInt("WATCHDOG_ALERT_REPEAT_MIN", 60)) * time.Minute,
		},
		Outbound: Outbound{
			UserAgent:           env("OUTBOUND_USER_AGENT", "cronsole/2 (+scheduled trigger)"),
			MaxBodyBytes:        int64(envInt("OUTBOUND_MAX_BODY_KB", 64)) * 1024,
			AllowPrivateTargets: envBool("OUTBOUND_ALLOW_PRIVATE", true),
		},
		Docker: Docker{
			API:   strings.TrimRight(env("DOCKER_API", ""), "/"),
			Label: env("DOCKER_LABEL", ""),
		},
		JWTSecret: os.Getenv("JWT_SECRET"),
	}

	loc, err := time.LoadLocation(cfg.DB.Zone)
	if err != nil {
		return nil, fmt.Errorf("config: unknown time zone %q: %w", cfg.DB.Zone, err)
	}
	cfg.App.Location = loc

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MinSecretLength is the shortest signing secret the service will start with.
// Below this a token is guessable, and a guessable token is every account.
const MinSecretLength = 32

func (c *Config) validate() error {
	if len(c.JWTSecret) < MinSecretLength {
		return fmt.Errorf("config: JWT_SECRET must be at least %d characters", MinSecretLength)
	}
	if c.Scheduler.MaxConcurrent < 1 {
		return fmt.Errorf("config: SCHEDULER_MAX_CONCURRENT must be at least 1")
	}
	if c.Scheduler.QueueSize < c.Scheduler.MaxConcurrent {
		return fmt.Errorf("config: SCHEDULER_QUEUE_SIZE (%d) must be at least SCHEDULER_MAX_CONCURRENT (%d)",
			c.Scheduler.QueueSize, c.Scheduler.MaxConcurrent)
	}
	// Refused rather than ignored, and http only. A "unix:///var/run/..." here
	// would be somebody asking for the socket this feature exists to avoid, and
	// a misspelled address would otherwise show as an empty panel, which reads
	// as a host with no containers on it.
	if api := c.Docker.API; api != "" &&
		!strings.HasPrefix(api, "http://") && !strings.HasPrefix(api, "https://") {
		return fmt.Errorf("config: DOCKER_API must be the http:// or https:// address of a "+
			"read-only Docker API proxy, not %q", api)
	}

	if c.App.Production() {
		// A production deployment reached over anything but loopback with
		// sslmode=disable sends its password in clear text.
		if c.DB.SSLMode == "disable" && !isLoopback(c.DB.Host) {
			return fmt.Errorf("config: DB_SSLMODE=disable is refused in production for host %q", c.DB.Host)
		}
		// The watchdog is the only thing that notices the dispatcher dying.
		// Without a recipient it notices in silence.
		if len(c.Watchdog.AlertTo) == 0 {
			return fmt.Errorf("config: WATCHDOG_ALERT_TO is required in production")
		}
		if !c.Mail.Configured() {
			return fmt.Errorf("config: MAIL_HOST and MAIL_FROM are required in production")
		}
	}
	return nil
}

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return true
	}
	return false
}

// Presence describes whether a secret is set without revealing it. Boot
// diagnostics use it; printing the value would put it in the log retention of
// every environment the service runs in.
func Presence(v string) string {
	if v == "" {
		return "unset"
	}
	return fmt.Sprintf("set(len=%d)", len(v))
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
