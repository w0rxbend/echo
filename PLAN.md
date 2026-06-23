# LED Matrix Proxy Server Plan

## Goal

Build a Go proxy server that owns the single long-lived TCP connection to the ESP8266 LED matrix controller, accepts events from HTTP and future integrations, maps them to animation intents, schedules them serially, and drives the matrix through the firmware binary protocol.

The durable architectural boundary remains:

```text
Integrations / HTTP
  -> normalized events
  -> rules and animation registry
  -> matrix play queue
  -> scheduler
  -> single matrix TCP client
  -> ESP8266 firmware
```

Integrations must never talk directly to the TCP client. Direct administrative matrix operations must also be coordinated through the scheduler so they cannot corrupt streamed animation timing.

## Firmware Contract

Keep these constraints central in every matrix-facing change:

- TCP server port is `7777`.
- Firmware expects one active client.
- Command frames are binary: `LM`, version `0x01`, command byte, payload length `0..255`, payload, XOR checksum over prior bytes.
- Responses are exactly 6 bytes: `LM`, version `0x01`, response command `0x80`, status, XOR checksum.
- The client must read exactly one response after every command and validate magic, version, response command, checksum, and status.
- Use one command in flight per connection.
- Enable `TCP_NODELAY`.
- Full frame payloads are 192 bytes; custom frame uploads are 196 bytes and must remain allowed.
- `SetFullFrame` and `UploadCustomFrame` take physical chain order. App code must draw in display-space and pack through the layout mapper.

Default display-space layout remains `h-tl` with odd-row display compensation:

```text
display_to_server_point(x, y):
  if y % 2 == 1: return 7 - x, y
  return x, y

physical_index(server_x, y):
  if y % 2 == 0: return y*8 + server_x
  return y*8 + (7 - server_x)
```

## Current Implementation Status

Completed through iteration 17 review:

