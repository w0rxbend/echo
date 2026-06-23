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
- `go test -race ./internal/animations ./internal/config ./internal/app ./internal/integrations/httpapi -run 'TestFrameAnimation|TestLoadFrameAnimation|TestLoadRejectsInvalidFrameAnimation|TestLoadRejectsStrayAnimationTypeFields|TestConfigAuthoredFrameAnimationPublicSurfaces|TestAnimationCatalog|TestAppPlaysConfigAuthoredFrameAnimationThroughFakeESP|TestReadyzAndMetricsProjectGeneratedBackgroundKind|TestRegistryCatalogProjectsInternalRenderableKindToGenerated' -count=10` passes.
- `go test -race ./internal/app -run 'TestAppPlaysConfigAuthoredFrameAnimationThroughFakeESP' -count=20` passes.

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
- Runtime animation config loads generated aliases, metadata-only firmware
  presets, and declarative `type: frames` animations.
- Frame animations are authored as 8x8 display-space rows, validate palette
  symbols/dimensions/delays at config load, render immutable frame copies,
  reject known type-specific stray fields, and remain generated/playable
  entries in public discovery.
- Config-authored frame animation playback is covered end-to-end through app
  workers, HTTP `/play`, scheduler playback, fake ESP `SetFullFrame` commands,
  and layout-packed physical-chain payload assertions.
- `matrix_rain_background` is config-authored as a metadata-only firmware
  preset background.
- Animation registry distinguishes generated/playable animations from
  metadata-only firmware presets through `IsRenderable`, `Entry`,
  `RenderableIDs`, and `Catalog`.
- Public animation kind vocabulary is projected through
  `animations.ProjectPublicKind`: generated/playable entries expose
  `generated`, firmware presets expose `firmware_preset`, and internal
  background `renderable` is translated before it reaches public readiness,
  catalog, or Prometheus surfaces.
- `/api/v1/animations` remains backward-compatible and lists only playable
  generated IDs, including config-authored frame animations.
- `/api/v1/animations/catalog` exposes all registry entries through an explicit
  HTTP DTO with stable required fields `{id, kind, playable}`. Firmware presets
  remain `playable=false` and may include bounded optional metadata fields
  `effect_id`, `interval`, and `color`.
- Catalog firmware preset `interval` is intentionally frozen as a JSON duration
  string such as `"90ms"`; `effect_id` is a JSON number and `color` is a
  structured RGB object. Generated entries, including `type: frames`, must omit
  firmware metadata.
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
- Background convergence uses one shared projection function,
  `ProjectBackgroundConvergence`, consumed by scheduler health,
  `/readyz.background`, and `matrix_proxy_background_state`.
- Background retry policy is fixed for v1: retryable failures back off
  `1s..30s`, permanent failures back off `30s..5m`, forever until convergence
  or a reset trigger.
- Duplicate idle background restores are suppressed when
  `restore: previous_frame` successfully restores a display state that
  explicitly matches the configured background. Firmware presets compare preset
  parameters; generated backgrounds rely on recorded background identity.
- `docs/background-convergence-v1.md` is the source-of-truth public contract
  for background state projection, retry semantics, animation discovery,
  generic event override validation, and duplicate-suppression telemetry.

## Current Findings

High severity:

- None found in this review.

Medium severity:

- Animation config now rejects known cross-type stray fields, but the YAML
  decoder still ignores completely unknown or misspelled keys. A typo such as
  `pallete` or an unsupported future-looking field can still be silently
  dropped before type validation. Add strict unknown-field validation for
  `animations.yaml`, with clear errors that include animation ID and field path.
- Catalog wire-shape compatibility is strong today, but the handler depends on
  a hand-written DTO conversion. Future metadata additions must update the DTO,
  README, contract doc, and compatibility tests together or risk accidental API
  broadening.
- Frame animations are registered with generator ID `"frames"` but do not expose
  a distinct public subtype. This remains acceptable for v1 because public kind
  is deliberately `generated`; add only a bounded optional `subtype` later if
  operators need to distinguish generated aliases from config-authored frames.
- Background restore event metrics use `ProjectBackgroundConvergence`, but the
  event-time path still constructs a partial projection input and uses wall
  clock time at callback execution. `/metrics` refreshes from scheduler health,
  so black-box behavior is correct, but future event-time gauge updates should
  remain subordinate to scheduler health to avoid stale or contradictory gauges.
