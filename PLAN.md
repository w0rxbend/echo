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

Integrations and administrative matrix operations must never bypass the scheduler/TCP ownership boundary.

## Firmware Contract

Keep these constraints central in every matrix-facing change:

- TCP server port is `7777`.
- Firmware expects one active client.
- Command frames are binary: `LM`, version `0x01`, command byte, payload length `0..255`, payload, XOR checksum over prior bytes.
- Responses are exactly 6 bytes: `LM`, version `0x01`, response command `0x80`, status, XOR checksum.
- Read exactly one response after every command and validate magic, version, response command, checksum, and status.
- Use one command in flight per connection and enable `TCP_NODELAY`.
- Full frame payloads are 192 bytes; custom frame uploads are 196 bytes and must remain allowed.
- `SetFullFrame` and `UploadCustomFrame` take physical chain order. App code draws in display-space and packs through the layout mapper.

Default display-space layout remains `h-tl` with odd-row display compensation:

```text
display_to_server_point(x, y):
  if y % 2 == 1: return 7 - x, y
  return x, y

physical_index(server_x, y):
  if y % 2 == 0: return y*8 + server_x
  return y*8 + (7 - server_x)
```

## Current Status

Verified after iteration 18 review:

- `go test ./...` passes.
- `go vet ./...` passes.
- `go test -race ./...` passes.
- `go test -race ./internal/matrix ./internal/app ./internal/integrations/httpapi -run 'TestScheduler.*Background|TestProjectBackgroundConvergence|TestReadyAndMetricsExpose.*Background|TestEventsAnimationOverride|TestAnimationCatalog|TestAnimationsEndpoint|TestSchedulerPreviousFrameRestore' -count=5` passes.

Core implementation status:

- Go service skeleton, config loading, structured logging, app lifecycle, `/healthz`, `/readyz`, `/metrics`, and one-shot app worker semantics are implemented.
- TCP matrix client has strict protocol framing/response validation, command serialization, retryable transport classification, immediate reconnect telemetry, `TCP_NODELAY`, and fake TCP contract tests.
- Scheduler owns queue mutation, controls, playback, background convergence, lifecycle outcomes, and matrix command sequencing.
- Queue snapshots are immutable DTOs. Raw queue mutation is not exposed outside scheduler ownership.
- Admin auth fails closed for non-local binds. Queue inspection and matrix controls are protected when non-local auth is required.
- Reconnect, heartbeat/probe, matrix command, render duration, event queue, event worker in-flight, publisher backpressure, play-item, and background restore/state metrics are wired with bounded labels.
- Event bus v1 contract is accepted and documented in `docs/event-bus-contract.md`: sequential blocking fan-out under bus read lock, per-subscriber ordering, visible partial fan-out on publish errors, lifecycle-blocking depth callbacks, and terminal zero-depth lifecycle observations.
- Runtime animation config loads generated `notification` aliases and firmware presets. `matrix_rain_background` is config-authored as a firmware preset background.
- Animation registry distinguishes renderable generated animations from metadata-only firmware presets through `IsRenderable`, `Entry`, `RenderableIDs`, and now `Catalog`.
- Ordinary playback rejects non-renderable firmware preset IDs at config rules, `/play`, `/notify`, `/events attributes.animation`, and scheduler guardrail boundaries.
- `/api/v1/animations` remains backward-compatible and lists only playable renderable IDs.
- `/api/v1/animations/catalog` now exposes structured metadata for all registry entries as `{id, kind, playable}` without making firmware presets playable.
- Configured background is scheduler-owned desired idle state. It applies after first verified connection, remains outside the ordinary queue, is restored after reconnects and transient display changes, and is observable through readiness and Prometheus.
- Background convergence uses the v1 bounded public state vocabulary: `unknown`, `dirty`, `attempting`, `converged`, `failed`, `retrying`.
- Public background state is projected by one shared function, `ProjectBackgroundConvergence`, consumed by scheduler health, `/readyz.background`, and `matrix_proxy_background_state`.
- Due-retry semantics are frozen for v1: a dirty background with future `next_retry` projects as `retrying`; a dirty background with failure evidence and no future suppression projects as `failed`; `attempting` wins while a restore command is running.
- Background retry policy is fixed for v1: retryable failures back off `1s..30s`, permanent failures back off `30s..5m`, forever until convergence or a reset trigger.
- Duplicate idle background restores are suppressed when `restore: previous_frame` successfully restores a display state that explicitly matches the configured background. Firmware presets compare preset parameters; renderable backgrounds rely on the recorded background ID.
- `docs/background-convergence-v1.md` is the source-of-truth public contract for background state projection and retry semantics.

## Current Findings

High severity:

- No high-severity runtime regression was found in iteration 18.

Medium severity:

