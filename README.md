# Cronsole

One place where every project's scheduled work is defined, run and recorded.

A project registers its jobs here, in one request from its own deploy, and this
service calls them on time. Every execution leaves a row: how long it took,
what came back, and what it triggered next.

Go, PostgreSQL, no build step for the interface, one binary.

## Why

A crontab line runs and, unless it writes somewhere itself, leaves nothing
behind. There is no history to read, no way to tell a job that failed from one
that never started, and no way to see the whole schedule at once when it is
spread across three servers and a hosted pinger.

This replaces that. The definitions live in one table, the executions in
another, and both are on a screen.

## Quick start

```bash
git clone https://github.com/mstgnz/cronsole
cd cronsole
cp .env.example .env

# fill in at least: DB_*, JWT_SECRET (32+ chars)
#   openssl rand -hex 32

make dev-db        # a throwaway Postgres on :55433, or point .env at your own
make dev-schema    # apply cronsole.sql
make run
```

Open `http://localhost:3333`. A deployment with no accounts serves one screen,
`/setup`, which creates the first administrator and then closes permanently.
Whoever reaches that address first claims it, so do it before the deployment is
public. Then create a project, and a job. One form, one save, and it runs.

## The model

```
project ──< job ──< job_schedule        one job, several cron expressions
                └─< job_header          headers sent with the request
                └─< job_link            when this finishes, run that one
                └─< job_run             every execution, with its result
```

**Project** is the unit of ownership. It holds one API key and, optionally, a
base address. With a base address set, every job under it gives a path and the
host comes from the project, so an account that can edit jobs cannot move one
to a different host.

**Job** is what to call, when, and how patiently. It has no cron expression of
its own, because a real job frequently needs several that cannot be folded into
one: `*/5 * * * *` alongside `58,59 15 * * *`.

**Run** is one execution: created before anything happens, claimed by a runner,
closed into success, failure, timeout or skipped. Nothing executes without
leaving one.

**Access is per project.** Somebody given a role on one project sees that
project's jobs and runs and nothing else, not even that the others exist. Three
roles ship: reader, writer and project administrator, the last of which manages
its own project's members without needing you. See
[docs/api.md](docs/api.md#roles).

## How it runs things

Once a minute, at the fifth second, the dispatcher reads the minute **from the
database**, writes a pending run row for every job that is due, and hands those
rows to a bounded worker pool. It executes nothing itself.

The runner then claims a row with one statement:

```sql
UPDATE job_runs SET status = 'running' WHERE id = $1 AND status = 'pending'
```

If that affects no row, another process already has it. **That is the only
thing preventing a run from executing twice**, and it is enough: replicas run
side by side with no coordination, no leader election and no lock table.

Two constraints, both in the database:

- `UNIQUE (job_id, planned_minute)` gives one run per job per minute
- the atomic claim gives one execution per row

Why the fifth second and not the minute boundary, and the rest of the design:
[docs/architecture.md](docs/architecture.md).

## What you get for using it

- **Timeout is not failure.** When the client gives up the target may still be
  working, so the run is recorded as `timeout` and nothing chained follows it.
- **One run at a time**, if you want it. Checked in the dispatcher *and* the
  runner, because a chained run never passes through the dispatcher.
- **Missed minutes replayed**, if you want it, bounded so a scheduler that was
  down for a week does not queue a week of work the moment it returns.
- **Chains.** When this job succeeds, run that one, after a delay. Every step
  is itself a run, so it appears in the history like anything else.
- **A watchdog** that closes stuck runs, reports a dead dispatcher, clock
  drift and jobs failing repeatedly, and prunes old rows. It is the only thing
  that notices the scheduler itself failing; everything else reports on jobs,
  and a dead dispatcher just looks like a quiet day.
- **Prometheus metrics** at `/metrics`, every one written from a real call
  site.
- **Per-project access**, scope based rather than a flag: six brands on one
  console, each team seeing only its own. Down to whether a reader sees the
  response body a job returned, which is often the part carrying customer data.
- **English and Turkish**, switchable per browser. The API stays English
  whatever the interface is set to, because an error message that changes
  wording with a header is one nobody can grep for.

## Registering jobs from a project

A project declares its whole schedule in one request, so the definitions live
in the repository they belong to rather than only here. Idempotent: the same
payload twice changes nothing the second time.

```bash
curl -X POST https://cron.example.com/api/v1/sync \
  -H 'X-API-Key: cj_...' \
  -H 'Content-Type: application/json' \
  -d '{
    "prune": true,
    "jobs": [
      {
        "code": "daily-report",
        "name": "Daily report",
        "url": "/cron/daily-report",
        "schedules": ["0 3 * * *"],
        "timeout_sec": 120,
        "active": true,
        "links": [{ "target": "cleanup", "delay_sec": 30 }]
      },
      { "code": "cleanup", "name": "Cleanup", "url": "/cron/cleanup" }
    ]
  }'
```

`prune` **deactivates** jobs missing from the payload; it never deletes. An
omitted field keeps the stored value, so a timeout raised on the screen
survives the next deploy. A partial failure answers `207` with a per job
outcome, so a pipeline cannot read "eleven of twelve registered" as clean.

Or with the client, which gets the exit code right:

```bash
cronsolectl sync -f cron.yaml --prune
```

It exits `3` when some jobs were refused, so a pipeline cannot read "eleven of
twelve registered" as a clean deploy. `cronsolectl run <code> --wait` blocks
until the run finishes and fails if it did not succeed.

Full reference: [docs/api.md](docs/api.md), or `/docs` on a running deployment.

## Documentation

| | |
|---|---|
| [docs/architecture.md](docs/architecture.md) | how it works and why, including the decisions that look wrong until you know what they prevent |
| [docs/api.md](docs/api.md) | every endpoint, the sync payload, cron grammar |
| `/docs` on a running deployment | the same reference, rendered and callable, from `/openapi.yaml` |
| [docs/operations.md](docs/operations.md) | configuration, metrics, alerts, reading a failure |
| [k8s/README.md](k8s/README.md) | Kubernetes deployment |
| [CONTRIBUTING.md](CONTRIBUTING.md) | layout, house rules, what is deliberately absent |
| [SECURITY.md](SECURITY.md) | reporting a vulnerability, and the known limits |

## Deployment

Multiple replicas are safe. `make docker-build` and `make docker-run`, or the
manifests in [k8s/](k8s). The container runs as an unprivileged user and
carries only the binary: the interface is embedded in it.

Before production, three settings are refused if missing, each because its
absence is invisible while everything is fine: `JWT_SECRET` of at least 32
characters, `WATCHDOG_ALERT_TO`, and a configured mail server. See
[docs/operations.md](docs/operations.md#boot-is-refused-when).

## Schema

`cronsole.sql` is the authoritative definition and is hand written. There are no
migration files. It includes the five `grantz_*` tables that hold the roles and
grants, copied from [grantz](https://github.com/mstgnz/grantz) so that one file
sets up the whole database; re-copy them when that dependency is upgraded and
its schema changes.

A fresh installation imports that one file and nothing else.

## Status

Used in production by its author, on a handful of projects. The interfaces are
not frozen. Issues and pull requests are welcome; please read
[CONTRIBUTING.md](CONTRIBUTING.md) first, particularly the list of things that
were considered and left out.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
