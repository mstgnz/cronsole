# Architecture

How the scheduler works, and why it works that way. The decisions worth
explaining are the ones that look wrong until you know what they prevent.

## The shape

```
cmd/cronsole         composition root: the only place concrete meets interface.
                     build() assembles the graph and returns an error; main
                     loads the environment, opens the database, and runs it
internal/domain      types and interfaces, no dependency outside the standard library
internal/authz       what a permission means here, over github.com/mstgnz/grantz
internal/i18n        the interface in English and Turkish; the API is never translated
internal/repository  the only package that speaks SQL
  .../memrepo        the same interfaces in memory, for tests that need the whole stack
internal/service     the rules: dispatcher, runner, watchdog, and the CRUD services
internal/handler     HTTP in, HTTP out; holds no rule
internal/middleware  who is calling, what they may reach, what gets recorded
internal/router      the whole route table, in one file
pkg/                 self-contained: cronexpr, token, worker, mail, metrics, auth
```

`handler -> service -> domain`, with `repository` implementing the domain
interfaces. Rules live in the service layer rather than in handlers because a
handler can be bypassed by the next route somebody adds. The API, the interface
and the sync endpoint all reach the same method.

The route table is one file so the whole surface can be read at once. That is
how you notice an endpoint sitting outside the middleware everyone assumed was
in front of it.

## Who may see what

Visibility is per project, and it is carried by a type rather than by a
convention.

```
grants (grantz)  ->  authz.Service.Scope  ->  domain.ProjectScope  ->  the WHERE clause
```

`domain.ProjectScope` is either "every project" or a list of ids, **and its zero
value reaches nothing**. That is the whole design: it sits on every list filter
and is a required argument to every single-row service method, so a query built
before the caller's scope was resolved returns an empty list rather than
everything, and a method that forgets it does not compile. It lives in `domain`
rather than in `authz` precisely so both sides can name it.

Three layers, each answering a different question:

| Layer      | Question                     | Where                           |
| ---------- | ---------------------------- | ------------------------------- |
| Middleware | is this caller signed in     | `internal/middleware`           |
| Handler    | may they do this verb at all | `guard.scope` / `guard.require` |
| Service    | may they do it to THIS row   | `owned()`, `RequireProject`     |

The third is the one middleware cannot do, because middleware runs before the
row is read. It is also the one that decides the answer shape: a row outside the
scope is `ErrNotFound`, never `ErrForbidden`.

