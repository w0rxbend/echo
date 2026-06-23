[learning] Completing canceled or expired controls should also release queue capacity promptly, not just make the scheduler skip them later.
[learning] Queue removal for canceled controls should use an internal queue identity, not caller-provided item IDs; duplicate IDs can complete or remove the wrong waiter.
[learning] Bounded heartbeat probes prevent indefinite scheduler stalls, but synchronous probes still add queue latency up to the probe timeout.
[learning] Read-only queue snapshots must not expose live pointers or mutable slices; otherwise callers can mutate queued work after scheduler admission.
[learning] Defining a terminal outcome is not enough if nothing observes it; queue-cleared animations need metrics/logs or outcome hooks, not only a local error value.
[learning] Broad lifecycle outcome enums should be wired at every terminal path or named narrowly; partial emission creates false observability confidence.
[learning] Scheduler shutdown outcome reporting must account for the current in-flight item as well as queued items; queued-only shutdown metrics silently undercount interrupted playback.
[learning] Asynchronous outcome observers protect scheduler latency, but one goroutine per report turns blocked observers into resource leaks unless dispatch is bounded or explicitly best-effort.
[learning] Bounded observer queues prevent resource leaks but make outcome metrics/logging lossy unless reliable internal sinks are separated from best-effort observers.
[anti-pattern] Async outcome tests should wait for the specific reports they assert; waiting on total report count is flaky when unrelated outcomes can arrive first.
[learning] Scheduler run-context cancellation and item/request cancellation need separate terminal labels; otherwise shutdown is misreported as caller cancellation.
[learning] Closing an observer dispatcher can stop admission and unblock scheduler shutdown, but it cannot preempt observer code already running.
[anti-pattern] Terminal outcome paths should not be split across guarded and unguarded reporters; queue-clear, execution, and failure paths need one exactly-once reporting discipline.
[learning] Reliable outcome sinks protect metrics from best-effort observer drops, but synchronous callbacks become scheduler critical-path code and must stay fast and nonblocking.
[learning] Scheduler dispatcher lifecycle needs both run-owned shutdown and explicit close for constructed-but-never-run owners; app-level lifecycle should expose that cleanup before reload work.
[pattern] Lifecycle leak tests should wait on explicit owner-visible completion signals instead of parsing goroutine profiles; stack dumps are brittle under unrelated runtime changes.
[learning] App constructors that allocate goroutines before later validation must roll back on error; `Close` cannot help when no app value is returned.
[learning] App lifecycle must define post-stop restart semantics; if worker shutdown closes scheduler-owned observers, a later run can silently degrade observability unless the app is one-shot or resources are reinitialized.
[pattern] Process-level `App.Run` and embedded `RunWorkers` should share one lifecycle admission gate before side effects; reserve the one-shot slot before binding HTTP.
[learning] Reconnect observability must include both scheduler backoff loops and TCP-client immediate reconnects; instrumenting only delayed retries undercounts real recoveries.
[learning] Matrix command metrics should distinguish TCP frame attempts from logical scheduler commands because retry success can emit both failure and success samples for one user action.
[pattern] Prefer fake resource wrappers/adapters for lifecycle failure tests over production-only hooks; they exercise real close paths with less test-shaped production code.
[learning] Reconnect recovery should mean firmware-verified connectivity, not just a TCP dial or successful retried logical command; permanent retry-command errors belong in command/outcome telemetry.
[learning] Immediate reconnect attempts need a terminal recovery or failure sample; attempts that disappear after verification errors leave dashboards ambiguous.
[learning] Matrix observability panic counters need bounded source attribution when scheduler and TCP client share callback names, or operators cannot localize failed telemetry hooks.
[learning] Retry-ping firmware/protocol failures are reconnect verification failures, not trustworthy command failures; close the replacement socket and emit a terminal verification outcome.
[pattern] Best-effort telemetry dispatchers should count drops; nonblocking behavior is only operator-safe when lost logs or observer reports are visible.
[learning] Event queue depth based on subscriber buffer length excludes the event currently being processed; add an in-flight signal if stuck-worker visibility matters.
[pattern] Freeze metric compatibility with help-text and label-set tests when dashboards depend on nuanced semantics such as reconnect recovery versus retried-command failure.
[learning] Subscriber backlog gauges need receive-side observations; publish-side depth callbacks alone cannot report events removed from the channel.
[pattern] Event depth callbacks need lifecycle gating: unsubscribe, close, and canceled subscriptions emit one terminal zero-depth observation, then suppress stale publish or receive observations so removed subscribers cannot be reported nonzero.
[anti-pattern] Blocking event fan-out while holding the bus read lock means unsubscribe or close cannot free a full subscriber; only receiver progress or publisher context cancellation unblocks the send.
[learning] With `queue.overflow_policy: block`, publisher backpressure is intentional until other overflow policies exist; track it through total publisher wait and timeout/drop metrics instead of adding unbounded subscriber labels.
[learning] Metric names in README/config must be checked against registered Prometheus families; otherwise docs can promise names that tests and dashboards cannot scrape.
[learning] Guaranteeing no bus-owned depth callbacks after unsubscribe returns can make terminal cleanup wait behind an in-flight callback, so depth observers must stay fast or move behind owned dispatch.
[learning] Sequential blocking fan-out can partially deliver before returning a publish context error; callers must not interpret publish failure as atomic non-delivery.
[pattern] Contract guardrail tests should cover every documented release path for each lifecycle operation; asymmetric coverage lets docs outrun executable guarantees.
[learning] Desired-state background convergence needs explicit dirty/converged health; `last_failure` alone can leave readiness green while idle state is wrong.
[learning] Background retry backoff needs operator-visible next-attempt/failure-count data; dirty/failed state alone does not explain why restoration is quiet.
[learning] Background retry state must account for both dirty triggers and retry-deadline timing; stale `retrying` after a due deadline is as misleading as `dirty` during a pending retry.
[learning] Public background convergence is safer when a single projection function feeds scheduler health, app readiness, and Prometheus state gauges.
[learning] Due-retry edge behavior should be explicitly codified in one bounded contract to prevent contradictory signals between `/readyz.background` and metric one-hot state.
[pattern] Desired-background duplicate suppression should require explicit state identity, such as a configured background ID or exact preset parameters; pixel-equivalent output alone is not a safe convergence proof.
[learning] Generic event endpoints should validate known override keys at ingress while preserving unknown attributes as schema-agnostic data; otherwise bad intents fail asynchronously.
[anti-pattern] Scheduler concurrency tests should not assert exact queue-depth transition sequences unless item selection is explicitly blocked; assert final behavior or controlled synchronization instead.
[pattern] Keep one bounded projection function as the single source for background convergence public state across scheduler health, readiness, and metrics.
[learning] If an API contract document changes field names (`generated`/`renderable`), every user-facing surface and test surface should be updated before release.
[learning] Catalog contracts can stay future-proof by requiring only stable semantics while allowing bounded additive metadata fields, with explicit contract tests for both forms.
[anti-pattern] Regression tests should avoid exact queue-depth ordering assumptions when scheduler preemption can legitimately interleave item admission and removal.
[anti-pattern] Returning internal contract-bearing structs directly from handlers can accidentally broaden API shape; enforce an explicit API-facing DTO or schema gate so additive fields stay intentional.