- Go service skeleton, config loading, structured logging, app lifecycle, `/healthz`, `/readyz`, and `/metrics`.
- Strict ESP8266 TCP protocol client with command serialization, response validation, `TCP_NODELAY`, retryable transport classification, immediate socket-error reconnect, and fake TCP contract tests.
- Display-space canvas/layout packing, built-in generated notification animation, animation registry, rules loader, event bus, and HTTP notify/play/event/control APIs.
- Scheduler-owned queue and matrix control boundary. HTTP matrix controls enqueue scheduler controls instead of calling the TCP client directly.
- Admin auth fails closed for non-local binds; queue inspection and admin controls are protected when non-local auth is required.
- Queue mutation is scheduler-owned; queue snapshots are immutable status DTOs and do not expose live control pointers, frame slices, or hooks.
- Scheduler control and animation lifecycles have terminal outcome reporting for executed, canceled, expired, dropped, queue-cleared, scheduler-stopped, and permanent-error paths.
- Reconnect uses exponential backoff with min/max delays, injectable jitter, deadline caps, and deterministic tests.
- Heartbeat probes use explicit `matrix.heartbeat_interval` and bounded `matrix.probe_timeout`. The current latency contract is documented: probes run on the scheduler selection path, so newly queued work can wait up to `probe_timeout` behind an in-progress idle probe.
- Terminal outcome reporting is centralized through guarded scheduler helpers. App play-item metrics are recorded through `matrix.NewSchedulerWithReliableAppOutcomeRecorder` before best-effort observer dispatch.
- Panics from the reliable outcome recorder are recovered, counted separately from observer drops, exposed through scheduler health, surfaced in `/readyz`, and exported as `matrix_proxy_play_item_outcome_recording_panics_total`.
- Outcome observers remain best-effort through a bounded nonblocking dispatcher. Dropped observer reports are counted and the dispatcher can be closed explicitly.
- App construction rolls back partially constructed resources on late failure. Public app construction is fixed-shape: `New(cfg, logger)` has no exported constructor option parameter.
- `App.Close` is terminal for constructed-but-never-run apps and apps whose workers have already stopped. `App.Shutdown(ctx)` coordinates active worker cancellation and terminal resource cleanup. App instances are explicitly one-shot after any worker run.
- `App.Run(ctx)` uses the shared lifecycle admission gate before opening the HTTP listener, then performs terminal cleanup through `Shutdown(context.Background())` after its HTTP server and worker errgroup exits.
- Matrix command metrics report TCP command frame attempts through `matrix_proxy_matrix_commands_total{command,status}` and `matrix_proxy_matrix_command_duration_seconds{command}`. The help text explicitly says these are TCP attempts, not logical scheduler commands.
- Scheduler reconnect attempts/delays/recoveries/failures, heartbeat/probe failures, matrix connected transitions, TCP immediate reconnects, TCP reconnect log drops, and matrix observability callback panics are wired to app metrics/readiness/logs.
- TCP-client immediate reconnect reports recovery only after firmware-verified replacement connectivity. Retry-ping firmware/protocol/validation failures are terminal `verification_failed` reconnect failures and close the suspect replacement socket.
- TCP reconnect structured logging is moved out of the TCP mutex-held callback path through a bounded app-owned dispatcher. The mutex-held callback keeps only fast in-memory metrics plus nonblocking log enqueue.
- `matrix_proxy_event_queue_depth` reports only the buffered backlog in the app event-worker subscriber channel, and `matrix_proxy_event_worker_inflight` reports whether the app event worker is actively processing one received event.
- `/readyz` now includes bounded event-worker diagnostics: state (`idle` or `processing`), processing stage (`receive`, `map`, `enqueue`, `log_drop`), and active duration seconds while processing.
- The v1 event bus contract is accepted and documented in `docs/event-bus-contract.md`: sequential blocking fan-out under the bus read lock, per-subscriber ordering, publisher backpressure behind any full subscriber, visible partial fan-out on publish errors, synchronous lifecycle-blocking depth callbacks, and terminal zero-depth lifecycle observations.
- Event-bus close/unsubscribe guardrails are executable in `internal/events`: a blocked `Publish` prevents concurrent unsubscribe and `Bus.Close` from completing until subscriber receive or publish context cancellation/expiry releases the publish path.
- Event publisher backpressure is exposed through total-only metrics with the final family names: `matrix_proxy_event_publish_backpressure_duration_seconds` and `matrix_proxy_event_publish_backpressure_timeouts_total`. Tests assert the obsolete `matrix_proxy_event_publish_backpressure_wait_seconds` family is absent.
- Runtime config supports `animations_file` loading. Configured animation files currently support `type: generated` with generator `notification`, and `type: firmware_preset` with effect ID, interval, and color.
- Configured animations are merged with built-ins during `config.Load`; unknown animation types, unknown generated animation generators, duplicate IDs, invalid firmware-preset bounds, unknown rule animation references, and unknown enabled background references fail startup.
- `matrix_rain_background` is now represented by `configs/animations.example.yaml` as a firmware preset and referenced from `configs/config.example.yaml` background config instead of being hardcoded in app construction.
- Scheduler background restore resolves the configured background ID from the merged registry. Firmware-preset backgrounds restore by scheduler-owned `SetPreset`; renderable generated backgrounds restore by rendering frames through the registry and layout packer.
- The animation registry distinguishes renderable generated animations from metadata-only firmware presets through `IsRenderable`, `Entry`, and `RenderableIDs`.
- Rule validation rejects non-renderable firmware-preset IDs for ordinary `play.animation` references, while preserving firmware presets for configured background restore.
- `/api/v1/animations` advertises only renderable/playable animation IDs. `/api/v1/play` and `/notify` animation overrides reject unknown and non-renderable IDs before scheduling or publishing.
- Scheduler playback has a defensive non-renderable guard: ordinary animation requests for firmware-preset IDs fail with `ErrNonRenderableAnimation` and never send preset commands through the playback path.
- `background.restore_on_idle` now makes the configured background scheduler-owned desired matrix state. When enabled, the scheduler applies the configured background after the first firmware-verified connection, keeps it out of the ordinary playback queue, and marks it dirty for restore after scheduler backoff reconnects and TCP-client immediate reconnect recoveries.
- Background restore remains scheduler-owned for both firmware presets and renderable generated animations. Firmware-preset backgrounds send `SetPreset`; renderable backgrounds are rendered to finite frames, packed through the layout mapper, and streamed without becoming infinite queue items.
- Desired-background convergence is now explicit for transient playback restore policies and direct display controls. `restore: leave`, `restore: previous_frame`, `restore: clear`, `restore: blank`, and successful fill/clear/preset controls affect only immediate display state; the configured background owns the eventual idle display when `restore_on_idle` is enabled.
- Desired-background convergence is exposed as an operator contract. `/readyz.background` reports the configured ID, background kind (`firmware_preset` or `renderable`), convergence state (`unknown`, `dirty`, `attempting`, `converged`, `failed`, or `retrying`), dirty/converged booleans, restore attempt/success timestamps, last restore error text, and bounded last restore error class (`none`, `retryable`, or `permanent`).
- Background non-convergence is visible but does not currently drive the top-level readiness decision. `/readyz` still returns ready when workers are running, the app is not draining, and matrix connectivity is healthy, even if the configured background is dirty, attempting, failed, or retrying.
- Background restore telemetry is separate from playback telemetry. Restore attempts increment `matrix_proxy_background_restore_attempts_total{kind}`; restore failures increment `matrix_proxy_background_restore_failures_total{kind,error_class}`. Scheduler-owned background restore does not emit play-item outcomes and is not counted by `matrix_proxy_play_items_total`.
- Background restore attempt/failure logs include the configured background ID, kind, state transition, bounded error class, and error text. Failure logs make clear that dirty desired backgrounds remain scheduled for retry.
- Manual hardware validation docs now explain that fill/clear/preset checks will be overwritten by the configured background unless operators temporarily disable `restore_on_idle` or set the desired validation state as the configured background.
- Fake-ESP black-box coverage verifies configured `matrix_rain_background` startup application, reconnect restoration, post-notification restore payloads, direct-control convergence, and generated/renderable background frame streaming end to end through the TCP protocol.
- Scheduler coverage verifies renderable background startup/reconnect render failures, retryable firmware-preset `SetPreset` failures, retryable renderable `SetFullFrame` failures, and permanent firmware/protocol/validation failures keep the background dirty, update convergence health, and avoid ordinary playback queue pollution.
- Fake-ESP black-box coverage verifies background failure and recovery visibility through `/readyz` and Prometheus for firmware-preset and renderable background restore paths, while preserving separation from `matrix_proxy_play_items_total`.
- Background convergence failures now use a background-specific retry controller instead of retrying on every heartbeat. Retryable failures back off from `1s` to `30s`; permanent failures back off from `30s` to `5m`. While a retry deadline is pending, idle heartbeat passes and `restore: background` playback policies keep the background dirty but do not force another restore attempt.
- Background retry state resets after a successful background restore. Later verified reconnect recovery or successful direct display control can also mark the background dirty with a prompt retry opportunity, but failed restore attempts still keep dirty-state correctness and never mark clean until the configured background is actually applied.
- The background retry policy is now documented and tested as intentionally fixed v1 behavior: retryable failures back off from `1s` to `30s`, and permanent failures back off from `30s` to `5m` while retrying forever with capped backoff.
- `/readyz.background` exposes retry controller state through `next_retry` and `failure_count`, in addition to the existing convergence fields.
- Prometheus now exposes current desired-background state through bounded gauges: `matrix_proxy_background_dirty{kind}`, `matrix_proxy_background_converged{kind}`, `matrix_proxy_background_next_retry_seconds{kind}`, and one-hot `matrix_proxy_background_state{kind,state}`. Background IDs are intentionally not metric labels.
- Scheduler coverage verifies background retry suppression before the retry deadline, retry after deadline, permanent-failure retry delay, prompt retry after verified reconnect recovery, and full replay from frame zero after partial renderable background streaming failure.
- Fake-ESP black-box coverage verifies current-state background gauges, partial generated-background stream failure visibility, and full generated-background replay after recovery while preserving separation from `matrix_proxy_play_items_total`.
- README and example config comments document the fixed v1 background retry contract, readiness retry fields, current-state gauges, and no-config-knob policy.
- Background pending-retry state semantics are now frozen for v1. While `next_retry` is still in the future, later dirty triggers such as `restore: background` playback, disconnect while retrying, and redundant dirty marks keep the background state at `retrying` rather than downgrading it to plain `dirty`.
- Verified reconnect recovery and successful direct display controls intentionally reset pending background retry state so the scheduler can make one prompt desired-background restore attempt when idle. Successful background restore remains the only path that marks the configured background clean.
- `/readyz.background` and `matrix_proxy_background_state{kind,state}` use the same pending-retry invariant: when a dirty background has a future `next_retry`, the public state is `retrying`, not `dirty`. README and config comments document that `restore: background` before `next_retry` cannot bypass the deadline or change state away from `retrying`.
- Iteration 17 adds a versioned due-retry projection contract: internal scheduler background state is first projected by `ProjectBackgroundConvergence` and consumed consistently by `/readyz.background` and background metrics, with new regression coverage for due-but-unattempted, long-playback, and disconnected edges.

