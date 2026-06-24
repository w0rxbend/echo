# Developer Guide

Internal contracts, implementation notes, and observability details for contributors and operators who need to understand the internals.

---

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

---

## Animation Configuration

`configs/config.example.yaml` enables a background named `matrix_rain_background`
and loads it from `configs/animations.example.yaml`. That entry is a
`firmware_preset`, so the rain effect ID, interval, and color are configuration
metadata instead of app-level special cases. Firmware preset entries are
background-only metadata; they are not generated animations and cannot be used
as ordinary rule `play.animation` values or direct playback requests.

Animation config is strict and additive under the top-level `animations` map.
Each key is the registry ID and must be unique after all animation sources are
merged. Duplicate YAML keys in `animations.yaml` are rejected before normal
decoding can collapse them; this covers duplicate fields at the document root,
duplicate animation IDs under `animations`, duplicate fields inside an
animation entry, duplicate fields inside frame objects, palette symbols, and
duplicate color channels in palette colors or firmware preset `color` values.
Unknown or misspelled keys are also rejected during config load. This strict
schema applies only to operator-authored `animations.yaml`; generic event
attributes remain schema-agnostic.

The supported animation entry forms are:

- `type: generated` — alias for a built-in app renderer. Playable via rules,
  `/play`, `/notify`, and as backgrounds.
- `type: firmware_preset` — metadata for an on-device effect. Background-only;
  `playable: false`.
- `type: frames` — config-authored 8×8 frame animation. Rows are authored in
  display-space; the layout mapper handles physical LED conversion.

---

## Desired Background Convergence

Full state-transition semantics live in [`docs/background-convergence-v1.md`](background-convergence-v1.md).

The configured background is a scheduler-owned desired idle state, not an
ordinary queued playback item. Background restore work stays outside the play
queue and is not counted in `matrix_proxy_play_items_total`.

Convergence states: `unknown` → `dirty` → `attempting` → `converged`
(or `retrying` / `failed` on failures). Failed restores use exponential backoff:
retryable `1s→30s`, permanent `30s→5m` (forever).

### Prometheus Background Metrics

| Metric | Labels | Description |
| --- | --- | --- |
| `matrix_proxy_background_restore_attempts_total` | `device`, `kind` | Scheduler-owned restore attempts |
| `matrix_proxy_background_restore_failures_total` | `device`, `kind`, `error_class` | Restore failures |
| `matrix_proxy_background_dirty` | `device`, `kind` | `1` when background needs restoring |
| `matrix_proxy_background_converged` | `device`, `kind` | `1` when background is applied |
| `matrix_proxy_background_next_retry_seconds` | `device`, `kind` | Seconds until next retry |
| `matrix_proxy_background_state` | `device`, `kind`, `state` | One-hot convergence state |

---

## Manual Hardware Validation

When validating hardware manually, remember that an enabled configured
background owns the eventual idle matrix state. To keep fill, clear, or preset
commands visible indefinitely during hardware checks, set
`background.restore_on_idle: false` or change `background.animation` to the
state you want to validate. Re-enable the production background before
unattended deployment.

---

## Related Docs

- [`docs/event-bus-contract.md`](event-bus-contract.md) — v1 event bus delivery contract
- [`docs/background-convergence-v1.md`](background-convergence-v1.md) — background state machine
- [`RUNNING_LOCALLY.md`](../RUNNING_LOCALLY.md) — local development and Docker guide