- `docs/background-convergence-v1.md` was added and README links it, but the new structured `/api/v1/animations/catalog` endpoint is not yet documented in README or operator examples. Operators may miss the supported way to inspect background-only firmware preset metadata.
- `/api/v1/events` now rejects unknown and non-renderable `attributes.animation` before publish, but the generic event surface still silently accepts invalid `attributes.restore`, invalid `attributes.duration`, and arbitrary override-like attributes that fail or no-op later in the app worker.
- The background duplicate suppression is intentionally conservative, but its telemetry semantics are subtle: a successful `restore: previous_frame` can mark desired background converged without incrementing background restore attempt/success metrics because the command belongs to playback restore, not scheduler-owned background restore.
- Renderable-background deduplication depends on display-state identity (`BackgroundID`) captured from scheduler-owned background restore. It does not attempt pixel equality, so equivalent generated frames from other sources still trigger a later idle background restore.
- `CatalogEntry` exposes only `id`, `kind`, and `playable`. That is safe, but operators still cannot inspect firmware-preset metadata such as effect ID, interval, and color through the HTTP catalog.
- Prometheus still intentionally omits background retry `failure_count`; dashboards must poll `/readyz.background` to distinguish first retry from repeated retry.
- Heartbeat probes remain synchronous on the scheduler selection path by documented contract. New queued work can wait up to `probe_timeout` behind an in-progress idle probe.
- TCP-client callbacks still execute Prometheus metric updates while the TCP client mutex is held. Logging is off-path, but metric recording remains command-serialization critical-path work.
- Event bus v1 limitations remain: blocking fan-out under read lock, lifecycle-blocking depth callbacks, partial delivery before publish errors, and total-only backpressure metrics.
- `InterruptMode` is mapped but ignored; `higher_priority` and `critical` interruption behavior is not implemented.
- Declarative frame/pixel-art animations are not implemented.
- `/api/v1/admin/reload` is planned but absent.

Low severity:

- Background previous-frame duplicate suppression tests use small real sleeps to prove no later idle duplicate command is sent. The tests passed repeatedly, but a deterministic no-extra-command assertion would be more robust if a scheduler idle hook becomes available.
- `NewSchedulerWithReliableAppOutcomeRecorder` is intentionally narrower than a general callback option, but it remains callable by any `internal/...` package and should remain under review for misuse.
- App lifecycle tests still use package-private test seams for some failure paths; they are no longer exported but still shape production structs slightly.
- Active animation deadlines are not enforced during a frame-delay sleep; expiration is checked before the next frame, so long frame delays can overshoot an item deadline.

## Next Iteration Priorities

### Phase 1: Finish Public API Truthfulness

1. Document structured animation catalog discovery. (priority: high)
   - Add README/API documentation for `GET /api/v1/animations/catalog`.
   - State clearly that `GET /api/v1/animations` is the playable-only backward-compatible list.
   - State that catalog firmware presets are metadata-only/background-safe and cannot be submitted to playback endpoints.
   - Add an HTTP/API compatibility test if docs mention a stable response shape.

2. Decide whether catalog metadata should include firmware-preset details. (priority: medium)
   - If operators need inspection, extend catalog entries with bounded metadata such as `effect_id`, `interval`, and `color` for `kind=firmware_preset`.
   - Keep `playable=false` for firmware presets and preserve `/play`, `/notify`, `/events`, and scheduler non-renderable guardrails.
   - Avoid adding high-cardinality runtime state or background IDs as metric labels.

3. Broaden generic `/api/v1/events` override validation. (priority: high)
   - Validate `attributes.restore` against the same restore-policy vocabulary as `/notify` and `/play`.
   - Validate `attributes.duration` bounds/parse errors at the HTTP boundary if it is treated as a first-class override.
   - Decide whether unknown `attributes.param.*` and other attributes remain schema-agnostic.
   - Add tests proving invalid generic event overrides are rejected before publish and do not reach the async app worker.

### Phase 2: Freeze Background Dedup Telemetry Semantics

1. Decide whether previous-frame background dedupe should record background success telemetry. (priority: medium)
   - Current behavior marks desired background clean/converged but does not increment background restore attempts or update `last_success`.
   - Either document this as playback-restore convergence, or update health/logging with a separate bounded reason that does not pollute background restore attempt counters.
   - Add tests around `/readyz.background.last_success` and background restore metrics after deduped `restore: previous_frame`.

2. Add explicit renderable-background identity tests. (priority: medium)
   - Prove dedupe only occurs when the previous display state carries the configured background ID.
   - Prove visually identical frames from a non-background source do not incorrectly mark the configured background clean.
   - Keep equality cheap and explicit; do not introduce full-frame comparisons unless there is a real hardware-noise problem.

3. Keep duplicate command suppression conservative. (priority: low)
   - Preserve correctness over optimization for `restore: leave`, `clear`, `blank`, direct controls, and unknown display states.
   - Only suppress background restore when the immediate restored state is explicitly known to be the configured background.

### Phase 3: Declarative Animation Expansion

1. Add declarative frame/pixel-art animations. (priority: medium)
   - Parse palette and 8-row pixel art in display-space.
   - Add brightness/simple transforms only if needed for v1 examples.
   - Keep config-authored frames in display-space and pack only through the layout mapper.
   - Reject malformed dimensions, unknown palette symbols, empty frame sets, invalid delays, and duplicate IDs at config load.