Verified after iteration 39 review:

- `go test ./...` passes.
- `go vet ./...` passes.
- `go test -race ./...` passes.
- `go test -race ./internal/matrix ./internal/app ./internal/metrics -run 'TestScheduler.*Background|TestReadyAndMetricsExpose.*Background|TestBackgroundStateGauges|TestBackgroundRestoreMetrics|TestBackgroundRetryBoundsAreFixedV1Contract' -count=5` passes.
- The repository checkout still lacks `.git`, so exact `git diff`/`git show` inspection was unavailable. Review used file mtimes, direct source inspection, repository-wide search, and validation commands.

## Current Gaps And Defects

High severity:

- No high-severity runtime regression was found in the current iteration.

Medium severity:

- Background retry bounds are now intentionally fixed v1 behavior. That keeps the contract simple, but operators still cannot tune retry cadence, disable permanent retries, or set a maximum attempt/suppression policy if hardware behavior proves the defaults noisy.
- Background retry projection is now explicit for pending/due edges with one shared projection used by scheduler health, `/readyz.background`, and background gauges.
- Remaining contract decision: finalize whether `failed` remains user-facing in v1, or becomes reserved, and keep compatibility snapshots aligned.
- Prometheus exposes `next_retry_seconds` and one-hot state, but it does not expose retry `failure_count`. Operators can see delay and state, but not whether a dirty background is on its first or later retry without polling `/readyz`.
- The retry reset contract is documented, but it remains subtle: a retryable restore command's own scheduler reconnect still records the restore attempt as failed and schedules the next try by background backoff, while a later verified reconnect recovery or successful direct display control can reset the retry delay for a prompt attempt.
- `/api/v1/events` can still carry `attributes.animation` overrides that are applied by the app event worker after publish. Unknown or non-renderable override IDs are rejected asynchronously by scheduler enqueue, not promptly at the HTTP event boundary.
- Declarative animation loading supports generated aliases and firmware presets only. There is still no declarative frame/pixel-art animation support.
- The HTTP animation catalog remains a flat backward-compatible list of playable IDs. There is no structured API for operators to inspect background-only firmware preset metadata, kind, or playability.
- `matrix_proxy_event_worker_inflight` plus `/readyz` active stage/duration improves diagnosis, but Prometheus remains intentionally low-cardinality and does not expose active age or stage labels.
- By accepted v1 contract, event-bus depth callbacks are synchronous lifecycle blockers. A slow callback can block unsubscribe, subscription context cleanup, or `Bus.Close`.
- By accepted v1 contract, the event bus uses sequential blocking fan-out under a bus read lock. A full subscriber can backpressure HTTP publishers, and `Close`/unsubscribe cannot remove that subscriber while a publisher is blocked on its channel.
- Event publisher backpressure metrics are intentionally total-only for v1. Operators can see aggregate wait duration and timeout drops, but not which subscriber class caused them.
- Heartbeat probes remain synchronous on the scheduler item-selection loop by documented contract. New queued work can wait up to `probe_timeout` behind an in-progress idle probe.
- TCP-client callbacks still execute Prometheus metric updates while the TCP client mutex is held. Logging is only enqueued on that path, but metric recording remains command-serialization critical-path work.
- TCP reconnect log drops are counted only as a total. Operators can see that reconnect log events were lost, but not which callback type or reason caused the loss.
- App readiness aggregates matrix observability callback panic counts by callback name only. Prometheus has bounded `source` labels, but `/readyz` remains compact rather than source-attributed.
- `InterruptMode` is mapped but ignored; there is no `higher_priority` or `critical` interruption behavior.
- Non-blocking event overflow and deduplication are rejected by config pending a fresh event bus design pass.
- `/api/v1/admin/reload` is planned but absent.

