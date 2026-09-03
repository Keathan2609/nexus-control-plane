# 7. List endpoints page by keyset, not offset

**Status:** Accepted

## Context

Runs, deployments and audit entries are continuously appended and always listed
newest-first.

## Decision

Every list endpoint pages by cursor — the id of the last row seen — rather than by
`OFFSET`.

## Consequences

Offset paging over a table that is actively being written to skips and repeats rows as
new entries shift the window. For a run list that gains entries continuously, that is not
an edge case but the normal experience: page 2 shows rows the user already saw on page 1.

Identifiers are prefixed ULIDs, so `WHERE id < $cursor ORDER BY id DESC` is both correct
and index-friendly. The primary key already provides the ordering, with no secondary
index on `created_at` and no sort step.

This is also why the ULID generator uses a monotonic entropy source. Within a single
millisecond, purely random ULIDs order arbitrarily, so `ORDER BY id` would silently stop
meaning "in creation order" precisely when the system is busiest. A test asserts that
property, and it caught exactly that bug during development — the first implementation
used `crypto/rand` directly and failed it.

The same reasoning shaped the demonstration seeder: it writes historical runs oldest-first
across all projects rather than project by project, so insertion order matches
chronological order and the dashboard interleaves as it would in a live system.

The cost is that pages cannot be jumped to — there is no "page 7" — and total counts are
not returned. Neither is needed: these are activity feeds, not catalogues. `has_more` is
derived by fetching one row beyond the page size, which avoids a second `COUNT` query.

## Alternatives considered

**Offset with a stable snapshot.** Would need a consistent read across requests, which
means holding a transaction open or materialising the result — both disproportionate for
an activity feed.

**Timestamp cursors.** Ambiguous when two rows share a millisecond, so they need a
tiebreak column anyway. The ULID already is one.
