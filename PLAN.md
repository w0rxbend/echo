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

Verified during iteration 20 review:

- `go test ./...` passes.
- `go vet ./...` passes.
- `go test -race ./...` passes.
- `go test -race ./internal/app ./internal/integrations/httpapi -run 'TestReadyAndMetricsExposePreviousFrameBackgroundDedupe|TestEventsOverrideValidation|TestEventsAnimationOverride|TestAnimationCatalog|TestAnimationsEndpoint|TestFirmwarePresetIsNotPlayableThroughPublicAnimationIngress' -count=5` passes.
- `go test -race ./internal/matrix -run 'TestSchedulerPreviousFrameRestoreDoesNotConvergeRenderableBackgroundFromVisuallyIdenticalNonBackgroundFrame' -count=20` fails intermittently/deterministically depending on scheduler interleaving.

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
- Animation registry distinguishes renderable generated animations from
  metadata-only firmware presets through `IsRenderable`, `Entry`,
  `RenderableIDs`, and `Catalog`.
- Ordinary playback rejects non-renderable firmware preset IDs at config rules,
  `/play`, `/notify`, `/events attributes.animation`, and scheduler guardrail
  boundaries.
- `/api/v1/animations` remains backward-compatible and lists only playable
  renderable IDs.
- `/api/v1/animations/catalog` exposes all registry entries with stable
  `{id, kind, playable}` fields. Firmware presets remain `playable=false`.
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
  parameters; renderable backgrounds rely on recorded background identity.
- Deduped previous-frame background convergence is documented as
  playback-restore convergence: it can mark the desired background clean without
  updating scheduler-owned background restore attempt/success telemetry.
- `docs/background-convergence-v1.md` is the source-of-truth public contract
  for background state projection, retry semantics, animation discovery,
  generic event override validation, and duplicate-suppression telemetry.

## Current Findings

High severity:

- The new renderable-background identity regression is not stable. Repeated
  focused race runs fail because the test asserts an exact queue-depth sequence
  (`[1,2,1,0]`) even though the scheduler may legitimately process the first
  item before the second enqueue and emit `[1,0,1,0]` or `[1,1,1,0]`. The
  production behavior under test still appears correct, but the guardrail cannot
  be trusted until it synchronizes on behavior rather than queue interleaving.

Medium severity:

- README and `docs/background-convergence-v1.md` show catalog examples with
  `kind: "renderable"` for `notification`, but the actual API and tests expose
  `kind: "generated"`. This is a public documentation/API contract mismatch.
- `TestAnimationCatalogEndpointIncludesNonPlayableMetadata` requires catalog
  entries to contain exactly three fields. That freezes out additive catalog
  metadata even though future firmware-preset details (`effect_id`, `interval`,
  `color`) are a known possible extension.
- The structured catalog remains intentionally minimal. Operators can discover
  firmware presets as `playable=false`, but cannot inspect preset parameters
  such as effect ID, interval, or color through HTTP.
- Previous-frame duplicate suppression tests still use small real sleeps to
  prove no later idle duplicate command is sent. They pass in broad suites, but
  deterministic no-extra-command assertions would be more robust.
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

### Phase 1: Repair Public Contract Regressions

1. Fix the renderable-background identity regression test. (priority: high)
   - Remove exact queue-depth ordering assumptions from
     `TestSchedulerPreviousFrameRestoreDoesNotConvergeRenderableBackgroundFromVisuallyIdenticalNonBackgroundFrame`.
   - Synchronize on matrix command sequence, display-state identity, and final
     convergence state instead of concurrent queue-depth interleavings.
   - Re-run the focused scheduler race test with `-count=20` and keep it stable.

2. Correct animation catalog kind documentation. (priority: high)
   - Align README and `docs/background-convergence-v1.md` examples with the
     actual API value `kind: "generated"` for generated animations.
   - Add or keep documentation/tests that freeze only the supported bounded
     kind vocabulary: `generated` and `firmware_preset`.

3. Relax catalog compatibility tests for additive fields. (priority: medium)
   - Preserve assertions that every entry includes `id`, `kind`, and
     `playable` with correct semantics.
   - Stop requiring exactly three fields unless the product decision is to
     forbid additive catalog metadata.
   - If exact-minimal catalog shape remains the intended v1 contract, document
     that explicitly and defer firmware-preset parameter inspection.

### Phase 2: Finish Catalog And Event API Truthfulness