Low severity:

- `NewSchedulerWithReliableAppOutcomeRecorder` is narrower than the old scheduler option, but it is still callable by any `internal/...` package. Keep watching for misuse.
- Production app code still contains a package-private constructor option path whose only current use is reliable-sink panic/blocking injection for tests.
- Active animation deadlines are not enforced during a frame-delay sleep; expiration is checked before the next frame, so a long frame delay can overshoot an item deadline unless frame durations are clipped.
- Scheduler deadline/backoff code still mixes injected `Now` with real `time.Until`, `time.AfterFunc`, and real timers.
- Startup retry coverage still uses a fake matrix client rather than a TCP-level fake ESP8266 or dialer seam.
- Request validation remains incomplete for enum-like fields, play interrupt mode, animation IDs on generic `/events` overrides, preset effect semantics, and duration bounds.
- `/api/v1/events` and `/notify` are intentionally public for now, but there is no source-specific rate limiting or admission policy.

## Next Iteration Priorities

### Phase 1: Background Retry State Edge Precision

1. Preserve the desired-background convergence operator contract. (status: complete)
   - Keep `/readyz.background` fields stable: configured ID, kind, state, dirty/converged booleans, restore attempt/success timestamps, last error, and bounded last error class.
   - Keep the new retry fields stable: `next_retry` and `failure_count`.
   - Keep state values bounded to `unknown`, `dirty`, `attempting`, `converged`, `failed`, and `retrying`.
   - Keep top-level readiness based on workers, draining state, and matrix connectivity unless there is a deliberate policy change to make background non-convergence return HTTP 503.

