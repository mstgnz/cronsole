package config

import (
	"strings"
	"testing"
)

// validEnv is the smallest environment that boots, so each test can change one
// thing and see only that thing fail.
func validEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	// Every key this package reads, cleared first. Without this a value left in
	// the developer's own environment decides the result of the test.
	for _, key := range []string{
		"APP_NAME", "APP_ENV", "APP_PORT", "PORT", "APP_DEBUG", "APP_URL",
		"TRUSTED_PROXY_HEADER", "TRUSTED_PROXY_HOPS",
		"DB_HOST", "DB_PORT", "DB_NAME", "DB_USER", "DB_PASS", "DB_SSLMODE", "DB_ZONE",
		"MAIL_HOST", "MAIL_PORT", "MAIL_USER", "MAIL_PASS", "MAIL_FROM", "MAIL_FROM_NAME",
		"SCHEDULER_ENABLED", "SCHEDULER_DRY_RUN", "SCHEDULER_MAX_CONCURRENT",
		"SCHEDULER_QUEUE_SIZE", "SCHEDULER_MISS_SCAN_MIN", "METRICS_ENABLED",
		"WATCHDOG_EVERY_MIN", "WATCHDOG_HEARTBEAT_LIMIT_MIN", "WATCHDOG_DRIFT_LIMIT_SEC",
		"RUN_RETENTION_DAYS", "LOG_RETENTION_DAYS", "WATCHDOG_FAILURE_THRESHOLD",
		"WATCHDOG_ALERT_TO", "WATCHDOG_ALERT_REPEAT_MIN",
		"OUTBOUND_USER_AGENT", "OUTBOUND_MAX_BODY_KB", "OUTBOUND_ALLOW_PRIVATE",
		"JWT_SECRET", "TRUSTED_PROXY_HEADER", "TRUSTED_PROXY_HOPS",
	} {
		t.Setenv(key, "")
	}

	t.Setenv("JWT_SECRET", strings.Repeat("s", MinSecretLength))
	for key, value := range overrides {
		t.Setenv(key, value)
	}
}

func TestLoadDefaults(t *testing.T) {
	validEnv(t, nil)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}

	if cfg.App.Env != "development" {
		t.Errorf("Env = %q, want development", cfg.App.Env)
	}
	if cfg.App.Port != "3333" {
		t.Errorf("Port = %q", cfg.App.Port)
	}
	if cfg.App.Production() {
		t.Error("a development environment reported itself as production")
	}
	// The reason the service exists is calling endpoints with no public
	// address, so this defaults on rather than off.
	if !cfg.Outbound.AllowPrivateTargets {
		t.Error("OUTBOUND_ALLOW_PRIVATE defaulted off")
	}
	// And this defaults OFF, because trusting a forwarding header that nothing
	// sets removes the rate limiter entirely.
	if cfg.App.TrustedProxyHeader != "" {
		t.Errorf("TRUSTED_PROXY_HEADER defaulted to %q, want empty", cfg.App.TrustedProxyHeader)
	}
	if cfg.App.Location == nil {
		t.Error("no time zone was resolved")
	}
}

func TestProductionIsRecognised(t *testing.T) {
	for _, env := range []string{"production", "prod"} {
		validEnv(t, map[string]string{
			"APP_ENV":           env,
			"WATCHDOG_ALERT_TO": "ops@example.com",
			"MAIL_HOST":         "smtp.example.com",
			"MAIL_FROM":         "cron@example.com",
		})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(%s) = %v", env, err)
		}
		if !cfg.App.Production() {
			t.Errorf("APP_ENV=%s did not report as production", env)
		}
	}
	validEnv(t, map[string]string{"APP_ENV": "staging"})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.App.Production() {
		t.Error("staging reported as production")
	}
}

func TestPortFallsBackToPORT(t *testing.T) {
	// Several hosts inject PORT rather than APP_PORT, and a service that
	// ignores it binds the wrong port and looks dead.
	validEnv(t, map[string]string{"PORT": "8080"})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.App.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.App.Port)
	}

	// APP_PORT wins when both are set.
	validEnv(t, map[string]string{"PORT": "8080", "APP_PORT": "9090"})
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.App.Port != "9090" {
		t.Errorf("Port = %q, want APP_PORT to win", cfg.App.Port)
	}
}

func TestBaseURLLosesItsTrailingSlash(t *testing.T) {
	// It is concatenated into links in alert mail and the sync example. A
	// trailing slash there produces //jobs, which some proxies treat as a
	// different path.
	validEnv(t, map[string]string{"APP_URL": "https://cron.example.com/"})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.App.BaseURL != "https://cron.example.com" {
		t.Errorf("BaseURL = %q", cfg.App.BaseURL)
	}
}

