// Command cronsole is the central scheduled job service.
//
// This file is the composition root: the ONLY place concrete types are wired
// to interfaces. Everything below it depends on an interface and is given its
// dependency, which is what makes the services testable and what keeps a
// repository from being reached out to from the middle of a business rule.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/mstgnz/grantz/sqlstore"
	"github.com/robfig/cron/v3"

	"github.com/mstgnz/cronsole/v2/internal/applog"
	"github.com/mstgnz/cronsole/v2/internal/authz"
	"github.com/mstgnz/cronsole/v2/internal/config"
	"github.com/mstgnz/cronsole/v2/internal/domain"
	"github.com/mstgnz/cronsole/v2/internal/handler"
	"github.com/mstgnz/cronsole/v2/internal/hostinfo"
	"github.com/mstgnz/cronsole/v2/internal/mailer"
	"github.com/mstgnz/cronsole/v2/internal/middleware"
	"github.com/mstgnz/cronsole/v2/internal/repository"
	"github.com/mstgnz/cronsole/v2/internal/router"
	"github.com/mstgnz/cronsole/v2/internal/service"
	"github.com/mstgnz/cronsole/v2/pkg/auth"
	"github.com/mstgnz/cronsole/v2/pkg/metrics"
	"github.com/mstgnz/cronsole/v2/pkg/token"
	"github.com/mstgnz/cronsole/v2/pkg/worker"
)

// version identifies the build. Set with
// -ldflags "-X main.version=$(git rev-parse --short HEAD)".
var version = "dev"

// shutdownTimeout bounds how long the process waits for work in flight.
//
// Generous on purpose: a run cut off mid-flight leaves a row in running that
// nothing closes until the watchdog sweeps, and for a job marked single run
// that means it does not execute again until then.
const shutdownTimeout = 90 * time.Second

