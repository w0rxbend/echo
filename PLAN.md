# LED Matrix Proxy Server Plan

## Goal

Build a Go proxy server that owns the single long-lived TCP connection to the
ESP8266 LED matrix controller, accepts events from HTTP and future integrations,
maps them to animation intents, schedules them serially, and drives the matrix
through the firmware binary protocol.

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

Integrations and administrative matrix operations must never bypass the
scheduler/TCP ownership boundary.

## Firmware Contract

Keep these constraints central in every matrix-facing change:

- TCP server port is `7777`.
- Firmware expects one active client.
- Command frames are binary: `LM`, version `0x01`, command byte, payload length
  `0..255`, payload, XOR checksum over prior bytes.
- Responses are exactly 6 bytes: `LM`, version `0x01`, response command `0x80`,
  status, XOR checksum.
- Read exactly one response after every command and validate magic, version,
  response command, checksum, and status.
- Use one command in flight per connection and enable `TCP_NODELAY`.
- Full frame payloads are 192 bytes; custom frame uploads are 196 bytes and
  must remain allowed.
- `SetFullFrame` and `UploadCustomFrame` take physical chain order. App code
  draws in display-space and packs through the layout mapper.

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

Verified during this review:

- `go test ./...` passes.
- `go vet ./...` passes.
- `go test -race ./...` passes.
- `go test -race ./internal/matrix ./internal/app ./internal/integrations/httpapi ./internal/metrics -run 'TestSchedulerPreviousFrameRestore|TestProjectPublicKind|TestReadyAndMetricsExpose.*Background|TestAnimationCatalog|TestAnimationsEndpoint|TestEventsOverrideValidation|TestOverrideValidationErrorVocabulary|TestBackgroundStateGauges|TestBackgroundRestoreMetrics' -count=5` passes.

Core implementation status:

- Go service skeleton, config loading, structured logging, app lifecycle,
  `/healthz`, `/readyz`, `/metrics`, and one-shot app worker semantics are
  implemented.
- TCP matrix client has strict protocol framing/response validation, command
  serialization, retryable transport classification, immediate reconnect
  telemetry, `TCP_NODELAY`, fake TCP contract tests, and bounded observability
  callback panic/drop accounting.
- Scheduler owns queue mutation, controls, playback, background convergence,
  lifecycle outcomes, and matrix command sequencing.
- Queue snapshots are immutable DTOs. Raw queue mutation is not exposed outside
  scheduler ownership.
- Admin auth fails closed for non-local binds. Queue inspection and matrix
  controls are protected when non-local auth is required.
- Reconnect, heartbeat/probe, matrix command, render duration, event queue,
  event worker in-flight, publisher backpressure, play-item, and background
  restore/state metrics are wired with bounded labels.
- Event bus v1 contract is accepted and documented in
  `docs/event-bus-contract.md`: sequential blocking fan-out under bus read
  lock, per-subscriber ordering, visible partial fan-out on publish errors,
  lifecycle-blocking depth callbacks, and terminal zero-depth lifecycle
  observations.
- Runtime animation config loads generated `notification` aliases and firmware
  presets. `matrix_rain_background` is config-authored as a firmware preset
  background.
- Animation registry distinguishes generated/playable animations from
  metadata-only firmware presets through `IsRenderable`, `Entry`,
  `RenderableIDs`, and `Catalog`.
- Public animation kind vocabulary is now projected through
  `animations.ProjectPublicKind`: generated/playable entries expose
  `generated`, firmware presets expose `firmware_preset`, and internal
  background `renderable` is translated before it reaches readiness or metrics.
- `/api/v1/animations` remains backward-compatible and lists only playable
  generated IDs.
- `/api/v1/animations/catalog` exposes all registry entries with stable
  `{id, kind, playable}` fields. Firmware presets remain `playable=false` and
  may include bounded additive metadata fields `effect_id`, `interval`, and
  `color`.
- Ordinary playback rejects non-renderable firmware preset IDs at config rules,
  `/play`, `/notify`, `/events attributes.animation`, and scheduler guardrail
  boundaries.
