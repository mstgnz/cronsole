# Security

## Reporting a vulnerability

Report privately through GitHub's [security advisory
form](https://github.com/mstgnz/cronsole/security/advisories/new), not as a
public issue.

Please include what an attacker gains and the smallest way to reproduce it.
You will get an acknowledgement within a few days. This is a small project
maintained in spare time, so a fix may take longer than that; you will be told
where it stands rather than left waiting.

## What this service is

A scheduler that issues HTTP requests on a timer, holding the addresses,
headers and bodies of those requests. **An account on it can cause requests to
be made from wherever it is deployed.** That is the feature, and it is also the
thing to think about when placing it: treat operator access as you would
access to the machines it can reach.

## What it does to limit that

**Project base address.** When a project sets `base_url`, every job under it
gives a path only and the host comes from the project. An account that can edit
jobs cannot move one to a different host, because the host is not a field it
can write. Setting it is the single most useful thing an operator can do.

**Scheme allowlist.** Only `http` and `https`. Not a denylist, so a scheme
nobody enumerated is refused by default. Credentials in an address are refused
outright; they would otherwise be copied into every run row.

**No redirect following.** A target answering 302 is recorded as 302. Following
one would also let a target on an allowed host land on a host that is not.

**`OUTBOUND_ALLOW_PRIVATE`.** Defaults on, because calling internal endpoints
with no public address is the point of the service. Set it false and loopback,
private, link local and the cloud metadata address are refused.

**Project API keys** are 24 random bytes from `crypto/rand`, shown once, stored
only as a SHA-256 hash, and compared in constant time. A key is scoped to one
project: it reaches that project's jobs and nothing else. Rotation is immediate.

**Sessions** are HS256 tokens in an `HttpOnly`, `SameSite=Lax` cookie, `Secure`
outside local development. The algorithm is pinned rather than read from the
token header. Logging out and changing a password move a per account cut-off
that retires every token issued before it, which is what makes those actions
actually end a session.

**Passwords** are bcrypt at cost 12. A login attempt for an address that does
not exist costs the same time as one for an address that does, so the form
cannot be used to enumerate accounts. Login is rate limited per client address,
read from a platform header or the socket rather than from a browser supplied
`X-Forwarded-For`.

**Authorization is scoped, and the scope is a required argument.** Every service
method that touches project-owned data takes a `domain.ProjectScope`, whose zero
value reaches nothing. A query built before the caller's scope was resolved
returns an empty list rather than everything, and a new method that forgets it
does not compile. The check that decides a single row lives in the service, not
the handler, because a handler can be bypassed by the next route somebody adds.

**A refused row answers 404, not 403.** Telling somebody that a job exists but
is not theirs confirms an id, and an id is the only thing needed to probe the
rest of the table.

**Every statement is parameterised.** There is no path in the repository that
concatenates a caller supplied value into SQL.

**Request forgery.** State changing browser routes are same origin checked in
addition to the `SameSite` cookie. The JSON API requires
`Content-Type: application/json`, which a plain HTML form cannot set.

## Known limits, stated rather than implied

- **Job headers are stored in plain text.** The `is_secret` flag masks a value
  on screen and in API responses; it is not encryption. Anyone with database
  access, or operator access to the job, can read them. Prefer a token your
  target can rotate.
- **A platform administrator sees everything.** `users.is_admin` is a
  break-glass flag rather than a role: it bypasses the permission check
  entirely, so it cannot be revoked by editing the role mapping and cannot
  silently miss a permission added in a later release. Everyone else reaches
  only the projects they were granted. Keep the number of administrators small;
  a project administrator can manage their own project's members without one.
- **A permission change takes up to the cache window to reach every replica.**
  Grants are cached per process for 30 seconds. Revoking somebody's access is
  immediate on the instance that did it and lands on the others within that
  window. There is no cross-instance invalidation.
- **Field restrictions narrow the interface, not the database.** Hiding run
  output from a role removes it from every screen and API response that role can
  reach; the row still holds it, and anyone with database access reads it.
  Hiding the address called does not redact it from the error text, because a
  transport error quotes the address it failed to reach.
- **`/metrics` is unauthenticated** when `METRICS_ENABLED` is on, as a scrape
  target normally is. It discloses project names, job names and run counts. Do
  not expose it through a public ingress.
- **Run output is stored.** Up to 16000 characters of each response body goes
  into `job_runs.output` and is shown on the runs screen. If a target returns
  anything sensitive, that is where it ends up. Retention is
  `RUN_RETENTION_DAYS`.
- **Rate limiting is per process.** With several replicas each has its own
  counters, so the effective limit is multiplied by the replica count. Adequate
  for slowing a guessing attack, not a quota.
- **The interface loads Bootstrap, HTMX and Chart.js from a CDN**, and the
  Content-Security-Policy names those hosts and permits inline scripts because
  the templates carry them. Self-hosting the assets and moving to a nonce would
  be a welcome contribution.

## Deploying it safely

- Set `JWT_SECRET` to at least 32 random characters. Boot is refused below
  that, because a guessable signing key is every account.
- Leave `DB_SSLMODE=require`. Production refuses `disable` for anything but a
  loopback host.
- Set `WATCHDOG_ALERT_TO`. Production boot is refused without it: the watchdog
  is the only thing that notices the scheduler itself dying.
- Connect as a least privilege database role. The application needs no `DROP`
  and no `CREATE`; migrations are run separately by an owner.
- Complete `/setup` before the deployment is reachable by anyone else. Until
  the first account exists that screen is the whole service, and it hands an
  administrator account to whoever submits it. It closes for good afterwards.

## Supported versions

The `main` branch. There are no maintained release branches.
