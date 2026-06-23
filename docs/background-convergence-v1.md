# Matrix Background Convergence v1 Contract

The proxy reports desired-background convergence through a bounded, shared contract.
All users of this contract must only emit one of these states:

- `unknown`
- `dirty`
- `attempting`
- `converged`
- `failed`
- `retrying`

This is the v1 contract and must not be widened without a versioned migration.
`failed` remains user-facing.

## Shared projection

The internal scheduler tracks raw background restore state plus helper fields:

- raw convergence state (`unknown`, `dirty`, `attempting`, `converged`, `failed`, `retrying`)
- desired-background dirty flag
- background restore attempt/success/error metadata
- next retry timestamp and failure count

All three delivery channels must use one projection function:

- scheduler health (`matrix.Health.BackgroundConvergenceState`, `.BackgroundDirty`, `.BackgroundConverged`)
- `/readyz.background` response payload
- `matrix_proxy_background_state{kind,state}` metric one-hot

If projection inputs differ between channels, operators receive contradictory signals.
Public background kind and state surfaces must emit projected public vocabulary
only. The internal background kind `renderable` is scheduler/app-internal and
must not appear in `/readyz.background`, animation catalog entries, or
Prometheus background metric labels.

## Projection rules

For a health sample at time `now`:

1. `attempting` always wins while a restore command is actively running.
2. `converged` is visible only when raw state is converged and desired-background is clean.
3. If `dirty` is false and there is no raw converge signal, the raw state is returned.
4. A dirty background with future `next_retry` reports `retrying`.
5. A dirty background without suppression and with failure evidence (`failure_count > 0`,
   restore error text, or `last_restore_error_class` retry/permanent) reports `failed`.
6. Otherwise a dirty background reports `dirty`.

## Invariants

- `retrying` means the scheduler is intentionally suppressing immediate restore attempts.
- `failed` means dirty background is due for restoration and not currently suppressed.
- `dirty` means desired background is not yet known converged and not in a suppressed retry window.
- `converged` is the only clean state.

`failed` is intentionally retained when `next_retry` reaches due/past and the scheduler has
not yet started another restore attempt; `next_retry` remains retained for visibility.

## Retry behavior

The scheduler uses fixed v1 behavior (no config knob):

- retryable failures: exponential backoff `1s -> 30s`
- permanent failures: exponential backoff `30s -> 5m`
- forever retry after reaching each cap until a successful restore, verified reconnect recovery,
  or successful direct display control resets the retry suppression.

Background restore failures are surfaced independently from playback metrics.

## Previous-frame duplicate suppression telemetry

When `restore: previous_frame` successfully restores a display state that is
explicitly known to match the configured background, the scheduler may mark the
desired background clean/converged and suppress a duplicate idle background
restore. This is playback-restore convergence, not a scheduler-owned
background restore command.

That path does not update background restore attempt/success metadata and does
not increment scheduler-owned background restore attempt or failure counters.
The v1 Prometheus contract does not expose a background restore success counter.
Operators should use `/readyz.background` for current convergence state.
Prometheus background restore counters represent scheduler-owned restore
commands only.

## Exported channels

- `/readyz` exposes `background` object fields:
  - `configured_id`, `kind`, `state`, `dirty`, `converged`, `last_attempt`,
    `last_success`, `last_error`, `last_error_class`, `next_retry`,
    `failure_count`
- `matrix_proxy_background_state{kind,state}` exposes the one-hot projected state.

Top-level `/readyz` still returns HTTP 200 when workers are running and matrix is connected,
regardless of dirty/attempting/failed/retrying background state.

The public background `kind` vocabulary is bounded to `generated` for
generated backgrounds and `firmware_preset` for firmware preset backgrounds.
The same vocabulary is used by `/readyz.background.kind`, animation catalog
entries, and background metric `kind` labels. The internal kind `renderable` is
not part of the public contract.

## Animation discovery

`GET /api/v1/animations` is the backward-compatible playable list. It returns
`{"animations":[...]}` with only generated animation IDs that may be submitted to
playback endpoints.

`GET /api/v1/animations/catalog` is the structured catalog. It returns entries
with the stable required fields `id`, `kind`, and `playable`. Generated entries
use public kind `generated`, have `playable: true`, and omit firmware metadata
fields. Firmware preset entries use public kind `firmware_preset`, have
`playable: false`, and may include only the bounded optional metadata fields
`effect_id`, `interval`, and `color` when configured. `effect_id` is a JSON
number, `interval` is a JSON string duration such as `"90ms"`, and `color` is a
structured RGB object.

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
      "playable": false,
      "effect_id": 12,
      "interval": "90ms",
      "color": {
        "r": 0,
        "g": 255,
        "b": 85
      }
    }
  ]
}
```

Firmware preset entries are metadata-only and background-safe. They are exposed
in the catalog with `kind: "firmware_preset"` and `playable: false`, but must
not be submitted to `POST /api/v1/play`, `POST /api/v1/notify`, or generic
`POST /api/v1/events` as `attributes.animation`.

## Animation config schema

The animation config file is additive under a top-level `animations` map. The
map key is the animation ID, and duplicate IDs across the merged registry are
rejected during config load.

Supported entry forms:

- `type: generated`: a generated alias. The `generator` field names an existing
  built-in app renderer, for example `notification`. These entries are
  playable generated animations.
- `type: firmware_preset`: firmware metadata for scheduler-owned background
  use. These entries can carry `effect_id`, `interval`, and `color`, but are
  not ordinary playable animations.
- `type: frames`: a declarative 8x8 frame animation authored in display-space.
  The entry must define a `palette` whose keys are one-character symbols and
  whose values are colors, plus a non-empty `frames` list. Every frame must have
  a positive `delay` and exactly eight `rows`; every row must contain exactly
  eight symbols from the palette.

Frame animations are exposed publicly the same way as other generated
animations: `kind: "generated"` and `playable: true`. Firmware metadata remains
absent from generated entries, including frame animations. Firmware presets
remain `kind: "firmware_preset"` and `playable: false`.

Display-space frame rows are not pre-packed in config loading. They are packed
only when rendered output crosses the layout mapper boundary toward the matrix
physical chain order.

```yaml
animations:
  status_check:
    type: frames
    palette:
      ".": "#000000"
      G: "#00FF55"
      W: "#FFFFFF"
    frames:
      - delay: 120ms
        rows:
          - "........"
          - "......G."
          - ".....GG."
          - ".W..GG.."
          - ".WW.G..."
          - "..WWW..."
          - "...W...."
          - "........"
```

Config load rejects malformed dimensions, unknown palette symbols, empty frame
sets, missing or invalid palettes, missing/zero/negative/malformed frame
delays, references from rules to unknown or non-renderable animations, and
duplicate animation IDs.

## Generic event override validation

`POST /api/v1/events` keeps the event payload schema-agnostic except for known
playback override attributes. Before publishing an event to the asynchronous
event path, the endpoint validates these known override keys:

- `attributes.animation` must name a known generated/playable animation, using
  the same playable animation contract as `POST /api/v1/play` and
  `POST /api/v1/notify`.
- `attributes.restore` must use the same restore vocabulary accepted by
  `POST /api/v1/play` and `POST /api/v1/notify`, such as `leave`,
  `previous_frame`, `background`, `clear`, or `blank`.
- `attributes.duration` must be a well-formed, non-negative duration.

Invalid known overrides return a client error synchronously and are not
published to the async event worker. Unknown or custom attributes are preserved
without endpoint-level schema validation for downstream event processing,
including namespaced custom attributes such as `param.*`.
