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
  - `state`, `dirty`, `converged`, `last_attempt`, `last_success`, `last_error`,
    `last_error_class`, `next_retry`, `failure_count`
- `matrix_proxy_background_state{kind,state}` exposes the one-hot projected state.

Top-level `/readyz` still returns HTTP 200 when workers are running and matrix is connected,
regardless of dirty/attempting/failed/retrying background state.

## Animation discovery

`GET /api/v1/animations` is the backward-compatible playable list. It returns
`{"animations":[...]}` with only renderable animation IDs that may be submitted
to playback endpoints.

`GET /api/v1/animations/catalog` is the structured catalog. It returns entries
with the stable fields `id`, `kind`, and `playable`:

```json
{
  "animations": [
    {
      "id": "notification",
      "kind": "renderable",
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

Firmware preset entries are metadata-only and background-safe. They are exposed
in the catalog with `kind: "firmware_preset"` and `playable: false`, but must
not be submitted to `POST /api/v1/play`, `POST /api/v1/notify`, or generic
`POST /api/v1/events` as `attributes.animation`.
