# API

Two audiences, and the difference matters.

A **project** calls in with an API key and may only ever see and act on its own
jobs. Every project scoped method checks ownership against the authenticated
project rather than trusting an identifier in the path.

An **operator** calls in with a session token and sees everything.

All responses share one envelope:

```json
{ "status": true, "data": { }, "message": "", "errors": [ ] }
```

`message` carries a sentence chosen by the handler, never a driver or library
error: those name tables, query fragments and file paths.

---

## Project endpoints

Authenticate with `X-API-Key: cj_...` (or `Authorization: Bearer cj_...`).
A key is issued once, on the Projects screen, and only its hash is stored. If
it is lost, rotate it.

Rate limited to 60 requests a minute per client address.

### `POST /api/v1/sync`

Register the project's whole job set from one declaration. Idempotent: the same
payload posted twice changes nothing the second time.

```jsonc
{
  "prune": true,
  "jobs": [
    {
      "code": "daily-report",          // required, stable, never renamed
      "name": "Daily report",
      "description": "",
      "tag": "reports",
      "url": "/cron/daily-report",     // path if the project has a base address
      "method": "GET",
      "body": "",
      "schedules": ["0 3 * * *"],
      "headers": { "X-Cron-Token": "..." },
      "timeout_sec": 120,
      "max_duration_sec": 1200,
      "retries": 0,
      "single_run": true,
      "run_missed": false,
      "max_delay_min": 10,
      "priority": 100,
      "success_min": 200,
      "success_max": 399,
      "active": true,
      "links": [
        { "target": "cleanup", "condition": "success", "delay_sec": 30 }
      ]
    }
  ]
}
```

Behaviour worth knowing before you rely on it:

- **`prune` deactivates, never deletes.** A partial payload posted by accident
  costs a switch being flipped back, not a job definition and its history.
- **An omitted field keeps the stored value.** A timeout an operator raised on
  the screen survives the next deploy.
- **`single_run`, `run_missed` and `active` are tri-state.** Absent means
  "leave it as it is"; `false` means "turn it off". Collapsing the two is how a
  deploy quietly disables `single_run` on a job that writes.
- **`single_run` defaults on** for a job declared here. A deploy script has not
  thought about overlap, and overlapping is the outcome that corrupts data.
- **`headers`: omit to leave them, send `{}` to clear them.**
- **`schedules`: omit to leave them, send `[]` to clear them.** A job with no
  schedule is registered and runs only when something triggers it: a chain
  link, the run button, or the trigger endpoint below.
- **`links` name a target by `code`**, not by id. Ids belong to this service;
  codes are what the declaring project knows about itself. They are applied in
  a second pass, so a link may point at a job declared later in the same
  payload.
- **`code` is fixed once created.** History, chain links and alert subjects are
  keyed by it.

One job failing does not stop the rest. A declaration of twelve jobs with one
bad expression registers eleven and names the one that failed.

**Status:** `200` when everything applied, **`207`** when any job failed. A
deploy pipeline must not read "eleven of twelve registered" as a clean run.

```json
{
  "status": false,
  "data": {
    "project": "sovtajyeri",
    "created": 1, "updated": 10, "deactivated": 0, "failed": 1,
    "jobs": [
      { "code": "daily-report", "action": "created", "job_id": 42 },
      { "code": "broken", "action": "failed",
        "errors": ["schedules.0: expression must have 5 fields, found 4"] }
    ]
  }
}
```

`action` is one of `created`, `updated`, `deactivated`, `failed`.

### `GET /api/v1/jobs`

This project's jobs, with each one's schedules, next run and 24 hour counts.

### `POST /api/v1/jobs/{code}/run`

Run one now. Addressed by code, which is also what scopes the lookup to the
project: a key holding one project cannot address another project's job,
because there is no query that would find it.

Queues a row and hands it over; it does not execute inline. That is what keeps
a manual run subject to the same `single_run` rule, the same concurrency cap
and the same logging as a scheduled one.

**Status:** `202`, with `{ "run_id": 1234, "job_id": 42, "code": "..." }`.

### `GET /api/v1/runs`

This project's history. Query parameters: `status`, `from`, `to`, `limit`,
`offset`. Defaults to the last 24 hours.

---

## Operator endpoints

Authenticate with `Authorization: Bearer <session token>`. Mounted under
`/api/v1/admin`.

Every one of these is narrowed to the projects the account may reach, which for
a platform administrator is all of them. A list returns what is visible; a
single row outside that set answers `404`, not `403`, so an id cannot be used to
learn that a job exists somewhere the caller cannot see.