1. Decide whether catalog metadata should include firmware-preset details.
   (priority: medium)
   - If operators need inspection, extend catalog entries additively with
     bounded metadata such as `effect_id`, `interval`, and `color` for
     `kind=firmware_preset`.
   - Keep `playable=false` for firmware presets and preserve `/play`, `/notify`,
     `/events`, and scheduler non-renderable guardrails.
   - Freeze any added fields with compatibility tests and avoid runtime state or
     background IDs as metric labels.

2. Keep generic event override documentation and tests aligned. (priority:
   medium)
   - Preserve synchronous validation for known `/api/v1/events` override fields:
     `animation`, `restore`, and `duration`.
   - Preserve schema-agnostic handling for unknown/custom attributes, including
     `param.*`.
   - Keep `/notify`, `/play`, and `/events` validation vocabulary aligned.

### Phase 3: Strengthen Background Dedup Guardrails

1. Make duplicate-command assertions deterministic where practical. (priority:
   medium)
   - Replace small real sleeps in previous-frame duplicate suppression tests
     with a fake-client quiet assertion, scheduler idle hook, or bounded
     no-command helper if a clean seam exists.
   - Preserve correctness over optimization for `restore: leave`, `clear`,
     `blank`, direct controls, and unknown display states.

2. Preserve telemetry separation for deduped playback restore convergence.
   (priority: medium)
   - Keep scheduler-owned background restore attempts/failures separate from
     playback restore commands.
   - If operators need a separate dedupe counter, add a bounded reason label
     without polluting restore-attempt counters.
   - Keep `/readyz.background` as the authoritative current convergence signal.

### Phase 4: Declarative Animation Expansion

1. Add declarative frame/pixel-art animations. (priority: medium)
   - Parse palette and 8-row pixel art in display-space.
   - Add brightness/simple transforms only if needed for v1 examples.
   - Keep config-authored frames in display-space and pack only through the
     layout mapper.
   - Reject malformed dimensions, unknown palette symbols, empty frame sets,
     invalid delays, and duplicate IDs at config load.

2. Tighten animation config docs. (priority: medium)
   - Document generated aliases, firmware presets, and frame animation schema in
     one place.
   - Decide whether relative `animations_file` and `rules_file` should resolve
     strictly relative to the config file.

### Phase 5: Event Delivery Boundary

1. Preserve the accepted v1 event bus contract until a redesign is planned.
   (priority: medium)
   - Do not add diagnostic subscribers, reload observers, non-block overflow
     policies, or deduplication on top of the current blocking fan-out model.
   - Treat publish errors as partial-delivery results, not atomic non-delivery.
   - Keep backpressure metrics total-only unless a bounded subscriber-class
     vocabulary is deliberately introduced.

2. If event delivery is redesigned, design subscriber isolation before
   implementation. (priority: medium)
   - Define independent delivery queues/goroutines, per-subscriber ordering,
     close/unsubscribe release semantics, terminal zero-depth ordering, publish
     timeout/drop metrics, partial-delivery reporting, and subscriber
     attribution.

### Phase 6: Scheduler, TCP, And Lifecycle Stability

1. Keep TCP callback critical paths bounded. (priority: medium)
   - Keep mutex-held callbacks limited to in-memory metrics and nonblocking
     enqueue.
   - Move command metrics off the TCP mutex only if profiling shows Prometheus
     contention or command latency impact.

2. Decide whether synchronous heartbeat probe latency is acceptable long-term.
   (priority: medium)
   - Current contract bounds queued-work delay by `probe_timeout`.
   - If unacceptable, move probes off the item-selection path without violating
     one command in flight.

3. Implement interrupt semantics. (priority: medium)
   - Support `none`, `higher_priority`, and `critical`.
   - Decide whether interrupted lower-priority items are dropped, paused, or
     requeued.
   - Add tests for no-interrupt, higher-priority interrupt, critical interrupt,
     and FIFO preservation after interruption.

4. Add `/api/v1/admin/reload`. (priority: low)
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
  direct controls, firmware presets, renderable backgrounds, render failures,
  retry/backoff suppression, due-retry projection, prompt recovery triggers,
  partial-stream replay, duplicate-restore suppression, and explicit
  renderable-background identity.
- HTTP notify/play/events/direct-control tests against fake ESP8266, including
  invalid generic event override rejection and documented preservation of
  schema-agnostic attributes.
- Animation catalog tests for playable list compatibility, structured
  metadata-only entries, additive metadata compatibility, and any future
  firmware-preset metadata.
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
- Should structured animation catalog expose firmware preset parameters?
- Should the catalog v1 shape allow additive metadata fields?
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
