# Operations

Running this thing, watching it, and working out what went wrong.

## The first run

A deployment whose `users` table is empty serves exactly one screen. Every path
redirects to `/setup`, which creates the first administrator, and the moment
that account exists the screen answers 404 for the life of the deployment.

**Treat the window between starting the process and completing that form as the
one moment the service is open.** Whoever submits the form becomes the platform
administrator, so complete it before the address is reachable by anyone else:
on a cluster that usually means port-forwarding to a pod rather than waiting for
the ingress.

There is no environment variable for it, which is deliberate. An administrator
password set through the environment lives on in a `.env` file, a shell history
and a container spec, and stays valid long after everybody has forgotten it is
there. Boot logs a warning while the deployment is still unclaimed.

Three things are outside the gate, because a redirect would be the wrong answer
for them: `/healthz`, `/readyz` and `/static/*`. An orchestrator has to be able
to ask whether the process is alive, and the setup screen needs its stylesheet.

Two properties make the screen safe to leave in the binary. The gate reads
"does an account exist" from the database until the answer is yes and then
latches, so it can never reopen; and the account is created by a single
conditional `INSERT`, so two people submitting the form at the same instant
produce one administrator, not two.

## Configuration

Every setting is read once at start up by `internal/config`; nothing else calls
`os.Getenv`. All of them are documented in [`.env.example`](../.env.example).

`ENV_FILE` names the file to read, so a local run can point at its own settings
without touching a `.env` that points at something real:

```bash
ENV_FILE=.env.local make live
```

Values already present in the environment win over the file, which is what
makes a container override work.

### Boot is refused when

| | why |
|---|---|
| `JWT_SECRET` shorter than 32 characters | a guessable signing key is every account. Discovering this on the first login means forged tokens were accepted for however long that took |
| `SCHEDULER_QUEUE_SIZE` below `SCHEDULER_MAX_CONCURRENT` | the queue would refuse work the pool could have started immediately |
| production with `DB_SSLMODE=disable` on a non-loopback host | the password crosses the network in clear text |
| production without `WATCHDOG_ALERT_TO` | the watchdog is the only thing that notices the scheduler dying, and with no recipient it notices in silence |
| production without `MAIL_HOST` and `MAIL_FROM` | same reason: no transport, no alert |

Failing at boot is the point. Each of these is a control whose absence is
invisible while everything is fine.

### Settings worth understanding

**`SCHEDULER_ENABLED=false`** serves the interface and runs nothing. Use it for
a second instance, or a developer copy pointed at production data, where you
need to be certain it fires nothing.

**`SCHEDULER_DRY_RUN=true`** computes each tick and writes nothing: no queued
rows, no execution, and **no heartbeat**. The heartbeat is deliberately left
alone, because refreshing it without doing the work would make a real
dispatcher coming back later skip its catch-up scan and lose those minutes.

**`SCHEDULER_MAX_CONCURRENT`** is per replica. Three replicas at 20 is 60 runs
in flight against your services.

**`OUTBOUND_ALLOW_PRIVATE`** defaults on, because calling internal endpoints
with no public address is the point. Set it false for a deployment whose jobs
are all public and the SSRF guard comes back.

**`TRUSTED_PROXY_HEADER`** is empty by default and should stay empty unless
there really is a proxy in front. It decides which address the rate limiters
count against. A forwarding header is an ordinary request header, so trusting
one when nothing sets it lets a caller send a different value per request, get a
fresh bucket each time, and turn the login form into an unlimited password
oracle. Set it to `X-Forwarded-For` behind nginx or a load balancer, together
with `TRUSTED_PROXY_HOPS` (1 for a single proxy), or to `CF-Connecting-IP`
behind Cloudflare. A header the service will not vouch for is refused at boot
rather than ignored, because a configuration that silently does nothing looks
exactly like one that works.

## Sending a host name somewhere else

Settings has a **Network** tab, administrator only, that maps a host name to an
address on your own network.

The problem it solves: a job calls `https://shop.example.com/cron/x`, public DNS
points that name at a CDN, and the request leaves the machine and comes back
through it to reach a service one hop away. A route sends the connection
straight to `10.10.0.5` while the `Host` header and the TLS server name stay
`shop.example.com`, so the certificate still validates and the origin still
routes by name. It is exactly what `curl --resolve` does, and the job definition
does not change.

Preferred over `/etc/hosts` because it needs no root and no shell, behaves the
same in a container and under Kubernetes without `hostAliases`, and is visible,
auditable and in the backup.

Three things to know:

- A route decides where credentials are sent, so it is the platform
  administrator's screen alone and is never delegated to a project role. Every
  change is written to the application log.
- The address must be an IP literal. A name there would mean a second DNS
  lookup, which is the round trip this exists to avoid.
- With `OUTBOUND_ALLOW_PRIVATE=false`, a route to a private address is refused:
  it would walk straight past the setting that says this deployment's jobs are
  all public.

A route saved on one replica is applied there immediately and reaches the others
within thirty seconds.

## Multiple replicas

Safe, with no coordination. Exclusivity comes from the unique index on
`(job_id, planned_minute)` and the atomic claim on `job_runs`. Every replica
runs its own dispatcher; all of them must point at the same database.