- Generic `POST /api/v1/events` rejects known invalid override fields before
  publish: unknown/non-renderable `attributes.animation`, invalid
  `attributes.restore`, and malformed or negative `attributes.duration`.
  Unknown/custom attributes, including `param.*`, remain schema-agnostic.
- Configured background is scheduler-owned desired idle state. It applies after
  first verified connection, remains outside the ordinary queue, is restored
  after reconnects and transient display changes, and is observable through
  readiness and Prometheus.
- Background convergence uses the v1 bounded public state vocabulary:
  `unknown`, `dirty`, `attempting`, `converged`, `failed`, `retrying`.
- Public background state is projected by one shared function,
  `ProjectBackgroundConvergence`, consumed by scheduler health,
  `/readyz.background`, and `matrix_proxy_background_state`.
- Due-retry semantics are frozen for v1: a dirty background with future
  `next_retry` projects as `retrying`; a dirty background with failure evidence
  and no future suppression projects as `failed`; `attempting` wins while a
  restore command is running.
- Background retry policy is fixed for v1: retryable failures back off
  `1s..30s`, permanent failures back off `30s..5m`, forever until convergence
  or a reset trigger.
- Duplicate idle background restores are suppressed when
  `restore: previous_frame` successfully restores a display state that
  explicitly matches the configured background. Firmware presets compare preset
  parameters; generated backgrounds rely on recorded background identity.
- Deduped previous-frame background convergence is documented as
  playback-restore convergence: it can mark the desired background clean without
  updating scheduler-owned background restore attempt/success telemetry.
- `docs/background-convergence-v1.md` is the source-of-truth public contract
  for background state projection, retry semantics, animation discovery,
  generic event override validation, and duplicate-suppression telemetry.

## Current Findings

High severity:

- None found in this review.

Medium severity:

- Catalog documentation examples show firmware preset `interval` as `"90ms"`,
  but the HTTP handler serializes `*time.Duration` directly, so the current JSON
  wire value is numeric nanoseconds. This must be corrected before operators or
  clients depend on the catalog metadata shape.
- The new scheduler idle hook makes previous-frame dedupe tests much more
  deterministic, but it is package-private production state used only by tests.
  Keep it under review; prefer an explicit test-only seam or fake-client quiet
  assertion if more idle synchronization tests accumulate.
- Background restore event metrics briefly consume raw event state, while
  readiness/health consume the shared projection. The current tests pass, but
  future background metrics should continue to be projected through the same
  public state function to prevent drift.
- Prometheus still intentionally omits background retry `failure_count`;
  dashboards must poll `/readyz.background` to distinguish first retry from
  repeated retry.
- Heartbeat probes remain synchronous on the scheduler selection path by
  documented contract. New queued work can wait up to `probe_timeout` behind an
  in-progress idle probe.
- TCP-client callbacks still execute Prometheus metric updates while the TCP
  client mutex is held. Logging is off-path, but metric recording remains
  command-serialization critical-path work.
- Event bus v1 limitations remain: blocking fan-out under read lock,
  lifecycle-blocking depth callbacks, partial delivery before publish errors,
  and total-only backpressure metrics.
- `InterruptMode` is mapped but ignored; `higher_priority` and `critical`
  interruption behavior is not implemented.
- Declarative frame/pixel-art animations are not implemented.
- `/api/v1/admin/reload` is planned but absent.

Low severity:

- `NewSchedulerWithReliableAppOutcomeRecorder` is intentionally narrower than a
  general callback option, but it remains callable by any `internal/...` package
  and should remain under review for misuse.
- App lifecycle tests still use package-private test seams for some failure
  paths; they are no longer exported but still shape production structs
  slightly.
- Active animation deadlines are not enforced during a frame-delay sleep;
  expiration is checked before the next frame, so long frame delays can overshoot
  an item deadline.

## Next Iteration Priorities

### Phase 1: Repair Catalog Metadata Wire Contract

1. Decide the catalog `interval` wire type. (high)
   - Current code emits numeric nanoseconds because `time.Duration` is encoded
     directly.
   - README and `docs/background-convergence-v1.md` currently show `"90ms"`.
   - Either change the API DTO to emit a duration string intentionally, or update
     docs and compatibility tests to freeze numeric nanoseconds.