func main() {
	// ENV_FILE names the file to read, so a local run can point at its own
	// settings without editing, or risking, the deployment's .env.
	//
	// A missing file is not fatal: in a container every value comes from the
	// environment and there is no file at all. Values already set in the
	// environment win, which is what makes an override work.
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	if err := godotenv.Load(envFile); err != nil {
		slog.Info("no environment file read, using the environment as it is", "file", envFile)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	logger := applog.New()

	db, err := openDatabase(cfg)
	if err != nil {
		log.Fatalf("startup: database: %v", err)
	}
	defer func() { _ = db.Close() }()
	logger.Attach(db)

	slog.Info("starting",
		"version", version,
		"env", cfg.App.Env,
		"port", cfg.App.Port,
		"zone", cfg.DB.Zone,
		"scheduler", cfg.Scheduler.Enabled,
		"dry_run", cfg.Scheduler.DryRun,
		// Presence, never the value. A secret printed once is disclosed
		// permanently: it lives in log retention, in a terminal buffer, and in
		// whatever recorded the screen.
		"jwt_secret", config.Presence(cfg.JWTSecret),
		"mail", cfg.Mail.Configured(),
	)

	application, err := build(cfg, db, logger)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application.start(ctx)

	<-ctx.Done()
	slog.Info("shutting down")
	application.shutdown()
	slog.Info("stopped")
}

// app is the built application: everything the process needs to run, and
// everything it has to stop.
//
// It exists so the wiring can be tested. The graph below is the one place
// concrete types meet interfaces, and a mistake in it — a runner built without
// its host resolver, a health handler without the pool — produces a service
// that starts cleanly and is quietly wrong. Every other test builds its own
// graph and therefore cannot see that.
type app struct {
	cfg    *config.Config
	logger *applog.Logger

	server    *http.Server
	scheduler *cron.Cron
	pool      *worker.Pool
	runner    *service.Runner
	notifier  *service.Notifier

	// host samples the machine; hostOverrides keeps the dialer's routes
	// current. Both run on timers started by start.
	host          *hostinfo.Reader
	hostOverrides *service.HostOverrideService

	instanceID string
}

// build wires the whole application over an open database.
//
// It returns an error rather than exiting, so a caller can say which part of
// the wiring failed and a test can assert on it. main turns that into the
// fatal it used to be.
func build(cfg *config.Config, db *sql.DB, logger *applog.Logger) (*app, error) {
	instanceID := instanceName()

	// --- repositories ---

	store := repository.New(db)
	userRepo := repository.NewUserRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	jobRepo := repository.NewJobRepo(store)
	runRepo := repository.NewRunRepo(store)
	execRepo := repository.NewExecRepo(store)
	notifyRepo := repository.NewNotificationRepo(store)
	logRepo := repository.NewAppLogRepo(store)
	statsRepo := repository.NewStatsRepo(store)
	authzRepo := repository.NewAuthzRepo(store)
	hostRepo := repository.NewHostOverrideRepo(store)

	// --- authorization ---

	// grantz owns the grant tables and reads them; this service decides what a
	// scope means. Built before everything else because account management and
	// every handler depend on it.
	authzService, err := authz.New(authz.Config{
		Store:  authzRepo,
		Grants: sqlstore.New(db),
		// Short, because the cache is per process: on more than one replica this
		// is what bounds how long a revoked grant keeps working elsewhere.
		CacheTTL: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("authorization: %w", err)
	}
	// The catalogue and the built-in roles are written from code at every boot,
	// so upgrading the binary upgrades what a role can do. Orphans are reported
	// and never deleted: a rollback would otherwise cascade away an
	// administrator's own role mappings.
	if orphans, err := authzService.Sync(context.Background()); err != nil {
		return nil, fmt.Errorf("authorization catalogue: %w", err)
	} else if len(orphans) > 0 {
		slog.Warn("permissions in the database that this build does not define",
			"keys", strings.Join(orphans, ", "))
	}

	// --- services ---

	policy := service.TargetPolicy{AllowPrivate: cfg.Outbound.AllowPrivateTargets}
	issuer := token.NewIssuer(cfg.JWTSecret)

	sender := mailer.New(cfg.Mail)
	notifier := service.NewNotifier(sender, logger, cfg.App.BaseURL)

	authService := service.NewAuthService(userRepo, issuer, authzRepo, logger)
	memberService := service.NewMemberService(authzService, userRepo)
	notificationService := service.NewNotificationService(notifyRepo)
	projectService := service.NewProjectService(projectRepo, jobRepo, policy)
	jobService := service.NewJobService(jobRepo, projectRepo, runRepo, policy, cfg.App.Location)
	syncService := service.NewSyncService(jobRepo, projectRepo, jobService)
	statsService := service.NewStatsService(statsRepo, jobRepo, jobService)

	// Where a hostname is dialled. Built before the runner, which reads its
	// table on every connection, and loaded before anything can fire so the
	// first tick already routes correctly.
	hostOverrides := service.NewHostOverrideService(hostRepo, policy, logger)
	hostOverrides.Reload(context.Background())

	// A fresh deployment has no account and no way to make one from the
	// environment: the first administrator is created on the setup screen, by
	// whoever opens it. Said out loud at boot, because the window between
	// starting and setting up is the one time this address is worth guarding.
	if needed, err := authService.NeedsSetup(context.Background()); err != nil {
		return nil, fmt.Errorf("the account count could not be read: %w", err)
	} else if needed {
		slog.Warn("no account exists yet; the first person to open " +
			middleware.SetupPath + " becomes the administrator")
	}

	// --- execution ---

	// The pool is built before the runner because the runner hands chain steps
	// back to it, and the dispatcher hands everything to it. One indirection
	// through dispatch breaks the cycle: nobody holds a reference to the thing
	// that will eventually run their work.
	var pool *worker.Pool
	var runner *service.Runner

	dispatch := func(runID int64, code string) {
		if pool == nil {
			return
		}
		ok := pool.Submit(func(ctx context.Context) {
			if runner == nil {
				return
			}
			runner.Run(ctx, runID)
		})
		if !ok {
			// A full queue is real backpressure and the run is genuinely not
			// starting. It stays pending in the database, so the next tick
			// picks it up; saying so is what keeps that from looking like a
			// job that silently skipped.
			logger.Warn("scheduler: work queue full, run left pending",
				fmt.Sprintf("run %d (%s) was not started", runID, code))
		}
	}

	// Metrics are read at scrape time for the queue gauges, so they report the
	// pool as it is rather than as it was when something last remembered to
	// record it. The closures are safe before the pool exists: nothing scrapes
	// until the server is listening.
	recorder := metrics.New(
		func() float64 {
			if pool == nil {
				return 0
			}
			return float64(pool.Queued())
		},
		func() float64 {
			if pool == nil {
				return 0
			}
			return float64(pool.Capacity())
		},
	)

	pool = worker.New(worker.Config{
		Workers:   cfg.Scheduler.MaxConcurrent,
		QueueSize: cfg.Scheduler.QueueSize,
		// A task's ceiling is well above any single job's timeout, because one
		// task covers every retry plus the closing writes.
		TaskTimeout: time.Duration(domain.MaxTimeoutSec*8) * time.Second,
		OnPanic: func(value any, stack string) {
			logger.Error("worker: task panicked", fmt.Sprintf("%v\n%s", value, stack))
		},
	})

	runner = service.NewRunner(execRepo, execRepo, logger, service.RunnerConfig{
		Policy:       policy,
		UserAgent:    cfg.Outbound.UserAgent,
		MaxBodyBytes: cfg.Outbound.MaxBodyBytes,
		InstanceID:   instanceID,
		Dispatch:     dispatch,
		Notifier:     notifier,
		Observer:     recorder,
		Routes:       hostOverrides.Resolver(),
	})

	runService := service.NewRunService(runRepo, jobRepo, dispatch)

	dispatcher := service.NewDispatcher(execRepo, dispatch, service.DispatcherConfig{
		MissScanMin:   cfg.Scheduler.MissScanMin,
		MaxConcurrent: cfg.Scheduler.MaxConcurrent,
		DryRun:        cfg.Scheduler.DryRun,
		InstanceID:    instanceID,
		Location:      cfg.App.Location,
	})

	watchdog := service.NewWatchdog(execRepo, notifier, logger, service.WatchdogConfig{
		HeartbeatLimitMin: cfg.Watchdog.HeartbeatLimitMin,
		DriftLimitSec:     cfg.Watchdog.DriftLimitSec,
		RunRetentionDays:  cfg.Watchdog.RunRetentionDays,
		LogRetentionDays:  cfg.Watchdog.LogRetentionDays,
		FailureThreshold:  cfg.Watchdog.FailureThreshold,
		AlertTo:           cfg.Watchdog.AlertTo,
		AlertRepeat:       cfg.Watchdog.AlertRepeat,
	})

	// Built here and started in start, so building the graph has no side
	// effects: a test can assemble the application without a dispatcher firing
	// at it.
	scheduler := newScheduler(cfg, logger, recorder, dispatcher, watchdog)

	// --- HTTP ---

	renderer, err := handler.NewRenderer(cfg.App.Location, logger, version)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}

	// The cookie is marked Secure everywhere except a plain HTTP development
	// server, where the browser would discard it and sign in would appear to
	// do nothing.
	secureCookie := cfg.App.Production() || strings.HasPrefix(cfg.App.BaseURL, "https://")
	mw := middleware.New(authService, projectService, authzService, logger, secureCookie)

	// Which address the rate limiters count against. Fatal rather than a
	// warning: a misspelled header silently trusts nothing, which looks exactly
	// like a working deployment until somebody reads the buckets.
	trustedProxy, err := auth.NewTrustedProxy(cfg.App.TrustedProxyHeader, cfg.App.TrustedProxyHops)
	if err != nil {
		return nil, err
	}
	if cfg.App.TrustedProxyHeader == "" {
		slog.Info("client address taken from the socket; set TRUSTED_PROXY_HEADER when behind a proxy")
	} else {
		slog.Info("client address taken from a trusted header",
			"header", cfg.App.TrustedProxyHeader, "hops", cfg.App.TrustedProxyHops)
	}

	// The machine this is installed on. Cronsole runs on somebody's own VM, so
	// the console is frequently the only thing watching that box; the disk it
	// reports is the working directory's, which is where this process writes.
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	host := hostinfo.NewReader(workDir)

	handlers := router.Handlers{
		Setup: handler.NewSetupHandler(authService, mw, renderer, logger),
		Auth: handler.NewAuthHandler(authService, renderer, mw,
			auth.NewLimiter(10, 5*time.Minute), trustedProxy, logger),
		Lang:      handler.NewLangHandler(secureCookie),
		Docs:      handler.NewDocsHandler(),
		Dashboard: handler.NewDashboardHandler(authzService, statsService, host, renderer, logger),
		Jobs: handler.NewJobHandler(authzService, jobService, projectService, runService,
			notificationService, statsService, renderer, logger),
		Runs: handler.NewRunHandler(authzService, runService, projectService, renderer,
			cfg.App.Location, logger),
		Projects: handler.NewProjectHandler(authzService, projectService, memberService,
			renderer, logger, cfg.App.BaseURL),
		Settings: handler.NewSettingsHandler(authService, notificationService, memberService, hostOverrides,
			logRepo, renderer, logger),
		API: handler.NewAPIHandler(authzService, jobService, syncService, runService, projectService,
			statsService, cfg.App.Location, logger),
		Health: handler.NewHealthHandler(db, pool, version),
	}

	routes := router.New(handlers, mw, trustedProxy, logger)
	// The metrics endpoint is mounted here rather than in the router because
	// the recorder belongs to the composition root, and the router should not
	// have to know that this deployment scrapes at all.
	mux := http.NewServeMux()
	if cfg.Scheduler.Metrics {
		mux.Handle("/metrics", recorder.Handler())
	}
	mux.Handle("/", routes)

	srv := &http.Server{
		Addr:    ":" + cfg.App.Port,
		Handler: mux,
		// ReadHeaderTimeout is the one that is easy to leave out and the one
		// that matters most: without it a client can send headers a byte at a
		// time and hold a connection open indefinitely.
		ReadTimeout:       20 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	return &app{
		cfg:           cfg,
		logger:        logger,
		server:        srv,
		scheduler:     scheduler,
		pool:          pool,
		runner:        runner,
		notifier:      notifier,
		host:          host,
		hostOverrides: hostOverrides,
		instanceID:    instanceID,
	}, nil
}

// start sets everything running. The timers stop when ctx is cancelled; the
// server is closed by shutdown.
func (a *app) start(ctx context.Context) {
	if a.cfg.Scheduler.Enabled {
		a.scheduler.Start()
		slog.Info("dispatcher started", "spec", domain.DispatcherSpec, "instance", a.instanceID)
	} else {
		slog.Warn("dispatcher is disabled; this instance serves the interface only")
	}

	// CPU usage is a delta between two readings. Sampling on a timer means the
	// figure is current whenever somebody opens the dashboard, rather than
	// blank on the first look and a thirty second average on the second.
	a.host.Start(ctx, 5*time.Second)

	// A route saved here is applied immediately by the handler. This is how one
	// saved on ANOTHER replica reaches this process, which never saw the
	// request, so the interval is the bound on how long a route can be stale
	// across the deployment.
	a.hostOverrides.Watch(ctx, 30*time.Second)

	go func() {
		slog.Info("listening", "addr", a.server.Addr)
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.logger.Error("http: server stopped", err.Error())
			log.Fatal(err)
		}
	}()
}

// shutdown stops everything, in the reverse of the order it started.
//
//  1. Stop the scheduler, so no new work is queued.
//  2. Stop the chain timers, so no delayed hand-off fires mid shutdown.
//  3. Stop accepting requests.
//  4. Drain the pool, so runs already started finish and write results.
//  5. Flush the notifier and the log writer, which is where the record of all
//     of the above ends up.
//
// Draining before closing the server would let a new request queue work into a
// pool that is already draining.
func (a *app) shutdown() {
	a.scheduler.Stop()
	if stopped := a.runner.Close(); stopped > 0 {
		slog.Info("stopped pending chain timers; the dispatcher will take those runs", "count", stopped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := a.server.Shutdown(ctx); err != nil {
		slog.Warn("http: shutdown was not clean", "err", err)
	}
	if drained := a.pool.Shutdown(ctx); !drained {
		slog.Warn("work queue did not drain in time; runs left in flight will be closed by the watchdog")
	}

	a.notifier.Close(ctx)
	a.logger.Close(ctx)
}

// openDatabase connects and verifies the connection.
func openDatabase(cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("postgres", cfg.DB.DSN())
	if err != nil {
		return nil, err
	}

	// Without these the pool is unbounded: a slow query storm opens
	// connections until the server refuses them, and none are ever recycled.
	// The ceiling is above the concurrency cap because the interface and the
	// dispatcher need connections of their own while jobs are running.
	db.SetMaxOpenConns(cfg.Scheduler.MaxConcurrent + 15)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	return db, nil
}

// newScheduler registers the service's own two entries.
//
// These are the scheduler's own work, not rows in the jobs table: the
// dispatcher tick and the watchdog sweep. Neither could be a job, because a
// job is an HTTP call to somewhere else.
func newScheduler(cfg *config.Config, logger *applog.Logger, recorder *metrics.Recorder,
	dispatcher *service.Dispatcher, watchdog *service.Watchdog) *cron.Cron {

	// The seconds field is optional so the five field expressions elsewhere
	// keep working, while the dispatcher can name a second. It has to: see
	// domain.DispatcherSpec for why firing on the minute boundary loses
	// minutes.
	parser := cron.NewParser(
		cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)

	c := cron.New(
		cron.WithLocation(cfg.App.Location),
		cron.WithParser(parser),
		cron.WithChain(
			cron.Recover(cron.DefaultLogger),
			// A tick that outruns its minute must not be joined by the next
			// one. The dispatcher guards this itself as well; both are cheap
			// and the failure they prevent is a doubled concurrency budget.
			cron.SkipIfStillRunning(cron.DefaultLogger),
		),
	)

	if _, err := c.AddFunc(domain.DispatcherSpec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
		defer cancel()

		result, err := dispatcher.Tick(ctx)
		if err != nil {
			recorder.TickFinished(metrics.TickObservation{Failed: true})
			logger.Error("dispatcher: tick failed", err.Error())
			return
		}
		recordTick(recorder, result)
		reportTick(logger, result)
	}); err != nil {
		log.Fatalf("startup: the dispatcher could not be scheduled: %v", err)
	}

	every := cfg.Watchdog.Every
	if every < time.Minute {
		every = 5 * time.Minute
	}
	if _, err := c.AddFunc(fmt.Sprintf("@every %s", every), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		result, err := watchdog.Sweep(ctx)
		if err != nil {
			logger.Error("watchdog: sweep failed", err.Error())
			return
		}
		if len(result.Warnings) > 0 {
			slog.Warn("watchdog", "warnings", len(result.Warnings),
				"stuck_closed", result.StuckClosed, "mail_sent", result.MailSent,
				"mail_skipped", result.MailSkipReason)
		}
	}); err != nil {
		log.Fatalf("startup: the watchdog could not be scheduled: %v", err)
	}

	return c
}

// reportTick logs what a tick did, and raises the parts that are faults.
// recordTick turns a tick into metrics. Separate from reportTick because one
// answers "what should page somebody" and the other "what should be graphed",
// and merging them makes both harder to read.
func recordTick(recorder *metrics.Recorder, result service.TickResult) {
	if result.Skipped {
		return
	}
	lost := result.MinuteGap
	if result.MinuteRepeat {
		// A repeated minute means the wall minute between the two ticks was
		// never processed, so it is exactly one lost minute.
		lost++
	}
	recorder.TickFinished(metrics.TickObservation{
		At:              float64(time.Now().Unix()),
		DurationSeconds: result.Duration.Seconds(),
		DriftSeconds:    float64(result.DriftSec),
		MinutesLost:     lost,
		Queued:          result.QueuedRuns,
		Dispatched:      result.Dispatched,
		Skipped:         result.SkippedRuns,
		QuotaFull:       result.QuotaFull,
		ActiveJobs:      result.ActiveJobs,
	})
}

func reportTick(logger *applog.Logger, result service.TickResult) {
	if result.Skipped {
		logger.Warn("dispatcher: tick skipped", "the previous tick was still running")
		return
	}

	// A repeated or skipped minute means minutes went unprocessed, and nothing
	// else in the system notices: a repeated minute conflicts on the unique
	// index without raising an error, and a skipped one leaves no trace at all.
	if result.MinuteRepeat {
		logger.Error("dispatcher: the same minute was processed twice",
			fmt.Sprintf("minute %s; the wall minute between the two ticks was never processed",
				result.Minute.Format(time.RFC3339)))
	}
	if result.MinuteGap > 0 {
		// Say what was recovered, not just what was missed. The replay runs
		// after this audit and covers those minutes for jobs with run_missed,
		// so reporting the gap alone reads as total loss when it often is not.
		recovered := result.MissedMinutes
		if recovered > result.MinuteGap {
			recovered = result.MinuteGap
		}
		logger.Error("dispatcher: minutes were skipped",
			fmt.Sprintf("%d minute(s) passed between ticks before %s. %d of them were replayed for jobs with run_missed; "+
				"the rest did not run. Usual causes: the process restarted, the host slept, or a tick outran its minute.",
				result.MinuteGap, result.Minute.Format(time.RFC3339), recovered))
	}
	for _, message := range result.ParseErrors {
		logger.Warn("dispatcher: invalid schedule expression", message)
	}
	for _, message := range result.WriteErrors {
		logger.Error("dispatcher: run could not be queued", message)
	}
	if result.QuotaFull {
		logger.Warn("dispatcher: concurrency limit reached",
			fmt.Sprintf("%d run(s) in flight; queued work waits for the next tick", result.Running))
	}

	if result.QueuedRuns > 0 || result.Dispatched > 0 || result.SkippedRuns > 0 {
		slog.Info("dispatcher tick",
			"minute", result.Minute.Format("15:04"),
			"queued", result.QueuedRuns,
			"dispatched", result.Dispatched,
			"skipped", result.SkippedRuns,
			"running", result.Running,
			"drift_sec", result.DriftSec,
			"took", result.Duration.Round(time.Millisecond),
		)
	}
}

// instanceName identifies this process in the heartbeat and on every run it
// claims.
//
// It has to be unique per process: two replicas sharing a name make a stuck
// run untraceable to the machine holding it. The hostname is included because
// that is what names a pod; the random suffix keeps it unique when a pod
// restarts under the same name.
func instanceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), auth.RandomHex(3))
}
