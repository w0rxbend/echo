# LED Matrix Proxy

Go proxy service for an ESP8266 LED matrix controller. The app accepts normalized events through HTTP and integrations, maps them through rules, schedules matrix work serially, and owns the single TCP connection to the controller.

## Event Bus Observability

The event bus currently supports only `queue.overflow_policy: block`. Publish uses sequential blocking fan-out: if a subscriber channel is full, the publisher waits until that subscriber receives from its channel or the publish context expires. In the current app, HTTP and event publishers can wait behind a full app-worker subscriber before the event worker receives from its channel. A publish error is not atomic non-delivery: earlier subscribers may already have received the event before a later subscriber blocks long enough for the publish context to expire.

`Bus.Close` and unsubscribe are not release mechanisms for an already blocked publisher because `Publish` holds the bus read lock while blocked. Depth callbacks are synchronous lifecycle blockers, so terminal cleanup waits for any in-flight callback to return.

`matrix_proxy_event_queue_depth` reports subscriber-channel backlog only. For the app worker, it is the number of normalized events buffered in the event-worker subscriber channel and waiting to be received. It does not include the event currently being mapped, enqueued, or logged by the worker.

`matrix_proxy_event_worker_inflight` is the separate current-event signal. It is `1` while the app event worker is actively processing one received event and `0` when idle.

Subscriber depth cleanup is terminal. Unsubscribe, subscription context cancellation, closed subscription, and bus close emit one final zero-depth observation for that subscription. Stale publish-side or receive-side observations are suppressed after terminal cleanup, so removed or closed subscribers cannot later report a nonzero queue depth. Bus-owned `OnDepthChange` callbacks are not invoked after unsubscribe returns.

Publisher backpressure is exposed as total-only metrics. These metric families intentionally have no subscriber labels:

- `matrix_proxy_event_publish_backpressure_duration_seconds`: blocked publish duration while publishers wait behind full subscriber channels.
- `matrix_proxy_event_publish_backpressure_timeouts_total`: publish attempts that fail because the publish context expires while blocked on subscriber backpressure.

The blocking fan-out design is still intentional for the supported `block` policy. Future `drop_oldest`, `drop_low_priority`, deduplication behavior, diagnostic subscribers, or subscriber-attributed backpressure metrics need explicit delivery and metric semantics before being enabled.

The obsolete `matrix_proxy_event_publish_backpressure_wait_seconds` metric family is not registered.

## Animation Configuration

`configs/config.example.yaml` enables a background named `matrix_rain_background`
and loads it from `configs/animations.example.yaml`. That entry is a
`firmware_preset`, so the rain effect ID, interval, and color are configuration
metadata instead of app-level special cases. Firmware preset entries are
background-only metadata; they are not renderable animations and cannot be used
as ordinary rule `play.animation` values or direct playback requests.

When `background.restore_on_idle` is true and `background.animation` is set,
the configured background is scheduler-owned desired matrix state. After the
first firmware-verified connection, whenever the matrix reconnects, and after
ordinary playback or direct matrix controls complete, the scheduler converges
the idle display back to that background.

Ordinary playback is transient. A playback item's `restore` policy controls
only the immediate post-playback behavior: `background` asks for an immediate
background restore, while policies such as `leave`, `previous_frame`, `clear`,
and `blank` may briefly leave or change the display after the item finishes.
Those policies do not permanently suppress idle convergence when
`restore_on_idle` is enabled.

Direct administrative matrix controls are also transient maintenance
operations. Requests such as fill, clear, and preset are still coordinated
through the scheduler and return only after the requested control executes, but
they do not take ownership of the desired idle display. If a configured
background is enabled, the scheduler will apply that background again after the
control path becomes idle.

The configured background ID is resolved from the merged animation registry and
may reference either a generated animation or a `firmware_preset`. Firmware
presets are applied with scheduler-owned `SetPreset` commands. Generated or
otherwise renderable backgrounds are rendered as finite frame sequences and
packed through the layout mapper; they are not long-running queue items and do
not block later transient events. HTTP handlers and app event processing do not
call the TCP matrix client directly.

### Animation Discovery

