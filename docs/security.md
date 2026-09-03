# Security

NEXUS is a delivery control plane, which makes it a high-value target: it holds
repository connections, deploys to production, and can roll production back. This
describes the boundaries it enforces and, equally, the ones it does not.

## Identity

**Passwords** are hashed with Argon2id at 64 MiB, one pass, four threads. The encoded
hash carries its own parameters, so the cost can be raised later without invalidating
existing hashes.

Login spends comparable time when the account does not exist — the dummy hash is verified
anyway — so response timing does not reveal which addresses are registered. A missing
user and a wrong password return the same message for the same reason.

**Session tokens** are HS256 JWTs, twelve hours by default. The algorithm is pinned at
parse time:

```go
jwt.ParseWithClaims(token, claims, keyFunc,
    jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
    jwt.WithIssuer(s.issuer))
```

Without pinning, a token presenting `alg: none` or an asymmetric algorithm could be
accepted through the wrong verification path. A role that is not one of the three
recognised values fails the parse, so a stale or tampered token fails closed.

**API tokens** for the CLI are 32 random bytes, prefixed `nxs_`. Only the SHA-256 is
stored; the plaintext is shown once at creation and never again, and the response says
so. A database disclosure yields nothing usable.

Both token forms arrive on the same `Authorization: Bearer` header and are told apart by
prefix, so there is one authentication path rather than two schemes.

**One exception exists** and is worth stating plainly: the SSE endpoint accepts its token
as a query parameter, because `EventSource` cannot set headers. It is scoped to that one
route by an explicit path check, and the request logger records `URL.Path` and never
`RawQuery`, so tokens do not reach the logs.

**Production configuration is validated at startup.** `NEXUS_AUTH_JWT_SECRET` must be at
least 32 bytes and `NEXUS_WEBHOOK_SECRET` must be set when `NEXUS_ENVIRONMENT=production`.
The development fallback secret cannot silently weaken a production deployment, because
`Validate` refuses to start one without a real secret.

## Authorisation

Three roles, checked in process on every request. The permission map is in code rather
than in an editable table: a control plane with three roles does not need a policy
engine, and every decision is a map lookup rather than a database round trip.

| Permission | Admin | Developer | Viewer |
|---|:-:|:-:|:-:|
| `project:read`, `pipeline:read`, `deployment:read`, `environment:read` | ● | ● | ● |
| `project:create`, `project:update` | ● | ● | |
| `pipeline:run`, `pipeline:cancel` | ● | ● | |
| `deployment:create`, `deployment:rollback` | ● | ● | |
| `environment:create` | ● | ● | |
| `audit:read` | ● | ● | |
| `project:delete`, `environment:delete` | ● | | |
| `secret:manage`, `member:manage` | ● | | |

Two boundaries are deliberate. A developer can deploy and roll back but cannot delete a
project or an environment, because those destroy history and running systems rather than
changing them. Only an admin manages secrets, because the ability to read or rotate a
secret reference is effectively the ability to reach whatever it protects.

**An unrecognised role holds nothing.** A role read from a corrupted row or a forged
token gets an empty permission set, not a default.

The console hides actions the user cannot take, as a courtesy. The server checks
regardless.

## Webhooks

Deliveries are assumed to be untrusted, duplicated and occasionally forged.

**Signatures** are verified with `hmac.Equal` rather than `==`. A byte-by-byte comparison
that returns early leaks, through timing, how much of a forged signature was correct —
which is enough to construct a valid one given enough attempts.

**Rejected deliveries are persisted** before the error is returned, so forged or
misconfigured deliveries are visible in the audit surface rather than only in logs.

**Duplicates cannot double-build.** The unique index on `(provider, delivery_id)` is the
entire defence: a redelivery loses the insert and returns without starting a second run.

**Payloads are bounded** at 5 MB and the body is read through a `LimitReader`.

**Only relevant events act.** A push to a non-default branch, a branch deletion, or a
pull-request action other than open/reopen/synchronize is recorded and ignored — but the
endpoint still returns 2xx, because GitHub disables a webhook whose endpoint keeps
erroring, and returning 500 for a repository NEXUS does not track would eventually
silence a hook that is working correctly.

