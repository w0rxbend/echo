# echo

> **LED Matrix Proxy** — turn any event into a light show on your ESP8266 matrix display.

[![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Docker](https://img.shields.io/badge/Docker-ghcr.io%2Fw0rxbend%2Fecho-2496ED?logo=docker&logoColor=white)](https://github.com/w0rxbend/echo/pkgs/container/echo)
[![CI](https://github.com/w0rxbend/echo/actions/workflows/docker.yml/badge.svg)](https://github.com/w0rxbend/echo/actions/workflows/docker.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**echo** is a lightweight HTTP proxy that sits between your home automation, monitoring stack, or any webhook source and an ESP8266-based 8×8 LED matrix. Send a JSON event, watch the matrix light up.

```text
 Webhook / Home Assistant / n8n
           │
           ▼
    ┌─────────────┐   TCP / binary protocol   ┌──────────────┐
    │    echo     │ ──────────────────────────► │  ESP8266 LED │
    │  (Go proxy) │ ◄────────────────────────── │  Matrix 8×8  │
    └─────────────┘       auto-reconnect        └──────────────┘
           │
    Prometheus /metrics
    Swagger UI  /docs
    Readiness   /readyz
```

## Features

- **Event-driven** — POST a JSON event; rules decide which animation plays
- **Multi-device** — manage several matrices from one service, each with independent queues and backgrounds
- **Idle background** — set a per-device animation that the scheduler restores whenever the display goes idle
- **Config-authored animations** — write 8×8 pixel art in YAML, no code required
- **22 firmware presets** — trigger built-in ESP8266 effects (`matrix_rain`, `fire`, `rainbow`, `heartbeat` …) via API
- **Auto-reconnect** — robust TCP reconnect with exponential backoff and heartbeat probing
- **Prometheus metrics** — per-device counters, gauges, and histograms out of the box
- **Swagger UI** — interactive API explorer at `/docs`
- **ARM-ready** — multi-arch Docker images (`amd64` · `arm64` · `arm/v7`) for Raspberry Pi

## Quick Start

### Docker (recommended)

```bash
# 1. Create config files
cp configs/config.example.yaml configs/config.yaml
#    → edit matrix.host to your device IP

cp .env.example .env
#    → set MATRIX_PROXY_ADMIN_TOKEN=your-secret

# 2. Run
docker compose up -d

# 3. Verify
curl http://localhost:8080/healthz
# {"status":"ok"}
```

### Send your first notification

```bash
curl -X POST http://localhost:8080/api/v1/devices/living-room/notify \
  -H "Content-Type: application/json" \
  -d '{"message": "Hello!", "duration": "3s"}'
```

The matrix plays the notification animation, then returns to the configured idle background.

### Play a firmware preset

```bash
# Play matrix_rain by animation ID (look up effect config from registry)
curl -X POST http://localhost:8080/api/v1/devices/living-room/preset/matrix_rain_background \
  -H "Authorization: Bearer your-secret"

# Or send custom preset parameters directly
curl -X POST http://localhost:8080/api/v1/devices/living-room/matrix/preset \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"effect_id": 11, "interval": "80ms", "color": {"r": 255, "g": 40, "b": 0}}'
```

### Pull the pre-built image

```bash
docker pull ghcr.io/w0rxbend/echo:latest
```

## Configuration at a Glance

`configs/config.yaml` controls everything. The minimal required fields:

```yaml
devices:
  living-room:                    # device ID used in API paths
    host: "192.168.1.127"        # ESP8266 IP
    port: 7777
    background:
      animation: "matrix_rain_background"
      restore_on_idle: true

animations_file: "configs/animations.yaml"
rules_file:      "configs/rules.yaml"
```

Multiple devices, each with independent idle animations, queues, and connection settings:

```yaml
devices:
  living-room:
    host: "192.168.1.127"
    background:
      animation: "matrix_rain_background"
      restore_on_idle: true
  office:
    host: "192.168.1.128"
    layout:
      rotation: 90              # compensate for physical mounting
    background:
      animation: "amber_rain_background"
      restore_on_idle: true
```

See [`configs/config.example.yaml`](configs/config.example.yaml) for all options.

## Animations

Drop YAML into `configs/animations.yaml`. Three authoring styles:

**Pixel art frames** — draw your own 8×8 art with a palette:

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

**Firmware presets** — trigger built-in ESP8266 effects (`matrix_rain`, `fire`, `rainbow`, `heartbeat`, and 18 more):

```yaml
animations:
  matrix_rain_background:
    type: firmware_preset
    effect_id: 12
    interval: 90ms
    color: "#00FF55"
```

**Generated** — aliases for built-in app renderers:

```yaml
animations:
  alert_pulse:
    type: generated
    generator: notification
```

See [`configs/animations.example.yaml`](configs/animations.example.yaml) for a full library including `spinner`, `wipe_down`, `checkerboard`, `alert_blink`, and more.

## Rules

Rules map incoming events to animations. `configs/rules.yaml`:

```yaml
rules:
  - id: http_notify_default
    when:
      source: http
      type: notify
    play:
      animation: alert_pulse
      priority: 50
      duration: 2s
      restore: background     # ← return to idle background when done
```

## API

> **Interactive docs:** `http://localhost:8080/docs`

All device-specific endpoints are namespaced by device ID:

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/v1/devices/{device}/notify` | POST | Send a notification |
| `/api/v1/devices/{device}/events` | POST | Publish a generic event |
| `/api/v1/devices/{device}/play` | POST ¹ | Play a renderable animation |
| `/api/v1/devices/{device}/preset/{id}` | POST ¹ | Play a firmware preset by animation ID |
| `/api/v1/devices/{device}/background` | GET / PUT ¹ | Read or change the idle animation |
| `/api/v1/devices/{device}/queue` | GET / DELETE ¹ | Inspect or clear the play queue |
| `/api/v1/devices/{device}/matrix/*` | POST ¹ | Direct display controls |
| `/api/v1/animations` | GET | List playable animation IDs |
| `/api/v1/animations/catalog` | GET | Full animation catalog |
| `/api/v1/devices` | GET ¹ | List configured device IDs |
| `/openapi.json` | GET | OpenAPI 3.0 spec |
| `/metrics` | GET | Prometheus metrics |
| `/readyz` | GET | Readiness (per-device breakdown) |
| `/healthz` | GET | Liveness |

¹ Requires `Authorization: Bearer <token>` when bound to a non-loopback address.

### Change the idle background at runtime

```bash
curl -X PUT http://localhost:8080/api/v1/devices/living-room/background \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"animation": "amber_rain_background", "restore_on_idle": true}'
```

## Observability

### Prometheus

All metrics carry a `device` label. Scrape `/metrics` — key signals:

| Metric | What it tells you |
| --- | --- |
| `matrix_proxy_matrix_connected{device}` | Is the device reachable? |
| `matrix_proxy_play_queue_depth{device}` | Animations waiting to play |
| `matrix_proxy_background_state{device,kind,state}` | Idle background convergence |
| `matrix_proxy_events_total{source,type}` | Event throughput |
| `matrix_proxy_matrix_reconnects_total{device,source}` | Reconnect attempts |

### Optional observability stack

```bash
docker compose --profile observability up -d
# Prometheus → http://localhost:9090
# Grafana    → http://localhost:3000  (admin / admin)
```

### Readiness

```bash
curl http://localhost:8080/readyz | jq .devices
```

```json
{
  "living-room": {
    "scheduler_state": "ready",
    "matrix_connected": true,
    "background": {
      "state": "converged",
      "configured_id": "matrix_rain_background"
    }
  }
}
```

## Running Locally (without Docker)

```bash
go build -o bin/matrix-proxy ./cmd/matrix-proxy

MATRIX_PROXY_ADMIN_TOKEN=dev \
  ./bin/matrix-proxy -config configs/config.yaml -log-level debug
```

Full setup guide including native Go, Docker Compose, and hardware validation tips: [**RUNNING_LOCALLY.md**](RUNNING_LOCALLY.md).

## Development

```bash
go test ./...    # run all tests
go build ./...   # build check
```

For internal contracts, implementation notes, and Prometheus metric reference, see [**docs/dev-guide.md**](docs/dev-guide.md).

## License

MIT — see [LICENSE](LICENSE).
