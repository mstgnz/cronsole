# Contributing

Thanks for looking. This is a small project with a narrow purpose, so the most
useful contribution is usually a bug report with a way to reproduce it.

## Getting it running

```bash
git clone https://github.com/mstgnz/cronsole
cd cronsole
cp .env.example .env.local          # fill in JWT_SECRET and DB_*
make dev-db                         # throwaway Postgres on :55433
make dev-schema
ENV_FILE=.env.local make live
```

`make live` rebuilds on change. It has to: the interface is embedded in the
binary, so editing a template or a stylesheet needs a rebuild, not a refresh.

`ENV_FILE` exists so a local run never has to touch a `.env` that points at
something real.

## Before opening a pull request

```bash
make lint       # go vet plus a gofmt check
make test       # everything that does not need a database
make test-repo  # the repository tests, against a throwaway Postgres in Docker
make cover      # coverage, whole project
```

CI runs the same, plus `go test -race` and `govulncheck`.

**The floor is 85% across the project**, measured with `-coverpkg` over every
package because most of the interface is covered by tests that live in
`internal/router` and drive the whole stack. Without that flag the figure is
each package's own tests only, and the handlers read as untested when they are
not. `internal/repository/memrepo` is left out of the measurement: it is the
in-memory stand-in the tests run against, so counting it rewards exercising a
fake over exercising the code that ships.

`make cover` runs exactly what CI runs. Two things still move the number
between your machine and the build:

- **The platform.** `internal/hostinfo` compiles a different file on Linux than
  on macOS, and the Linux one is ten times the size. CI is the figure that
  counts; if yours is comfortably above the floor and CI is not, that is why.
- **Go's version.** CI takes it from `go.mod`. A newer toolchain instruments
  statements slightly differently, so the two totals never match exactly.

`make test-repo` starts its own database on a separate port from `make dev-db`.
The repository tests empty every table between cases, so pointing them at the
development database would delete whatever you were working on.

## What the code expects of you

**The layers only point one way.** `handler -> service -> domain`, with
`repository` implementing the domain interfaces. `domain` imports nothing
outside the standard library. A handler holds no rule, because a handler can be
bypassed by the next route somebody adds; the API, the interface and the sync
endpoint all reach the same service method, so a rule written once applies to
all three.

**SQL lives in `internal/repository` and nowhere else**, as a constant next to
the method that runs it. Every statement is parameterised. Where a filter set is
dynamic the clauses come from a fixed set of literals and only the values are
bound.

**Comments say why, not what.** One or two lines. The interesting comments in
this repository are the ones explaining a decision that looks wrong until you
know what it prevents: why the dispatcher fires at second five, why a timeout
does not trigger a chain step, why `PanicLogger` mounts inside `chi.Recoverer`.
If you change one of those behaviours, change the comment in the same commit.

**Tests describe behaviour.** The fakes behave like the database rather than
recording calls, and there are three levels of them:

- `internal/repository/memrepo` implements every repository interface in
  memory, enforcing what the database enforces: the scope narrows every list, a
  soft-deleted row disappears, a duplicate code is refused. Test-only, but
  ordinary code because more than one package needs it.
- `internal/router` builds the WHOLE application over it and drives real
  requests through the real middleware, handlers, services and authorization.
  That is where the access rules are checked, because "a reader on one brand
  cannot see another brand's jobs" is a property of the running system and
  nothing smaller can demonstrate it.
- `internal/repository` runs against a real Postgres, because the questions
  there are the database's: does the scope reach the WHERE clause, does the
  unique index refuse the second row, do ten processes claiming one run leave
  exactly one winner.

A test that asserts a mock was called proves nothing about the scheduler.

**`cmd/cronsole` is the composition root and it is tested.** `build` assembles
the graph and returns an error; `main` turns that into a fatal. Every other test
builds its own graph, so a mistake in the real one — a runner without its host
resolver, a health handler without the pool — would be invisible to all of them.

**Interface text goes through `t`, and the English source is the key.**
`{{ t "Save changes" }}` in a template, `tr(r, "...")` in a handler. There is no
`en.json`: an untranslated string renders as itself, so a lagging catalogue
shows English rather than a raw key like `jobs.form.save_button`.

The cost of source-as-key is that editing English copy orphans its translation
silently, and `internal/handler/i18n_test.go` is what makes that safe. It scans
every template and every handler, and fails when a catalogue and the code
disagree **in either direction**: a missing translation and a leftover entry are
both errors. A string reached through a non-literal call, `{{ t . }}` on a
status or a page title, is invisible to that scan and must be declared in
`i18n.Dynamic`.

Translating is per screen and finished per screen. Half of a screen in Turkish
is worse than none of it.

**`internal/handler/api_handler.go` never calls `tr`.** The language middleware
is not mounted on `/api/v1` at all, so an API error is the same string whatever
the caller's `Accept-Language` says. A message that changes wording with a
header is one nobody can grep for in a log.

## Changing the schema

`cronsole.sql` is the authoritative definition and is hand written. There are no
migration files. A change goes into that file, and anything an existing
installation has to run is handed over as an explicit script.

If you add a table or a column:

- every foreign key gets an explicit `ON DELETE` and an index on the
  referencing column
- every index has a query it serves, written down in a comment
- nullable is the exception, and gets a reason
- money is `numeric`, time is `timestamptz`, always

## Things that are deliberately absent

Please open an issue before building any of these; they were considered and
left out, and the reasoning may still hold.

- **A leader election.** There is none and none is needed: exclusivity comes
  from a unique index and an atomic claim, so replicas need no coordination.
- **A message queue integration.** An earlier version carried Kafka and
  Elasticsearch clients that nothing ever called. The seam for shipping run
  results elsewhere is `service.RunObserver`, or a chain link pointing at your
  own endpoint.
- **Per-user visibility.** Every signed-in operator sees every project. This is
  a shared operations console; `is_admin` gates account management, not
  visibility.
- **Seconds level scheduling.** Cron has minute resolution and so does the run
  table's unique index. Sub-minute work belongs somewhere else.

## Reporting a bug

Include the cron expression, the job's timeout and `single_run` setting, and
the run row if you have it. Most scheduling reports come down to one of three
things: an expression that does not mean what the writer thought (the form
shows the next runs for exactly this reason), a job overlapping itself, or a
target that answers a status outside the job's success range.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## Licence

By contributing you agree that your work is licensed under the Apache License
2.0, the same as the rest of the project.
