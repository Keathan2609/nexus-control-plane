# Security policy

## Reporting a vulnerability

Open a private security advisory on the repository rather than a public issue.

## Scope

NEXUS is a portfolio project. It is not operated as a hosted service, and the
demonstration credentials in `cmd/nexus-seed` are intentionally public and intentionally
weak — they exist so the system can be opened and used.

What is in scope is the code: authentication, authorisation, webhook verification, SQL
handling, secret management and the container images.

## What the project does

See [docs/security.md](docs/security.md) for the boundaries it enforces, and the section
at the end of that document for the ones it deliberately does not.

## Dependencies

`govulncheck` runs over the Go module and `npm audit` over the console on every CI run,
and Dependabot proposes updates weekly across gomod, npm, GitHub Actions and Docker.