2. Finalize projection semantics for due-retry transitions. (status: in-progress)
   - Keep transition policy explicit: `failed` when dirty/no suppression exists and restore is due/past but not yet attempted; `retrying` when a future suppression is active.
   - Add explicit transition tests across `failed <-> retrying <-> attempting` with long playback, disconnect, and loop-delay due windows.
   - Prove scheduler state, `/readyz.background`, and `matrix_proxy_background_state` remain coherent in all due-retry edge paths.

3. Decide whether `failed` should stay executable in the v1 state vocabulary. (status: in-progress)
   - If kept executable, publish a final contract and transition matrix, then freeze with compatibility checks.
   - If reserved/removed, remove it from `/readyz.background` and gauges before rollout and replace with bounded polling or gauge-based alternatives.
   - Keep the state vocabulary fixed once the decision is made; do not extend it in place.

4. Keep retry metrics bounded and dashboard-friendly.
   - Keep `matrix_proxy_background_dirty{kind}`, `matrix_proxy_background_converged{kind}`, `matrix_proxy_background_next_retry_seconds{kind}`, and `matrix_proxy_background_state{kind,state}` stable.
   - Decide whether a bounded `matrix_proxy_background_retry_failures{kind}` gauge is useful; do not add it unless dashboards need failure count without polling `/readyz`.
   - Keep background IDs out of metric labels unless a deliberate cardinality policy exists.