`GET /api/v1/animations` returns the backward-compatible response shape
`{"animations":[...]}`. The list contains only generated, directly playable
animation IDs, so clients that already submit discovered IDs to playback
endpoints can keep using this endpoint. Metadata-only firmware preset IDs such
as `matrix_rain_background` are intentionally excluded.

`GET /api/v1/animations/catalog` returns the structured discovery catalog for
all registry entries. Each entry exposes the stable fields `id`, `kind`, and
`playable`; no other catalog fields are part of this contract. `kind` is
bounded to `generated` and `firmware_preset`, and `playable` is true only for
`generated` entries.

```json
{
  "animations": [
    {
      "id": "notification",
      "kind": "generated",
      "playable": true
    },
    {
      "id": "matrix_rain_background",
      "kind": "firmware_preset",
      "playable": false
    }
  ]
}
```

Firmware preset catalog entries are metadata-only and safe for configured
background use. They have `playable: false` and must not be submitted to
ordinary playback surfaces, including `POST /api/v1/play`,
`POST /api/v1/notify`, or generic `POST /api/v1/events` with
`attributes.animation`.

### Generic Event Overrides

`POST /api/v1/events` accepts normalized events for asynchronous rule
processing. Known playback override attributes are validated synchronously
before the event is published to that async path:

- `attributes.animation` must name a known generated/playable animation, using
  the same playable animation contract as `POST /api/v1/play` and
  `POST /api/v1/notify`.
- `attributes.restore` must use the same restore vocabulary accepted by
  `POST /api/v1/play` and `POST /api/v1/notify`, such as `leave`,
  `previous_frame`, `background`, `clear`, or `blank`.
- `attributes.duration` must be a well-formed, non-negative duration.

Invalid known overrides return a client error before event publishing, so the
async event worker never sees those malformed playback intents. Unknown or
custom attributes remain schema-agnostic event data: they are not validated by
the generic event endpoint and are preserved for downstream event processing.
That includes namespaced custom attributes such as `param.*`.

### Desired Background Convergence

`/readyz.background` and matrix background observability are v1 contract-fixed; full
state transition semantics live in `docs/background-convergence-v1.md`.

The configured background is a scheduler-owned desired idle state, not an
ordinary queued playback item. Background restore work stays outside the play
queue, does not emit play-item outcomes, and is not counted in
`matrix_proxy_play_items_total`.

The scheduler tracks background convergence explicitly. A configured background
starts as `unknown`, becomes `dirty` when reconnects, playback, or direct
controls may have changed the display, moves to `attempting` while a restore is
in progress, and becomes `converged` only after the configured background has
been applied successfully. Restore failures keep `dirty: true`; retryable
failures and permanent failures with a pending retry deadline are reported as
`retrying`. The plain `dirty` state means restore is needed and no retry
deadline is currently suppressing another attempt and no failed retry state is
recorded. `failed` means the configured background is still dirty after a
failed restore and there is no future retry deadline suppressing attempts:
`next_retry` is absent, due, or already in the past. Dirty backgrounds remain
the scheduler's desired idle state and are retried by the scheduler at the next
eligible idle and connected opportunity.

Failed background restores do not retry on every scheduler heartbeat or every
notification completion. For v1, the background retry policy is intentionally
fixed scheduler behavior and is not configurable. Retryable failures use
exponential backoff from `1s` to `30s`; permanent failures use exponential
backoff from `30s` to `5m` and continue retrying forever with the capped
backoff. While `next_retry` is in the future, the state remains `retrying`: the
configured background is dirty, the scheduler is intentionally suppressing
another restore attempt until that deadline, and later redundant dirty triggers
cannot downgrade the state to `dirty`. Playback restore policies such as
`restore: background` before `next_retry` cannot bypass the deadline, change the
state away from `retrying`, or force another restore attempt. Once `next_retry`
is due or in the past, it is retained as the last scheduled retry timestamp and
the state becomes `failed` until the scheduler starts another restore attempt.
A successful scheduler-owned background restore is the normal path that marks a
dirty configured background clean.