2. Add catalog metadata wire-shape tests. (high)
   - Assert exact JSON types for `effect_id`, `interval`, and `color`.
   - Keep stable required fields `id`, `kind`, `playable`.
   - Keep bounded additive firmware metadata optional and absent from generated
     entries.

3. Keep registry structs out of HTTP wire contracts. (medium)
   - Continue using explicit handler DTOs rather than returning registry structs
     directly.
   - If more metadata is added, update DTO, docs, and compatibility tests in the
     same change.

### Phase 2: Strengthen Public Projection Guardrails

1. Add cross-surface background kind compatibility tests. (high)
   - Verify generated backgrounds expose `kind: "generated"` in
     `/readyz.background`, `matrix_proxy_background_*{kind="generated"}`, and
     catalog entries.
   - Verify no public readiness or metric surface emits `kind="renderable"`.

2. Keep `ProjectPublicKind` as the canonical boundary translator. (medium)
   - Treat matrix-internal `BackgroundKindRenderable` as private.
   - Avoid duplicating string mappings outside the projection helper.

3. Route background metric state through shared projection consistently. (medium)
   - Review `recordBackgroundRestoreMetric` event-time updates for any raw-state
     drift.
   - Prefer projection-derived state for current gauges and reserve restore
     attempt/failure counters for event telemetry.

### Phase 3: Finish Previous-Frame Dedupe Guardrails

1. Keep deterministic display-state identity tests. (medium)
   - Preserve the negative case: pixel-equivalent non-background frames must not
     suppress idle convergence.
   - Preserve positive cases for exact configured firmware preset parameters and
     generated background ID identity.

2. Revisit the scheduler idle test seam. (medium)
   - If only these tests use it, keep it package-private and documented by tests.
   - If more tests need it, consider a test-only helper or fake-client quiet
     assertion that does not add production fields.

3. Preserve dedupe telemetry separation. (medium)
   - Keep scheduler-owned background restore attempts/failures separate from
     playback restore commands.
   - If operators need a dedupe counter, add a bounded reason label without
     polluting restore-attempt counters.

### Phase 4: Declarative Animation Expansion

1. Add declarative frame/pixel-art animations. (medium)
   - Parse palette and 8-row pixel art in display-space.
   - Add brightness/simple transforms only if needed for v1 examples.
   - Keep config-authored frames in display-space and pack only through the
     layout mapper.
   - Reject malformed dimensions, unknown palette symbols, empty frame sets,
     invalid delays, and duplicate IDs at config load.

2. Tighten animation config docs. (medium)
   - Document generated aliases, firmware presets, and frame animation schema in
     one place.
   - Decide whether relative `animations_file` and `rules_file` should resolve
     strictly relative to the config file.

### Phase 5: Event Delivery Boundary

1. Preserve the accepted v1 event bus contract until a redesign is planned.
   (medium)
   - Do not add diagnostic subscribers, reload observers, non-block overflow
     policies, or deduplication on top of the current blocking fan-out model.
   - Treat publish errors as partial-delivery results, not atomic non-delivery.
   - Keep backpressure metrics total-only unless a bounded subscriber-class
     vocabulary is deliberately introduced.

2. If event delivery is redesigned, design subscriber isolation before
   implementation. (medium)
   - Define independent delivery queues/goroutines, per-subscriber ordering,
     close/unsubscribe release semantics, terminal zero-depth ordering, publish
     timeout/drop metrics, partial-delivery reporting, and subscriber
     attribution.

### Phase 6: Scheduler, TCP, And Lifecycle Stability

1. Keep TCP callback critical paths bounded. (medium)
   - Keep mutex-held callbacks limited to in-memory metrics and nonblocking
     enqueue.
   - Move command metrics off the TCP mutex only if profiling shows Prometheus
     contention or command latency impact.

2. Decide whether synchronous heartbeat probe latency is acceptable long-term.
   (medium)
   - Current contract bounds queued-work delay by `probe_timeout`.
   - If unacceptable, move probes off the item-selection path without violating
     one command in flight.

