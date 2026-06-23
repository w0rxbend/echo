# Event Bus Contract

Status: Accepted for v1.

## Context

The current event bus is intentionally minimal. It exists to carry normalized events
from HTTP and integrations to the app worker while preserving simple ordering and
lifecycle behavior. This decision records the contract that current callers and
operators may rely on before new event delivery features are added.

## Decision

The v1 event bus contract accepts the current implementation behavior:

- `Publish` performs sequential blocking fan-out to subscribers.
- Each subscriber observes events in publish order for that subscriber.
- A full subscriber buffer applies publisher backpressure. The publisher waits
  until that subscriber receives, or the publish context is canceled or expires.
- `Publish` holds the bus read lock while it fans out events, including while it
  is blocked sending to a full subscriber channel. `Bus.Close` and unsubscribe
  paths require the bus write lock, so they cannot complete until the blocked
  `Publish` path is released. The only release paths for an already blocked
  publisher are receive-side progress from the full subscriber, or cancellation
  or expiry of the publish context.
- Publish failure is not atomic non-delivery. Earlier subscribers may already
  have received the event before `Publish` returns an error while sending to a
  later subscriber.
- Publisher backpressure metrics remain total-only. The registered families are
  `matrix_proxy_event_publish_backpressure_duration_seconds` and
  `matrix_proxy_event_publish_backpressure_timeouts_total`; they intentionally
  do not carry subscriber labels. The obsolete
  `matrix_proxy_event_publish_backpressure_wait_seconds` family must remain
  absent.
- Subscriber depth observations include terminal zero-depth lifecycle
  observations for unsubscribe, subscription context cancellation, closed
  subscriptions, and bus close. Stale nonzero observations after terminal zero
  remain suppressed.

`OnDepthChange` callbacks are synchronous lifecycle blockers. They may run
during publish, receive, unsubscribe, subscription cleanup, or bus close
observation paths after bus locks are released, and terminal cleanup may wait
for an in-flight callback to return. Arbitrary depth callbacks must therefore
remain fast, in-memory, and nonblocking.

## Constraints

Do not add new subscriber classes, overflow policies, deduplication, reload
observers, or subscriber-attributed metrics on top of this v1 event bus contract
until the contract is revisited or replaced.

Any future design pass must explicitly decide whether to preserve or replace the
blocking fan-out model, partial-delivery error semantics, terminal zero-depth
lifecycle observations, and synchronous depth-callback behavior.
