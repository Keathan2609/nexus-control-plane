# 6. Realtime updates use SSE, not WebSockets

**Status:** Accepted

## Context

The console needs to reflect pipeline and deployment state without polling.

## Decision

Server-Sent Events, over the existing HTTP API.

## Consequences

The traffic is entirely one-directional — the console renders state, it does not push it
— so the bidirectional capability of a WebSocket would buy nothing.

SSE is plain HTTP, which means it passes through the same authentication middleware, the
same proxies, the same rate limiter and the same request logging as every other endpoint,
and it reconnects on its own. A WebSocket would be a second protocol to secure, observe
and operate.

Three costs, all handled:

`EventSource` cannot set request headers, so the stream accepts its token as a query
parameter. That is scoped to this one route by an explicit path check, and the request
logger records `URL.Path` and never `RawQuery`, so tokens do not reach the logs.

Buffering intermediaries turn a live stream into a long silence followed by a burst, so
the response sets `X-Accel-Buffering: no`, and a comment frame is sent every twenty
seconds to keep idle connections open through proxies that close silent ones.

A wrapped `ResponseWriter` silently swallows `Flush` unless it forwards it, which would
buffer every event until the connection closed. The logging middleware's recorder
implements `http.Flusher` for exactly this reason.

Subscriber channels are buffered and non-blocking: a slow client's events are dropped
rather than every other client being held up. That is only acceptable because Postgres,
not the channel, holds the truth — a dropped notification costs a moment of staleness,
and the console refetches on reconnect.

## Alternatives considered

**WebSockets.** Justified if the console ever needs to send high-frequency messages
upstream. It does not; actions are ordinary REST calls.

**Polling.** Simple, and still the fallback the console degrades to when the stream is
unavailable. Rejected as the primary mechanism because a run whose steps take a second
each would look sluggish at any poll interval cheap enough to run continuously.