// --- what boot refuses ------------------------------------------------------

func TestASigningSecretShorterThanTheMinimumIsRefused(t *testing.T) {
	// A guessable signing secret is every account, and a service that starts
	// without one discovers the problem after it has been accepting forged
	// tokens for however long that took.
	for _, secret := range []string{"", "short", strings.Repeat("s", MinSecretLength-1)} {
		validEnv(t, map[string]string{"JWT_SECRET": secret})
		if _, err := Load(); err == nil {
			t.Errorf("a %d character JWT_SECRET was accepted", len(secret))
		}
	}

	validEnv(t, map[string]string{"JWT_SECRET": strings.Repeat("s", MinSecretLength)})
	if _, err := Load(); err != nil {
		t.Errorf("a secret of exactly the minimum was refused: %v", err)
	}
}

func TestTheQueueMustBeAtLeastAsLargeAsTheWorkerCount(t *testing.T) {
	// Otherwise the pool cannot hold what its own workers are about to take,
	// and submissions are refused while workers sit idle.
	validEnv(t, map[string]string{
		"SCHEDULER_MAX_CONCURRENT": "20",
		"SCHEDULER_QUEUE_SIZE":     "10",
	})
	err := requireLoadError(t)
	if !strings.Contains(err.Error(), "SCHEDULER_QUEUE_SIZE") {
		t.Errorf("err = %v, want it to name the setting", err)
	}
}

func TestAtLeastOneWorker(t *testing.T) {
	for _, n := range []string{"0", "-1"} {
		validEnv(t, map[string]string{"SCHEDULER_MAX_CONCURRENT": n, "SCHEDULER_QUEUE_SIZE": "512"})
		if _, err := Load(); err == nil {
			t.Errorf("SCHEDULER_MAX_CONCURRENT=%s was accepted", n)
		}
	}
}

func TestAnUnknownTimeZoneIsRefused(t *testing.T) {
	// The dispatcher takes the minute from the database and fires from the
	// application. A zone that does not exist would silently become UTC and
	// move every schedule.
	validEnv(t, map[string]string{"DB_ZONE": "Mars/Olympus_Mons"})
	err := requireLoadError(t)
	if !strings.Contains(err.Error(), "time zone") {
		t.Errorf("err = %v, want it to mention the time zone", err)
	}
}

func TestProductionRefusesWhatWouldFailSilently(t *testing.T) {
	base := map[string]string{
		"APP_ENV":           "production",
		"WATCHDOG_ALERT_TO": "ops@example.com",
		"MAIL_HOST":         "smtp.example.com",
		"MAIL_FROM":         "cron@example.com",
	}

	cases := []struct {
		name    string
		change  map[string]string
		mention string
		why     string
	}{
		{
			name:    "no watchdog recipient",
			change:  map[string]string{"WATCHDOG_ALERT_TO": ""},
			mention: "WATCHDOG_ALERT_TO",
			why:     "the watchdog is the only thing that notices the dispatcher dying, and without a recipient it notices in silence",
		},
		{
			name:    "no mail server",
			change:  map[string]string{"MAIL_HOST": ""},
			mention: "MAIL_HOST",
			why:     "an alert recipient with no way to send is the same as no recipient",
		},
		{
			name:    "no sender address",
			change:  map[string]string{"MAIL_FROM": ""},
			mention: "MAIL_FROM",
			why:     "the same",
		},
		{
			name:    "an unencrypted database connection to a remote host",
			change:  map[string]string{"DB_SSLMODE": "disable", "DB_HOST": "db.internal"},
			mention: "DB_SSLMODE",
			why:     "the password crosses the network in clear text",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range c.change {
				env[k] = v
			}
			validEnv(t, env)

			err := requireLoadError(t)
			if !strings.Contains(err.Error(), c.mention) {
				t.Errorf("err = %v, want it to name %s (%s)", err, c.mention, c.why)
			}
		})
	}
}

func TestDevelopmentIsNotHeldToTheProductionRules(t *testing.T) {
	// The checks above would be unhelpful on a laptop, which is why they are
	// gated rather than universal.
	validEnv(t, map[string]string{"DB_SSLMODE": "disable", "DB_HOST": "db.internal"})
	if _, err := Load(); err != nil {
		t.Errorf("a development configuration was refused: %v", err)
	}
}