There is one conservative duplicate-suppression case: when
`restore: previous_frame` successfully restores a display state explicitly
known to match the configured background, the scheduler may mark the desired
background clean/converged and skip a later duplicate idle background restore.
That is playback-restore convergence, not a scheduler-owned background restore
command. It does not update `/readyz.background.last_success` and does not
increment scheduler-owned background restore attempt counters; this v1
Prometheus contract does not expose a background restore success counter.
Operators should use `/readyz.background` for current convergence state;
Prometheus background restore counters represent scheduler-owned restore
commands only.

Verified reconnect recovery and successful direct display controls reset the
retry delay so the scheduler can make one prompt background restore attempt
when the display returns to idle. That reset only affects retry timing: dirty
state correctness is preserved, and the background remains dirty until the
configured background is actually applied successfully.

`GET /readyz` includes a `background` object, also referred to as
`/readyz.background` in operator docs and tests, with:

- `configured_id`: configured background animation ID, omitted when no
  background is configured.
- `kind`: `firmware_preset` or `generated`, omitted when no background is
  configured.
- `state`: one of `unknown`, `dirty`, `attempting`, `converged`, `failed`, or
  `retrying`. `retrying` means `dirty: true`, `next_retry` is pending in the
  future, and another restore attempt is intentionally suppressed until then.
  `dirty` means restore is needed but no retry deadline is currently
  suppressing attempts and no failed retry state is recorded. `failed` means
  the configured background is dirty after a failed restore, and `next_retry`
  is absent, due, or in the past.
- `dirty`: whether the desired background still needs to be restored.
- `converged`: whether the configured background is currently known to be
  applied.
- `last_attempt`: timestamp of the last restore attempt.
- `last_success`: timestamp of the last successful restore.
- `last_error`: last restore error text.
- `last_error_class`: bounded class `none`, `retryable`, or `permanent`.
- `next_retry`: timestamp of the last scheduled retry. It is retained when due
  or in the past, and is omitted when no retry has been scheduled or after
  convergence/reset.
- `failure_count`: consecutive failures for the current bounded error class,
  reset after successful restore, verified reconnect recovery, and successful
  direct display controls.

Readiness still uses the existing top-level policy: workers must be running,
the app must not be draining, and the matrix must be connected. A dirty,
attempting, failed, or retrying background is visible in `/readyz.background`
but does not by itself make `/readyz` return HTTP 503.

Prometheus exposes background restore telemetry separately from playback. The
event counters are:

- `matrix_proxy_background_restore_attempts_total{kind}`: scheduler-owned
  desired-background restore attempts by bounded kind.
- `matrix_proxy_background_restore_failures_total{kind,error_class}`:
  scheduler-owned desired-background restore failures by bounded kind and
  bounded error class.

The current-state gauges are:

- `matrix_proxy_background_dirty{kind}`: `1` when the configured background is
  currently dirty, otherwise `0`.
- `matrix_proxy_background_converged{kind}`: `1` only when the configured
  background is known converged, otherwise `0`.
- `matrix_proxy_background_next_retry_seconds{kind}`: seconds until the next
  background retry for the bounded kind, or `0` when no retry is pending, due,
  or in the past. `/readyz.background.next_retry` may still expose the retained
  due or past timestamp.
- `matrix_proxy_background_state{kind,state}`: one-hot current background state
  with bounded `state` values `unknown`, `dirty`, `attempting`, `converged`,
  `failed`, and `retrying`. This gauge reports `retrying`, not `dirty`, while
  `next_retry` is pending in the future, and reports `failed` after a failed
  restore when the retry deadline is due or not pending.

The `kind` label is bounded to `firmware_preset` and `generated`; background
IDs are not metric labels. `failure_count` is available
only in `/readyz.background`; there is intentionally no Prometheus
failure-count metric in this v1 contract. Background restore attempts, failures,
and state gauges are independent of ordinary playback outcome metrics, so
scheduler-owned restore work remains separate from `matrix_proxy_play_items_total`.

## Manual Hardware Validation

When validating hardware manually, remember that an enabled configured
background owns the eventual idle matrix state. To keep fill, clear, or preset
commands visible indefinitely during hardware checks, either set
`background.restore_on_idle: false` or change `background.animation` to the
state you want to validate as the idle background. Re-enable or restore the
production background before unattended deployment.