Shutdown drains for up to 90 seconds. Give Kubernetes more than that
(`terminationGracePeriodSeconds: 120`) or a rolling update abandons runs that
were already started, and they sit in `running` until the watchdog closes them.

**Permissions are cached per process for 30 seconds** and there is no
cross-instance invalidation. Revoking somebody's access takes effect
immediately on the instance that did it and within the window on the others.
If you need a revocation to be instant everywhere, deactivate the account
instead: that is checked against the database on every request.

## Metrics

`/metrics`, when `METRICS_ENABLED` is on. Unauthenticated, as a scrape target
normally is, and it names projects and jobs, so do not expose it through a
public ingress.

Every metric is written from a real call site. The ones worth alerting on:

| metric | alert when |
|---|---|
| `cronsole_dispatcher_minutes_lost_total` | **increases at all.** A minute was never processed. Nothing else reports this: a repeated minute conflicts on the unique index without an error, and a gap writes nothing |
| `cronsole_dispatcher_last_tick_timestamp_seconds` | more than a few minutes old. The dispatcher is not queueing anything |
| `cronsole_clock_drift_seconds` | past about 30. The application and database clocks are separating, and the minute boundary is what suffers |
| `cronsole_last_success_timestamp_seconds` | too old for a given job. This is the one that catches a job failing quietly, which `cronsole_runs_total` alone cannot |
| `cronsole_queue_depth` approaching `cronsole_queue_capacity` | work is about to be refused |
| `cronsole_quota_exceeded_total` | rising. The concurrency cap is the bottleneck, not your targets |
| `cronsole_dispatcher_tick_duration_seconds` | approaching 60. Ticks are about to overlap |

A worked example, jobs that have not succeeded in over a day:

```promql
time() - cronsole_last_success_timestamp_seconds > 86400
```

## Reading a failure

The runs screen is the record. Each row carries the address actually called,
which replica claimed it, the status, the duration, the response and the error.

**`failed`** means the target answered outside the job's success range, or the
request could not be made at all. The error says which.

**`timeout`** means the client gave up. The target may well still be working,
so nothing chained follows it and it is not retried. Repeated timeouts on one
job usually mean `timeout_sec` is short for the service being called, and the
job list shows that count separately for exactly this reason: a timeout and a
failure need opposite fixes, and merged into one red number you cannot tell
which is in front of you.

**`skipped`** means it did not run. Either the previous run was still going and
the job is `single_run`, or a chain step was blocked. The reason is on the row.

**`pending` for a long time** means nothing collected it. Check the dispatcher
heartbeat on the dashboard; the watchdog reports this too.

### Three things that look like scheduler bugs and are not

1. **An expression that does not mean what was intended.** `0 16 * * 7` is
   valid and fires on Sunday. The job form shows the next firings under every
   expression precisely so this is caught before saving rather than six days
   later.
2. **A job overlapping itself.** With `single_run` on, the second run is
   skipped, which reads as "it did not run". With it off, two copies write at
   once, which is worse.
3. **A success range that does not match the target.** A service answering 302
   is a failure at the default 200-399? No: it is a success. A service
   answering 202 with `success_max: 200` is a failure. Check the range on the
   job before blaming the target.

## Retention

`RUN_RETENTION_DAYS` and `LOG_RETENTION_DAYS`, both 90 by default, both pruned
by the watchdog. Deletion is capped at 5000 run rows per sweep so it never
holds a long lock on the busiest table; a backlog drains over a few passes.

Zero disables pruning. Both tables grow with every execution and nothing else
deletes from them, so zero means forever.

## Backup and restore

Everything is in Postgres. `job_runs` is the largest table by far and the least
valuable per row; if a backup window is tight, that is the one to sample rather
than the definitions.

After a restore, check the heartbeat: a stale row makes the first dispatcher
tick treat the gap as missed minutes. It is bounded by
`SCHEDULER_MISS_SCAN_MIN` and only replays jobs with `run_missed`, so the blast
radius is small, but it is worth knowing before it happens.

## Upgrading the schema

`cronsole.sql` is authoritative and hand written. There are no migration files.
A change goes into that file, and anything an existing installation has to run
is handed over as an explicit script.

A fresh installation imports `cronsole.sql` and nothing else.

### The permission catalogue and the built-in roles

Both are written from code at every boot, so upgrading the binary upgrades what
a role can do. Nothing to run by hand, and two consequences worth knowing:

- **A permission this build does not define is reported, never deleted.** Boot
  logs `permissions in the database that this build does not define` and carries
  on. Rolling back to an older binary would otherwise cascade away role mappings
  an administrator configured.
- **A role somebody created by hand is never touched.** Only the three built-in
  keys are reconciled, including one that happens to hold the same permissions.

Boot fails if the catalogue cannot be written. That is deliberate: a service
running with a stale catalogue answers permission questions with yesterday's
list, and the symptom is somebody quietly having access they should not.

## Local development

```bash
make dev-db       # throwaway Postgres on :55433
make dev-schema
ENV_FILE=.env.local make live
make dev-stop
```

`make live` rebuilds on change. It has to: templates and static files are
embedded in the binary, so editing one needs a rebuild rather than a refresh.