func TestSSLModeDisableIsAllowedForLoopbackInProduction(t *testing.T) {
	// A database on the same machine is not crossing a network, and refusing it
	// would push people to a worse workaround.
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "host.docker.internal"} {
		validEnv(t, map[string]string{
			"APP_ENV": "production", "DB_SSLMODE": "disable", "DB_HOST": host,
			"WATCHDOG_ALERT_TO": "ops@example.com",
			"MAIL_HOST":         "smtp.example.com",
			"MAIL_FROM":         "cron@example.com",
		})
		if _, err := Load(); err != nil {
			t.Errorf("sslmode=disable to %s was refused: %v", host, err)
		}
	}
}

// --- shapes and helpers -----------------------------------------------------

func TestDSN(t *testing.T) {
	d := DB{Host: "h", Port: "5432", Name: "cron", User: "u", Pass: "p",
		SSLMode: "require", Zone: "Europe/Istanbul"}
	got := d.DSN()

	for _, want := range []string{
		"host=h", "port=5432", "user=u", "password=p", "dbname=cron",
		"sslmode=require", "TimeZone=Europe/Istanbul",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN is missing %q: %s", want, got)
		}
	}
}

func TestMailConfigured(t *testing.T) {
	// Both are needed to attempt a send. Either one alone would fail per
	// message rather than once at boot.
	cases := []struct {
		mail Mail
		want bool
	}{
		{Mail{Host: "smtp.example.com", From: "a@b.c"}, true},
		{Mail{Host: "smtp.example.com"}, false},
		{Mail{From: "a@b.c"}, false},
		{Mail{}, false},
	}
	for _, c := range cases {
		if got := c.mail.Configured(); got != c.want {
			t.Errorf("Configured(%+v) = %v, want %v", c.mail, got, c.want)
		}
	}
}

func TestPresenceNeverRevealsTheValue(t *testing.T) {
	// It goes in the boot log, which lives in the log retention of every
	// environment the service runs in. A secret printed once is disclosed
	// permanently.
	const secret = "super-secret-signing-key"
	got := Presence(secret)

	if strings.Contains(got, secret) {
		t.Fatalf("Presence leaked the value: %q", got)
	}
	if !strings.Contains(got, "24") {
		t.Errorf("Presence = %q, want it to report the length", got)
	}
	if Presence("") != "unset" {
		t.Errorf("Presence(\"\") = %q, want unset", Presence(""))
	}
}

func TestEnvHelpersFallBackOnRubbish(t *testing.T) {
	// A typo in a numeric setting must not make the service start with zero
	// workers or a zero retention that deletes everything.
	validEnv(t, map[string]string{
		"SCHEDULER_MAX_CONCURRENT": "twenty",
		"SCHEDULER_ENABLED":        "yes please",
		"OUTBOUND_MAX_BODY_KB":     "",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.Scheduler.MaxConcurrent != 20 {
		t.Errorf("MaxConcurrent = %d, want the default", cfg.Scheduler.MaxConcurrent)
	}
	if !cfg.Scheduler.Enabled {
		t.Error("an unparseable boolean did not fall back to the default")
	}
	if cfg.Outbound.MaxBodyBytes != 64*1024 {
		t.Errorf("MaxBodyBytes = %d, want the default", cfg.Outbound.MaxBodyBytes)
	}
}

func TestAlertRecipientsAreSplitAndTrimmed(t *testing.T) {
	validEnv(t, map[string]string{"WATCHDOG_ALERT_TO": " ops@example.com , , oncall@example.com "})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	want := []string{"ops@example.com", "oncall@example.com"}
	if len(cfg.Watchdog.AlertTo) != len(want) {
		t.Fatalf("AlertTo = %v, want %v", cfg.Watchdog.AlertTo, want)
	}
	for i, v := range want {
		if cfg.Watchdog.AlertTo[i] != v {
			t.Errorf("AlertTo[%d] = %q, want %q", i, cfg.Watchdog.AlertTo[i], v)
		}
	}
}

func TestValuesAreTrimmed(t *testing.T) {
	// A trailing space in a .env file is invisible and would otherwise become
	// part of a host name or a port.
	validEnv(t, map[string]string{"DB_HOST": "  db.internal  ", "APP_PORT": " 4444 "})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.DB.Host != "db.internal" {
		t.Errorf("Host = %q", cfg.DB.Host)
	}
	if cfg.App.Port != "4444" {
		t.Errorf("Port = %q", cfg.App.Port)
	}
}

func requireLoadError(t *testing.T) error {
	t.Helper()
	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded, want a refusal")
	}
	return err
}