| | permission | |
|---|---|---|
| `GET /me` | — | the signed in account |
| `GET /summary?hours=24` | `jobs.read` | the dashboard figures, over the visible projects |
| `GET /schedule-preview?expression=...` | — | validate an expression and get its next firings |
| `GET /projects` | `projects.read` | the visible projects with counters |
| `GET /jobs` | `jobs.read` | filters: `project_id`, `tag`, `active`, `q`, `limit`, `offset` |
| `POST /jobs` | `jobs.create` | create; body is the job input, JSON only |
| `GET /jobs/{id}` | `jobs.read` | one job with schedules, headers, chain and recent runs |
| `PUT /jobs/{id}` | `jobs.update` | update; `code` is ignored |
| `DELETE /jobs/{id}` | `jobs.delete` | remove the definition, keep the history |
| `POST /jobs/{id}/run` | `jobs.run` | queue a run, `202` |
| `GET /runs` | `runs.read` | filters: `job_id`, `project_id`, `status`, `from`, `to` |

`GET /runs` and `GET /jobs/{id}` honour the field restriction on `runs.read`: a
role that may not see run output gets the rows with `output`, `error` or
`request_url` empty rather than being refused the endpoint. See
[Roles](#roles).

### `POST /api/v1/login`

Issues a token. JSON only, rate limited to ten attempts per five minutes per
client address, and a wrong address is indistinguishable from a wrong password.

```bash
TOKEN=$(curl -sS -X POST "$CRON_URL/api/v1/login" \
  -H 'Content-Type: application/json' \
  -d '{"email":"ops@example.com","password":"..."}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["data"]["token"])')

curl -sS -H "Authorization: Bearer $TOKEN" "$CRON_URL/api/v1/admin/summary"
```

The token is valid for 24 hours. It sets no cookie, and it is retired early by
that account logging out or changing its password.

---

## Roles

Access is granted per project. A person is given a role over one or more
projects and reaches nothing else: not the jobs, not the runs, not the project
in the picker.

| role | may |
|---|---|
| Reader | see the project, its jobs and their history. Changes nothing. |
| Writer | the above, plus add, edit, delete and run jobs. Cannot switch a job on. |
| Project administrator | everything in the project: activation, the API key, and granting access to others. |

Two things sit outside the roles. `users.is_admin` is a platform administrator:
it bypasses the permission check entirely, reaches every project, and is the
only account that can create projects, manage accounts or edit these roles. And
a role assignment with no scope reaches every project, which is the second way
to give somebody a platform-wide view without making them an administrator.

**Run visibility** is a property of the role, set under Settings → Roles. A
reader who may know that a job failed does not necessarily get the response body
it failed with: each role independently sees or does not see the response body,
the error text and the address called. Status, timing, HTTP code and which job
ran are always visible to anyone who may read runs at all.

The escalation rules, enforced in the service rather than the interface:

- Nobody changes their own access.
- Nobody grants a role above their own.
- A project administrator cannot target a platform administrator, and cannot
  hand out a custom role.
- An assignment covering every project cannot be narrowed one project at a time.

A grant is cached per process for 30 seconds, so a change is immediate on the
instance that made it and reaches the others within that window.

---

## Unauthenticated

| | |
|---|---|
| `GET /healthz` | the process is up. Touches nothing else: a liveness probe that queried the database would restart the service every time the database hiccups |
| `GET /readyz` | the database answers, plus queue depth and capacity |
| `GET /metrics` | Prometheus, when `METRICS_ENABLED` is on |

`/metrics` discloses project names, job names and run counts. Do not expose it
through a public ingress.

---

## The interactive reference

A running deployment serves the same thing this page describes, rendered and
callable, at **`/docs`**. It is behind sign in, like every other screen.

The document behind it is at **`/openapi.yaml`**: real OpenAPI 3.1, so Postman,
Insomnia and any client generator can read it without scraping the page.

It is hand written, and `internal/router/openapi_test.go` compares it against the
route table on every build: a route added without a line in the document fails
the test, and a documented path that no route serves fails too. That is what
makes hand written safe. The previous revision of this service shipped a
generated Swagger file describing an API two versions old, which is what happens
when nothing fails on drift.

## Errors

**Always English.** The interface is available in English and Turkish, but the
language middleware is not mounted on `/api/v1`: an `Accept-Language` header
changes nothing here. An error whose wording depends on a request header is one
nobody can grep for in a log, and a client that switches on the message would
break the day somebody adds a language.

| status | when |
|---|---|
| `400` | the request could not be read |
| `401` | no credential, or one that does not verify |
| `403` | authenticated but not allowed |
| `404` | no such thing, or not yours |
| `409` | it already exists |
| `415` | a JSON endpoint got something else |
| `422` | validation failed; `errors` names each field |
| `429` | rate limited |
| `500` | something broke; the detail is in the service log, not the response |

A validation failure reports **every** problem at once:

```json
{
  "status": false,
  "message": "code: use lower case letters, digits, dash and underscore (and 2 more)",
  "errors": [
    { "field": "code", "message": "use lower case letters, digits, dash and underscore, 2 to 64 characters" },
    { "field": "url",  "message": "this project has a base address, so the target must be a path such as /cron/report" },
    { "field": "timeout_sec", "message": "must be between 1 and 600 seconds" }
  ]
}
```

A form that reports one problem per submission takes as many round trips as it
has mistakes.

---

## Cron expressions

Five fields: `minute hour day-of-month month day-of-week`.

Supported: `*`, `n`, `a-b`, `a-b/s`, `*/s`, `n/s`, and comma separated lists of
those. Day of week takes `0-7`, where both `0` and `7` are Sunday.

Not supported, deliberately: `@daily` style descriptors, a seconds field, and
the Quartz extensions `L`, `W` and `#`.

**The day rule is Vixie's**, which reads as a bug and is the standard: when
day-of-month and day-of-week are *both* restricted they are joined by OR
("the 1st of the month, **or** any Monday"); when either is `*` the other
decides alone.

An invalid expression is an error, never an expression that silently never
matches. Use `schedule-preview` before saving one: a job that never runs is the
failure nobody notices.

---

## Example: a deploy step

With the client, which is what it exists for:

```bash
export CRONSOLE_URL=https://cron.example.com
export CRONSOLE_API_KEY=cj_...

cronsolectl sync -f cron.yaml --prune
```

It exits `0` when everything applied, `3` when some jobs were refused, and
prints which ones and why. That `3` is the case worth having a code for: a
pipeline that treats `207` as success deploys eleven of twelve jobs and reports
green.

With curl, if you would rather not install anything:

```bash
#!/bin/sh
set -e
curl -fsS -X POST "$CRON_URL/api/v1/sync" \
  -H "X-API-Key: $CRON_API_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @cron.json \
  -o /tmp/sync.json -w '%{http_code}' | grep -qx 200 || {
    echo "cron sync reported failures:"; cat /tmp/sync.json; exit 1;
  }
```

`grep -qx 200` rather than `curl -f` alone, for the same reason.

## The command line client

`cronsolectl` speaks the project side of this API. Build it with `make cli`, or
`go install github.com/mstgnz/cronsole/v2/cmd/cronsolectl@latest`.

It reads `CRONSOLE_URL` and `CRONSOLE_API_KEY` from the environment. The key is
read from there **by preference**: a key passed as `--key` lands in the shell
history and in the process list, where anyone on the machine can read it.

| | |
|---|---|
| `cronsolectl sync [-f cron.yaml] [--prune] [--dry-run]` | register the whole schedule. `--dry-run` prints the payload and sends nothing |
| `cronsolectl jobs` | this project's jobs |
| `cronsolectl run <code> [--wait] [--timeout 5m]` | queue a run; `--wait` blocks and fails if it did not succeed |
| `cronsolectl runs [--status failed] [--since 24h]` | recent runs |

Add `--json` to any of them to get the raw payload instead of a table.

**The declaration is YAML or JSON.** YAML because a schedule file is read far
more often than it is written and JSON has no comments: the reason a job runs at
03:00 belongs next to the expression.

```yaml
jobs:
  # The base address is on the project, so this is a path.
  - code: daily-report
    name: Daily report
    url: /cron/daily-report
    schedules: ["0 3 * * *"]
    timeout_sec: 120
    links:
      - target: cleanup
        delay_sec: 30

  - code: cleanup
    name: Temporary file cleanup
    url: /cron/cleanup
    schedules: []      # runs only when the chain above triggers it
```

### Exit codes

| | |
|---|---|
| `0` | fine |
| `1` | the request failed, or the server refused it |
| `2` | the command line was wrong, or a credential is missing |
| `3` | sync applied some jobs and refused others |
| `4` | `--wait`, and the run did not succeed |

`run` without `--wait` exits `0` once the run is **queued**. It is not a success
report: the job has not executed yet. Use `--wait` when the pipeline needs to
know the outcome.