## Secrets

NEXUS does not implement encryption. It stores **references**:

```sql
CREATE TABLE secret_references (
    project_id  text NOT NULL,
    key         text NOT NULL,
    backend     text NOT NULL,   -- LOCAL | VAULT | AWS_SM | GCP_SM
    locator     text NOT NULL,   -- opaque, interpreted by the backend
    ...
);
```

No column here ever holds a secret value, and no API response returns one. Building a
home-grown encryption product would be a worse version of software that already exists.

The worker inherits only `PATH` and a short list of platform essentials into step
environments, so a secret exported into the worker process does not silently become
visible to every step it runs.

The structured logger carries a redaction list as a backstop — `password`, `token`,
`secret`, `authorization`, `api_key`, `database_url` and others are replaced with
`[redacted]` regardless of what a caller passes. It is a backstop, not the primary
defence, but one that costs a map lookup per attribute.

## The audit trail

Append-only, enforced by the database rather than promised by convention: the application
role is granted INSERT and SELECT on `audit_events` and not UPDATE or DELETE. Nothing in
the `audit` package offers a way to amend or remove a record, and that omission is the
point.

`RecordTx` writes the entry **inside the caller's transaction**, so the trail cannot
disagree with the state it describes. Caller-supplied metadata is stripped of anything
that looks like a credential before it is stored.

## HTTP surface

- **Security headers** on every response: `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, a `Permissions-Policy`
  disabling geolocation, microphone and camera, and
  `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` — strict because
  the API serves JSON, and nothing it returns should ever be executed or framed.
- **CORS is an explicit allowlist.** The `Origin` header is never reflected: reflecting it
  with credentials enabled is equivalent to allowing every origin, which is the common way
  this gets quietly wrong.
- **Rate limiting** is a token bucket keyed by principal where one exists and by client
  address otherwise, so one user's CLI loop cannot exhaust the allowance of everyone
  behind the same NAT. Login is limited far more tightly than the rest of the API.
- **Request bodies** are capped at 1 MB and decoded with `DisallowUnknownFields`, which
  turns a client typo into a 400 naming the field rather than a silently ignored value.
- **Internal errors never reach the client.** `httpx.Internal` logs the cause and returns
  a generic message, because internal error text leaks schema and topology.
- **Identifiers are parsed, not cast.** `ParseID` validates the prefix and the ULID body,
  so a deployment id in a job route fails loudly instead of matching nothing.

## SQL

Every query uses parameter placeholders. There is no string concatenation of user input
anywhere. The one place a query is assembled with `fmt.Sprintf` is the claim statement's
optional project predicate, which is a compile-time constant chosen by a branch, not user
input — and it is commented as such.

## Containers

The API and reconciler run on distroless with a non-root user, a read-only root
filesystem, dropped capabilities and `seccompProfile: RuntimeDefault`.

The worker is Alpine rather than distroless, and that difference is deliberate: its job
is to execute pipeline steps, and steps are shell commands, so an image with no shell
cannot do the work. The trade is accepted knowingly — the worker is the component that
runs the least trusted code, so in a real deployment it belongs in its own namespace with
its own service account and no access to the control plane's secrets, which is how the
Helm chart wires it. `tini` reaps the zombies that spawning build subprocesses creates.

The Kubernetes manifests carry a `NetworkPolicy` restricting database access to control
plane pods, and the namespace enforces `pod-security.kubernetes.io/enforce: restricted`.

## What is not covered

- **No OIDC or SSO.** Local users only. `Principal` is the seam a provider would sit behind.
- **No multi-factor authentication.**
- **No secret encryption at rest** beyond what PostgreSQL and the chosen backend provide.
- **No per-resource ACLs.** Permissions are role-wide, not per project.
- **No CSRF tokens**, because the API is not cookie-authenticated — a bearer token is not
  sent automatically by the browser, so the attack does not apply. Introducing cookie
  sessions would require adding them.
- **No signed or attested container images** in the demonstration path.
- **The rate limiter is per-process**, so N replicas allow N times the rate.