2. Tighten animation config docs. (priority: medium)
   - Document generated aliases, firmware presets, and future frame animation schema in one place.
   - Decide whether relative `animations_file` and `rules_file` should resolve strictly relative to the config file.

### Phase 4: Event Delivery Boundary

1. Preserve the accepted v1 event bus contract until a redesign is planned. (priority: medium)
   - Do not add diagnostic subscribers, reload observers, non-block overflow policies, or deduplication on top of the current blocking fan-out model.
   - Treat publish errors as partial-delivery results, not atomic non-delivery.
   - Keep backpressure metrics total-only unless a bounded subscriber-class vocabulary is deliberately introduced.

2. If event delivery is redesigned, design subscriber isolation before implementation. (priority: medium)
   - Define independent delivery queues/goroutines, per-subscriber ordering, close/unsubscribe release semantics, terminal zero-depth ordering, publish timeout/drop metrics, partial-delivery reporting, and subscriber attribution.

### Phase 5: Scheduler, TCP, And Lifecycle Stability

1. Keep TCP callback critical paths bounded. (priority: medium)
   - Keep mutex-held callbacks limited to in-memory metrics and nonblocking enqueue.
   - Move command metrics off the TCP mutex only if profiling shows Prometheus contention or command latency impact.

2. Decide whether synchronous heartbeat probe latency is acceptable long-term. (priority: medium)
   - Current contract bounds queued-work delay by `probe_timeout`.
   - If unacceptable, move probes off the item-selection path without violating one command in flight.

3. Implement interrupt semantics. (priority: medium)
   - Support `none`, `higher_priority`, and `critical`.
   - Decide whether interrupted lower-priority items are dropped, paused, or requeued.
   - Add tests for no-interrupt, higher-priority interrupt, critical interrupt, and FIFO preservation after interruption.

4. Add `/api/v1/admin/reload`. (priority: low)
   - Validate new config/rules/animations before applying.
   - Construct a fresh app instance instead of restarting a stopped instance in place.
   - Use coordinated app shutdown after a successful swap.
   - Do not add reload event observers without a fresh event bus design pass.

## Testing Plan

Keep existing coverage and expand in these areas:

- Protocol builder checksum and payload limits, including 196-byte custom frame uploads.
- Response parser validation and typed status mapping.
- Fake TCP matrix server strict frame validation, firmware status injection, dropped responses, one-response-per-command behavior, and pipelining detection.
- Matrix reconnect tests for scheduler backoff, TCP immediate reconnect, retry-ping verification failure, recovery/failure metrics, callback panics, and no reconnect after protocol/status errors.
- Display-space orientation tests using asymmetric fixtures.
- Scheduler serial playback, priority, control lane, queue clear, cancellation, capacity, queue identity, snapshot immutability, terminal outcomes, and interrupt behavior.
- Desired-background tests for startup, reconnect, transient restore policies, direct controls, firmware presets, renderable backgrounds, render failures, retry/backoff suppression, due-retry projection, prompt recovery triggers, partial-stream replay, and duplicate-restore suppression.
- HTTP notify/play/events/direct-control tests against fake ESP8266, including invalid generic event override rejection.
- Animation catalog tests for playable list compatibility and structured metadata-only entries.
- App lifecycle tests for never-run close, repeated close, construction rollback, close while running, post-stop one-shot semantics, shutdown timeout recovery, and process-run cleanup.
- Metrics exposure tests for play-item outcomes, observer drops, reliable sink failures, queue depth, reconnects, probe failures, matrix command attempts, render duration, connected transitions, callback panics, event queue/in-flight/backpressure, and background convergence.
- Event bus instrumentation tests for depth callback panic recovery, no-lock callback execution, lifecycle terminal zero-depth behavior, blocked publish behavior, close/unsubscribe blocked-publish release paths, and partial fan-out before timeout.

Manual hardware validation remains required before unattended LAN deployment:

- Ping matrix.
- Fill red/green/blue.
- Draw an asymmetric 8x8 orientation fixture.
- Play the 2 second notification animation.
- Queue three notifications and verify serial playback.
- Start configured background preset, trigger notification, verify restore, then exercise manual fill/clear/preset behavior with `restore_on_idle` expectations.
- Disconnect/reconnect matrix power or Wi-Fi and verify recovery.

## Open Decisions

- Should background non-convergence ever affect top-level readiness, or remain visible-only in `/readyz.background`?
- Should structured animation catalog expose firmware preset parameters?
- Should generic `/api/v1/events` remain schema-agnostic beyond known override fields?
- Should background retry remain fixed v1 policy, or become configurable after hardware validation?
- Should controls always use the reserved lane, or should some controls require explicit interrupt modes?
- Should critical interrupts drop, pause, or requeue the interrupted item?
- Should idle heartbeat probes remain synchronous with bounded queue delay, or run asynchronously outside the item-selection loop?
- Should command metrics remain TCP-attempt metrics only, or should scheduler logical command metrics be added separately?
- Should config remain split across `config.yaml`, `animations.yaml`, and `rules.yaml` for reload granularity?
- Should dashboards use polling, SSE, or WebSocket for queue and health state?