3. Implement interrupt semantics. (medium)
   - Support `none`, `higher_priority`, and `critical`.
   - Decide whether interrupted lower-priority items are dropped, paused, or
     requeued.
   - Add tests for no-interrupt, higher-priority interrupt, critical interrupt,
     and FIFO preservation after interruption.

4. Add `/api/v1/admin/reload`. (low)
   - Validate new config/rules/animations before applying.
   - Construct a fresh app instance instead of restarting a stopped instance in
     place.
   - Use coordinated app shutdown after a successful swap.
   - Do not add reload event observers without a fresh event bus design pass.

## Testing Plan

Keep existing coverage and expand in these areas:

- Protocol builder checksum and payload limits, including 196-byte custom frame
  uploads.
- Response parser validation and typed status mapping.
- Fake TCP matrix server strict frame validation, firmware status injection,
  dropped responses, one-response-per-command behavior, and pipelining
  detection.
- Matrix reconnect tests for scheduler backoff, TCP immediate reconnect,
  retry-ping verification failure, recovery/failure metrics, callback panics,
  and no reconnect after protocol/status errors.
- Display-space orientation tests using asymmetric fixtures.
- Scheduler serial playback, priority, control lane, queue clear, cancellation,
  capacity, queue identity, snapshot immutability, terminal outcomes,
  background convergence/dedupe, and interrupt behavior.
- Desired-background tests for startup, reconnect, transient restore policies,
  direct controls, firmware presets, generated backgrounds, render failures,
  retry/backoff suppression, due-retry projection, prompt recovery triggers,
  partial-stream replay, duplicate-restore suppression, and exact background
  identity.
- HTTP notify/play/events/direct-control tests against fake ESP8266, including
  invalid generic event override rejection and documented preservation of
  schema-agnostic attributes.
- Animation catalog tests for playable list compatibility, structured
  metadata-only entries, additive metadata compatibility, wire JSON types, and
  bounded firmware-preset metadata.
- App lifecycle tests for never-run close, repeated close, construction
  rollback, close while running, post-stop one-shot semantics, shutdown timeout
  recovery, and process-run cleanup.
- Metrics exposure tests for play-item outcomes, observer drops, reliable sink
  failures, queue depth, reconnects, probe failures, matrix command attempts,
  render duration, connected transitions, callback panics, event
  queue/in-flight/backpressure, and background convergence.
- Event bus instrumentation tests for depth callback panic recovery, no-lock
  callback execution, lifecycle terminal zero-depth behavior, blocked publish
  behavior, close/unsubscribe blocked-publish release paths, and partial fan-out
  before timeout.

Manual hardware validation remains required before unattended LAN deployment:

- Ping matrix.
- Fill red/green/blue.
- Draw an asymmetric 8x8 orientation fixture.
- Play the 2 second notification animation.
- Queue three notifications and verify serial playback.
- Start configured background preset, trigger notification, verify restore, then
  exercise manual fill/clear/preset behavior with `restore_on_idle`
  expectations.
- Disconnect/reconnect matrix power or Wi-Fi and verify recovery.

## Open Decisions

- Should background non-convergence ever affect top-level readiness, or remain
  visible-only in `/readyz.background`?
- Should catalog firmware preset `interval` be a duration string or numeric
  nanoseconds on the HTTP wire?
- Should background retry `failure_count` be exposed as a bounded Prometheus
  gauge, or remain `/readyz`-only?
- Should structured animation catalog expose any additional firmware preset
  parameters beyond `effect_id`, `interval`, and `color`?
- Should generic `/api/v1/events` remain schema-agnostic beyond known override
  fields?
- Should background retry remain fixed v1 policy, or become configurable after
  hardware validation?
- Should controls always use the reserved lane, or should some controls require
  explicit interrupt modes?
- Should critical interrupts drop, pause, or requeue the interrupted item?
- Should idle heartbeat probes remain synchronous with bounded queue delay, or
  run asynchronously outside the item-selection loop?
- Should command metrics remain TCP-attempt metrics only, or should scheduler
  logical command metrics be added separately?
- Should config remain split across `config.yaml`, `animations.yaml`, and
  `rules.yaml` for reload granularity?
- Should dashboards use polling, SSE, or WebSocket for queue and health state?