- The scheduler idle hook makes previous-frame dedupe tests deterministic, but
  it is package-private production state used only by tests. Keep it contained;
  prefer a fake-client quiet assertion or test-only helper if more idle
  synchronization tests accumulate.
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

### Phase 1: Make Animation Config Schema Strict

1. Reject unknown keys in `animations.yaml`. (high)
   - Use YAML node-level validation or `KnownFields`-style decoding for
     animation entries, frame objects, palette/color objects, and top-level
     fields.
   - Include the animation ID and offending key in errors.
   - Preserve intentional schema-agnostic behavior only for event attributes,
     not operator-authored animation config.

2. Keep type-specific stray-field validation locked. (high)
   - Preserve generated rejection of firmware preset fields and frame fields.
   - Preserve firmware preset rejection of `generator`, `palette`, and
     `frames`.
   - Preserve frame rejection of `generator`, `effect_id`, `interval`, and
     `color`.
   - Add regression cases for empty-but-present disallowed fields so presence,
     not only value, drives rejection.

3. Keep frame-animation playback coverage black-box. (medium)
   - Preserve the fake-ESP test that submits a config-authored frame animation
     through `/play` and verifies exact physical-chain `SetFullFrame` payloads.
   - Keep an asymmetric fixture that fails if display-space rows bypass the
     layout mapper or odd-row display compensation.

### Phase 2: Preserve Public Animation Discovery Contracts

1. Keep catalog wire shape locked. (high)
   - Preserve JSON type tests for `effect_id`, `interval`, and `color`.
   - Keep stable required fields `id`, `kind`, and `playable`.
   - Keep firmware metadata optional, bounded, and absent from generated/frame
     entries.

2. Keep explicit HTTP DTOs for catalog responses. (medium)
   - Do not return registry structs directly from handlers.
   - If more metadata is added, update the DTO, README, contract doc, and
     compatibility tests in the same change.

3. Preserve public kind projection guardrails. (medium)
   - Generated backgrounds and frame animations must expose `kind: "generated"`
     in `/readyz.background`, background metrics, and catalog output.
   - Any new public surface must include a negative check that internal
     `renderable` cannot leak.
   - Do not change `kind` to distinguish frame animations; use a bounded
     optional field only if that operator need becomes concrete.

### Phase 3: Background Projection And Dedupe Stability

1. Keep one background projection authority. (medium)
   - Route readiness, scheduler health, and current-state gauges through
     `ProjectBackgroundConvergence`.
   - Treat restore attempt/failure callbacks as event telemetry; prefer
     scheduler health refresh for current gauges when there is ambiguity.

2. Preserve previous-frame dedupe identity guardrails. (medium)
   - Preserve the negative case: pixel-equivalent non-background frames must not
     suppress idle convergence.
   - Preserve positive cases for exact configured firmware preset parameters and
     generated background ID identity.
   - Keep dedupe telemetry separate from scheduler-owned background restore
     attempts/failures.

3. Revisit the scheduler idle test seam if usage grows. (low)
   - If only dedupe tests need it, keep it package-private and documented by
     tests.
   - If more tests need it, prefer an explicit test helper or fake-client quiet
     assertion that does not add production behavior.

### Phase 4: Event Delivery Boundary

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

### Phase 5: Scheduler, TCP, And Lifecycle Stability

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

- Declarative frame animation tests for display-space parsing, palette
  validation, immutable render output, type-specific rejection, unknown-key
  rejection, public catalog projection, and fake-ESP packed-frame playback.
- Animation config schema tests for strict unknown fields, type-specific stray
  fields, empty-but-present disallowed fields, and clear error vocabulary.
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
- Play a config-authored frame animation with an asymmetric fixture.
- Queue three notifications and verify serial playback.
- Start configured background preset, trigger notification, verify restore, then
  exercise manual fill/clear/preset behavior with `restore_on_idle`
  expectations.
- Disconnect/reconnect matrix power or Wi-Fi and verify recovery.

## Open Decisions

- Should background non-convergence ever affect top-level readiness, or remain
  visible-only in `/readyz.background`?
- Should background retry `failure_count` be exposed as a bounded Prometheus
  gauge, or remain `/readyz`-only?
- Should frame animations expose an optional subtype in the catalog, or is
  public `kind: "generated"` sufficient for v1?
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