5. Preserve the fixed v1 retry policy unless evidence requires knobs.
   - Treat retryable `1s..30s` and permanent `30s..5m` capped forever retry as the current v1 contract.
   - If hardware validation shows noisy logs/commands, add config fields with defaults matching the fixed v1 behavior and validate min/max ordering.
   - Preserve dirty-state correctness: failed desired backgrounds must never be marked clean until the configured background is actually applied.

6. Keep background restore observability separate from playback.
   - Keep `matrix_proxy_background_restore_attempts_total{kind}` and `matrix_proxy_background_restore_failures_total{kind,error_class}` as the background restore metric families.
   - Keep background restore logs explicit about background ID, kind, state transition, bounded error class, error text, and retry of dirty desired backgrounds.
   - Keep scheduler-owned background restore outside ordinary playback outcomes so it does not appear in `matrix_proxy_play_items_total`.

7. Reduce redundant background commands where it is safe.
   - `restore: previous_frame` currently restores the pre-item background and then idle convergence can send the same background command again. Consider marking the desired background clean when the immediate restore exactly matches the configured background.
   - Preserve correctness first; only optimize duplicate preset/frame restores when equality is explicit and cheap to prove.

### Phase 2: Playback And HTTP Validation Completeness

1. Validate generic event animation overrides consistently.
   - If `/api/v1/events` accepts `attributes.animation` as a first-class override, reject unknown and non-renderable IDs at the HTTP boundary just like `/notify`.
   - If generic events should remain schema-agnostic, document that override validation happens asynchronously in the app worker and add tests for the drop/metric/log behavior.
   - Add app or HTTP tests proving firmware-preset IDs cannot slip into playback from event attributes without an observable drop.

2. Decide whether operators need structured animation registry discovery.
   - Keep `GET /api/v1/animations` as a flat playable-ID list for backward compatibility.
   - If background/preset discovery is useful, add a new structured endpoint or optional response shape with `id`, `kind`, and `playable` without re-advertising presets as directly playable.
   - Include tests proving metadata-only firmware presets stay non-playable while visible through any new catalog path.

3. Broaden HTTP request validation around the playback contract.
   - Validate restore, interrupt mode, duration bounds, preset effect semantics, and unknown fields consistently for `/events`, `/notify`, `/play`, and direct controls.
   - Preserve the scheduler guardrail so internal callers still cannot enqueue non-renderable playback even if an HTTP validation path is missed.

### Phase 3: Declarative Animation Expansion

1. Add declarative frame animations now that the playable/preset split is fixed.
   - Parse palette and 8-row pixel art in display-space.
   - Add brightness and simple transforms needed for v1 examples.
   - Keep all config-authored frames in display-space and pack only through the layout mapper.

2. Tighten loader validation and docs.
   - Reject empty animation files or empty `animations:` maps only if that is operationally useful; otherwise document that they simply add no entries.
   - Decide whether relative `animations_file` and `rules_file` should resolve strictly relative to the config file instead of checking the process working directory first.
   - Add documentation for the supported animation file schema and clear examples for generated, firmware preset, and future frame animations.

### Phase 4: Event Delivery Boundary