[grantz](https://github.com/mstgnz/grantz) owns the grant tables and folds roles,
per-user exceptions and deny precedence into a decision. It returns a scope
without interpreting it, which is the right split: "does this row belong to that
scope" is domain knowledge. `internal/authz` is where that interpretation lives,
along with the permission catalogue and the three built-in roles, both written
from code at boot so upgrading the binary upgrades what a role can do.

`users.is_admin` is wired to grantz's superuser hook rather than expressed as a
role. A flag on the account cannot be revoked by editing the role mapping and
cannot silently miss a permission added in a later release, which is what a
break-glass path is for.

## Execution

Once a minute, at the fifth second:

1. The dispatcher reads the minute **from the database**.
2. It writes a `pending` row in `job_runs` for every job whose expression falls
   on that minute.
3. It hands pending rows to a bounded worker pool, in job priority order, and
   does not wait.

It runs nothing itself. If it did, twelve jobs falling on 16:15 would run in
sequence, each taking thirty seconds, and the next minute's work would slide
behind them. The job that must not slide is always the one that would.

The runner then claims a row:

```sql
UPDATE job_runs SET status = 'running' WHERE id = $1 AND status = 'pending'
```

If that affects no row, another process already has it and this call returns
having done nothing.

### Why replicas need no coordination

Two constraints, both in the database:

|                                   |                                                                        |
| --------------------------------- | ---------------------------------------------------------------------- |
| `UNIQUE (job_id, planned_minute)` | one run per job per minute, however many dispatchers reach that minute |
| the atomic claim above            | one execution per row                                                  |

Neither depends on anything held in a process. There is no leader election, no
lock table, no lease. The previous design used a lease and it had a failure the
constraints do not: a run that outlived its lease could be taken over and
executed a second time while the first was still going.

### Why the tick fires at second five

`domain.DispatcherSpec` is `5 * * * * *`. The seconds field must not be zero.

The tick is fired by the application clock while the minute it processes comes
from the database. Let the two differ by a few hundred milliseconds and a tick
on the exact boundary falls on the wrong side of it:

```
app 10:11:00.000  ->  database 10:10:59.7  ->  minute processed: 10:10
```

10:10 was already queued on the previous tick, so nothing is written, and 10:11
is never processed at all. Jobs without `run_missed` lose that minute for the
day.

Two machines always differ by a few milliseconds and NTP cannot remove that.
The fix is not to correct the clocks, it is to sample away from the boundary.

This failure is silent everywhere else: a repeated minute conflicts on the
unique index without raising an error, and a skipped minute writes nothing at
all. The dispatcher therefore audits its own chain of minutes and reports a
repeat or a gap, and `cronsole_dispatcher_minutes_lost_total` counts them. Any
increase in that counter is a fault.

### Outcomes

| status    | meaning                                                                |
| --------- | ---------------------------------------------------------------------- |
| `pending` | queued, not yet claimed                                                |
| `running` | claimed                                                                |
| `success` | the target answered inside the job's status range                      |
| `failed`  | the target answered outside it, or the request could not be made       |
| `timeout` | the client gave up                                                     |
| `skipped` | not run: the previous run was still going, or a chain step was blocked |

**A timeout is not a failure**, and the distinction is load bearing. When the
client gives up the target may well still be working, so nothing chained
follows it: running a dependent job on data that may be half written is worse
than not running it. A timeout is also never retried, for the same reason.

### The rules a job carries

- **`single_run`** skips a new run while the previous one is going. Checked in
  the dispatcher _and_ in the runner, because a chain step never passes through
  the dispatcher. Without the second check, a chained run can execute alongside
  the same job's scheduled run.
- **`run_missed`** replays minutes the dispatcher was down for, bounded by the
  job's `max_delay_min` and by `SCHEDULER_MISS_SCAN_MIN`. Suits a job that runs
  once a day; not one that runs every five minutes.
- **`priority`** orders jobs falling on the same minute. Lower goes first.
- **`timeout_sec`** is per attempt; **`max_duration_sec`** is when the watchdog
  decides a run is stuck. The second must exceed the first, or healthy runs get
  closed.
- **`retries`** are extra attempts a second apart, on failure only.

### Chains

A `job_link` says: when this job finishes with this outcome, run that one.

Hand-off has two routes. Short delays the runner takes itself, because leaving
them to a dispatcher that comes round once a minute meant a link written as two
seconds actually ran at the fifth second of the next minute. Long delays, and
anything blocked by the concurrency cap or `single_run`, stay pending with
`run_after` set and the dispatcher takes over.

The second route is also the safety net: the row is written **before** the
timer is set, so if the direct hand-off is lost to a restart the run still
exists.

A chain step that does not run still writes a row, marked `skipped` with the
reason. A step that vanishes without a trace is far harder to notice than one
that failed.

Depth is capped at five. The form additionally refuses a link that would close
a cycle, walking the graph forward from the target; the runner's cap would stop
the loop anyway, but silently and days later.

### The watchdog

Every five minutes it closes runs stuck in `running` past their
`max_duration_sec`, counts rows nothing ever collected, checks the heartbeat
age and the clock drift, reports jobs failing repeatedly, and prunes old rows.

Closing stuck runs is not cosmetic: a job marked `single_run` whose row is
stuck never executes again, because every tick decides the previous execution
is still going.

Alerts are suppressed by a fingerprint **of the warnings themselves**, not by a
timestamp. A new problem appearing during a quiet period gets through
immediately even if an unrelated alert went out two minutes ago.

The watchdog is the only thing that notices the scheduler itself failing.
Everything else reports on jobs, and a dead dispatcher just looks like a quiet
day. That is why `WATCHDOG_ALERT_TO` is required in production.

## The worker pool

Bounded concurrency and a bounded queue. Four properties, each present because
its absence is a specific production failure:

1. A `go func()` per run lets a slow target turn a busy minute into thousands
   of goroutines.
2. Every task runs on `context.Background()` with its own timeout, never the
   caller's context. A request context is cancelled when the response is
   written, which would kill work that had just started.
3. A panic handler writes to `app_logs`. Default recovery into stdout is
   invisible on a restarted container, which is the exact moment it matters.
4. Shutdown drains, so a run already started finishes and writes its result.

A full queue makes `Submit` return false rather than block. That is real
backpressure and the caller records it: the run stays pending and the next tick
picks it up. Blocking would push the backlog into the minute tick.

## Ordering on shutdown

Reverse of start up, and each step depends on the one before:

1. stop the scheduler, so nothing new is queued
2. stop the chain timers, so no delayed hand-off fires mid shutdown
3. stop accepting requests
4. drain the pool, so runs already started finish and write results
5. flush the notifier and the log writer, which is where the record of all of
   the above ends up

Draining before closing the server would let a new request queue work into a
pool that is already draining.

## Two ordering details that read backwards

**`PanicLogger` mounts inside `chi.Recoverer`.** `r.Use(A); r.Use(B)` builds
`A(B(handler))`, so a panic unwinds through B first, and chi's Recoverer
recovers _without_ re-panicking. The logger therefore has to be the inner one:
it records, re-panics, and the Recoverer wrapped around it writes the 500.
Mounted the other way, every production crash is invisible outside stdout, and
the symptom is an absence.

**The dispatcher's `Tick` uses a named return.** The deferred write of the tick
duration runs after the return value is copied. With an unnamed result it wrote
into a local nobody could see, and every tick logged a duration of zero, which
is the only measurement that says whether a tick outran its minute.

## Where to hook something in

- **Run results going elsewhere:** implement `service.RunObserver`. It is
  called once per finished run, after the row is written, so an observer can
  never claim an outcome the history does not have. `pkg/metrics` is the
  reference implementation.
- **Anything HTTP shaped:** a chain link pointing at your own endpoint. It is
  logged, retried and visible like any other job.
- **Pulling data:** the JSON API, or the database. `job_runs` is the record.

## What is not here

No leader election, no message queue integration, no sub-minute scheduling. Each
was considered; see [CONTRIBUTING.md](../CONTRIBUTING.md) for why, before
building one.
