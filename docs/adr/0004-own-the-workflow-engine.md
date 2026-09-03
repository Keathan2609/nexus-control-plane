# 4. NEXUS owns its workflow engine

**Status:** Accepted

## Context

Temporal, Argo Workflows and GitHub Actions all execute DAGs with retries and durable
state. Building another one is, on its face, unjustifiable.

## Decision

NEXUS implements its own: state machines, DAG planning, claim-based dispatch, leases,
retry policy and reconciliation.

## Consequences

This is the decision the whole project rests on, and it is worth being honest about why.

**For a product, this would be the wrong call.** Temporal is better at durable execution
than anything built in a few weeks, and adopting it would be the correct engineering
decision.

**For this system, the engine is the point.** NEXUS exists to demonstrate control-plane
engineering — concurrency under contention, failure recovery without a supervisor,
transactional consistency between state and events, level-triggered reconciliation.
Delegating exactly those concerns to a library would leave a project that configures
someone else's control plane.

There is also a genuine technical argument. A general workflow engine does not know what
a deployment is. It cannot express "a deployment is healthy only after three consecutive
probes", "one deployment is live per environment", or "a rollback is a deployment of an
earlier release" as invariants — those become application code layered on top, with the
engine's state and the domain's state as two things to keep in agreement. Owning both
collapses that into one, which is why the deployment lifecycle can be enforced by a
transition table and a partial unique index rather than by care.

The cost is real: no cron, no dynamic fan-out over collections, no cross-run
dependencies, no priority ageing, and every reliability property has to be built and
tested rather than assumed.

## Alternatives considered

**Temporal.** The right answer for a product. Rejected because it would move the
interesting engineering out of the project.

**Argo Workflows.** Ties execution to Kubernetes, which would make the local development
path require a cluster — and the local path is what makes this runnable at all.

**GitHub Actions as the executor.** Makes NEXUS a dashboard over someone else's runner,
with no ownership of retries, leases or recovery. It remains a supported integration
point rather than the engine.