1. Preserve the accepted v1 event bus contract until a deliberate redesign is planned.
   - Do not add diagnostic subscribers, reload observers, non-block overflow policies, or deduplication on top of the current blocking fan-out model.
   - Treat publish errors as partial-delivery results, not atomic non-delivery.
   - Keep backpressure metrics total-only unless there is a concrete operational need for bounded subscriber attribution.

2. If event delivery is redesigned, design subscriber isolation before implementation.
   - Define whether each subscriber owns an independent delivery queue/goroutine and how per-subscriber ordering is preserved.
   - Define `Close`/unsubscribe release semantics for blocked publishers and how terminal zero-depth observations remain ordered.
   - Define publisher backpressure/drop metrics, timeout semantics, partial-delivery reporting, and subscriber attribution before enabling new overflow policies.

### Phase 5: TCP, Scheduler Latency, And Lifecycle Stability

1. Keep TCP callback critical paths bounded.
   - Keep mutex-held TCP callbacks limited to Prometheus in-memory updates and nonblocking log enqueue.
   - Move command metrics off the mutex-held path only if profiling shows Prometheus contention or command latency impact.
   - Preserve the immediate reconnect terminal contract: recovery means firmware-verified replacement connectivity; retried-command failures after that belong to command telemetry.

2. Decide whether TCP reconnect log-drop diagnostics need labels.
   - Keep the current total drop counter if "any reconnect log loss" is sufficient.
   - If diagnosis needs precision, add bounded labels or separate counters for callback (`reconnect_attempt`, `reconnect_recovered`, `reconnect_failure`) and drop reason (`queue_full`, `closed`, `closing`).

3. Decide whether synchronous heartbeat probe latency is acceptable long-term.
   - The current contract bounds queued-work delay by `probe_timeout`.
   - If unacceptable, move probes off the item-selection path without violating one command in flight.
   - Keep timeout/probe-failure metrics stable if the implementation changes.

### Phase 6: Interrupts And HTTP API Completeness

1. Implement interrupt semantics.
   - Support `none`, `higher_priority`, and `critical`.
   - Decide whether interrupted lower-priority items are dropped, paused, or requeued; make behavior explicit in config and tests.
   - Add tests for no-interrupt, higher-priority interrupt, critical interrupt, and FIFO preservation after interruption.

2. Add `/api/v1/admin/reload`.
   - Validate new config/rules/animations before applying.
   - Construct a fresh app instance instead of restarting a stopped instance in place.
   - Use the coordinated app shutdown path after a successful swap.
   - Do not add reload event observers without a fresh event bus design pass.

3. Broaden HTTP request validation and tests.
   - Cover `/events`, `/play`, queue list/clear, animations list, direct control authorization, invalid JSON, unknown fields, invalid duration, unknown/non-renderable animation, queue full, control cancellation, and immediate invalid control rejection.
   - Validate restore, interrupt, preset effect IDs, duration bounds, and direct command payloads consistently.

## Testing Plan

Keep these tests in place and expand them as features land:

- Protocol builder checksum and payload limits, including 196-byte custom frame uploads.
- Response parser validation and typed status mapping.
- Fake TCP matrix server with strict frame validation, firmware status injection, dropped responses, one-response-per-command behavior, and pipelining detection.
- Reconnect after socket close, reconnect recovery/failure labeling, stale-ping immediate reconnect labeling, retry-ping verification failure, and no reconnect after protocol/status errors.
- Error taxonomy tests for permanent validation/status/protocol errors and retryable transport/dial failures.
- TCP-client command metric callback tests for OK, firmware status, protocol, transport, cancellation, ping, retry-success, callback panic, and callback critical-path blocking.
- Display-space orientation tests using asymmetric fixtures.
- Scheduler serial playback timing.
- Scheduler priority, control-lane, queue-clear, cancellation, capacity, queue-identity, snapshot-immutability, outcome exactly-once, and interrupt behavior.
- Scheduler control terminal outcomes: executed, canceled, expired, dropped, queue-cleared, scheduler-stopped.
- Scheduler animation terminal outcomes: executed, canceled, expired, dropped, queue-cleared, scheduler-stopped, permanent-error.
- Scheduler outcome sink tests for reliable metric recording, critical-path sink blocking/ordering, reliable sink panic accounting, observer delivery drops, dispatcher close, and never-run scheduler cleanup.
- App lifecycle tests for never-run close, repeated close, construction-failure rollback, `Close` while running, `RunWorkers` after close/shutdown, close/run startup races, post-stop restart semantics, shutdown timeout recovery, and coordinated shutdown.
- App process-run tests for pre-listen admission rejection, `Run` cleanup on context cancellation/listen failure/worker failure, and compound cleanup errors.
- Scheduler restore policy behavior, especially previous frame and background.
- Desired-background behavior for startup, reconnect, transient playback restore policies, direct controls, firmware presets, renderable backgrounds, render failures, retry/backoff suppression, prompt recovery triggers, and partial-stream replay from frame zero.
- Readiness tests for startup without matrix, connected fake matrix, idle disconnect, shutdown, draining, and recoverable command failures.
- Heartbeat probe timeout and queued-work latency tests.
- Reconnect backoff tests for min/max growth, jitter injection, attempt reset, deadline-capped sleeps, scheduler-level metrics/logging, and TCP-client internal reconnect metrics/logging.
- HTTP notify/play/events/direct-control tests against fake ESP8266.
- Admin auth tests for local and non-local binds.
- Metrics exposure tests for reliable play-item outcomes, observer delivery drops, reliable sink failures including nonzero app-level exposure, play queue depth, reconnect attempts/delays/recoveries/failures, ping-specific immediate reconnect behavior, probe failures, matrix command attempts, render duration, matrix connected transitions, observability callback panics, event queue depth, event-worker in-flight state, and event publisher backpressure duration/timeout metrics.
- Event bus instrumentation tests for depth callback panic recovery, no-lock callback execution, publish/unsubscribe and publish/close ordering, lifecycle terminal zero-depth behavior, blocking publish behavior, publisher context timeout while a subscriber is full, close/unsubscribe waiting behind blocked publish until every documented release path, and partial fan-out before a later subscriber timeout.

Manual hardware validation remains required before unattended LAN deployment:

- Ping matrix.
- Fill red/green/blue.
- Draw an asymmetric 8x8 orientation fixture.
- Play the 2 second notification animation.
- Queue three notifications and verify serial playback.
- Start configured background preset, trigger notification, verify restore, then exercise manual fill/clear/preset behavior according to the chosen desired-background control contract.
- Disconnect/reconnect matrix power or Wi-Fi and verify recovery.

## Open Decisions

- Should non-local admin endpoints fail startup when token is missing, or start with admin endpoints disabled? Current implementation fails startup.
- App instances are one-shot after any worker run. Future reload should construct a fresh app instead of restarting a stopped instance in place.
- `App.Run` owns terminal resource cleanup after its errgroup exits and shares lifecycle admission with `RunWorkers` before binding HTTP.
- Should controls always use the reserved lane, or should some controls require explicit interrupt modes?
- Should critical interrupts drop, pause, or requeue the interrupted item?
- Should idle heartbeat probes remain synchronous with a bounded queue delay, or run asynchronously outside the item-selection loop?
- Should command metrics represent TCP attempts, scheduler logical commands, or both as separate metric families?
- Should reconnect observability remain split between scheduler and TCP client, or move behind a shared app-owned matrix connection observability adapter?
- Should config remain split across `config.yaml`, `animations.yaml`, and `rules.yaml` for reload granularity?
- Is text rendering useful on one 8x8 matrix, or should v1 stay icon/symbol focused?
- Should dashboards use polling, SSE, or WebSocket for queue and health state?
