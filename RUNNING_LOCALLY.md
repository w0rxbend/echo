# Running Locally

This service (`matrix-proxy`) proxies HTTP events and rules to an ESP8266 LED matrix controller over a persistent TCP connection. It starts up and accepts HTTP requests even when the matrix is unreachable — it will keep reconnecting in the background.

## Prerequisites

### Native (Go)
- Go 1.23+

### Docker
- Docker 24+
- Docker Compose v2 (the `docker compose` plugin, not `docker-compose`)

---

## 1. Configuration

All three config files live under `configs/`. The service reads YAML — no environment variable interpolation in the config files themselves.

### Step 1 — create `configs/config.yaml`

```bash
cp configs/config.example.yaml configs/config.yaml
```

Edit the key fields:

| Field | Default | Notes |
|---|---|---|
| `server.addr` | `:8080` | HTTP listen address. Empty host (`:8080`) is **not** loopback — admin token required. |
| `server.admin_token_env` | `MATRIX_PROXY_ADMIN_TOKEN` | Name of the env var that holds the bearer token for admin endpoints. |
| `matrix.host` | `192.168.1.127` | IP of your ESP8266. The service will retry if unreachable. |
| `matrix.port` | `7777` | TCP port on the device. |
| `matrix.brightness` | `30` | 0–255. |
| `background.animation` | `matrix_rain_background` | Animation ID restored when idle. Must exist in `animations_file`. |
| `background.restore_on_idle` | `true` | Set false to disable idle background restore during hardware testing. |
| `animations_file` | `configs/animations.example.yaml` | Path to animations YAML. Leave as-is or create your own. |
| `rules_file` | `configs/rules.example.yaml` | Path to rules YAML. Leave as-is or create your own. |

### Step 2 — (optional) customise animations and rules

The example files at `configs/animations.example.yaml` and `configs/rules.example.yaml` work out of the box. Copy and edit them only if you need custom animations or routing rules.

---

## 2. Running Natively

```bash
# Install dependencies
go mod download

# Build
go build -o ./bin/matrix-proxy ./cmd/matrix-proxy

# Run (reads configs/config.yaml by default)
./bin/matrix-proxy

# Or with flags
./bin/matrix-proxy -config configs/config.yaml -log-level debug
```

When `server.addr` resolves to a non-loopback address (anything other than `localhost` or `127.0.0.1`), the service requires the admin token env var to be set:

```bash
MATRIX_PROXY_ADMIN_TOKEN=your-secret ./bin/matrix-proxy
```

For pure local development on loopback, set `server.addr: "127.0.0.1:8080"` in `configs/config.yaml` — auth is then skipped entirely.

---

## 3. Running with Docker Compose

### Step 1 — set up environment

```bash
cp .env.example .env
# Edit .env and set a real MATRIX_PROXY_ADMIN_TOKEN
```

### Step 2 — copy and edit config

```bash
cp configs/config.example.yaml configs/config.yaml
# Set matrix.host to your ESP8266 IP (or leave default to run without hardware)
```

### Step 3 — start

```bash
docker compose up --build
```

The service is available at `http://localhost:8080`.

To run detached:

```bash
docker compose up --build -d
docker compose logs -f matrix-proxy
```

To stop:

```bash
docker compose down
```

### Optional: Observability stack (Prometheus + Grafana)

```bash
docker compose --profile observability up --build
```

- Prometheus: `http://localhost:9090`
- Grafana: `http://localhost:3000` (user `admin`, password from `GRAFANA_PASSWORD` in `.env`, default `admin`)

In Grafana, add a Prometheus data source pointing to `http://prometheus:9090` and start exploring metrics from the `matrix_proxy_*` namespace.

---

## 4. Running Without Hardware

The service starts and accepts all HTTP requests even without a matrix device. The TCP client retries the connection with exponential backoff (`reconnect_min_delay` → `reconnect_max_delay`).

The only observable difference is:

- `GET /healthz` → `200 OK` always (process liveness only)
- `GET /readyz` → `503` with `"matrix_connected": false` until connected
- Admin matrix control endpoints (`/matrix/clear`, `/matrix/fill`, etc.) will time out or return `503` while disconnected

Everything else — event publishing, notify, animation catalog — works normally without hardware. Play requests are queued and will execute once the scheduler connects.

To suppress verbose reconnect warnings while developing without hardware, set `log-level: warn` or `error`.

---

## 5. Endpoints

All endpoints are under the single HTTP server (`server.addr`).

### Health and observability

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/healthz` | — | Liveness. Always `200` while the process runs. |
| GET | `/readyz` | — | Readiness. `503` when matrix is disconnected or workers are not running. |
| GET | `/metrics` | — | Prometheus metrics (`matrix_proxy_*` namespace). |

### Animation and event API (`/api/v1/`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/api/v1/events` | — | Publish a generic event for async rule processing. |
| POST | `/api/v1/notify` | — | Send a notification (triggers `type: notify` rules). |
| GET | `/api/v1/animations` | — | List playable animation IDs. |
| GET | `/api/v1/animations/catalog` | — | Full animation catalog with kind, playability, and metadata. |
| POST | `/api/v1/play` | Admin | Enqueue an animation directly by ID. |
| GET | `/api/v1/queue` | Admin | Current scheduler queue depth and state. |
| DELETE | `/api/v1/queue` | Admin | Clear the play queue. |

### Matrix controls (`/api/v1/matrix/`)

All matrix control endpoints require the admin token.

| Method | Path | Description |
|---|---|---|
| POST | `/api/v1/matrix/clear` | Clear the display. |
| POST | `/api/v1/matrix/fill` | Fill with a solid RGB colour: `{"r":0,"g":255,"b":0}` |
| POST | `/api/v1/matrix/brightness` | Set brightness 0–255: `{"value":30}` |
| POST | `/api/v1/matrix/preset` | Apply a firmware effect: `{"effect_id":12,"interval":"90ms","color":{"r":0,"g":255,"b":85}}` |

### Authentication

Admin endpoints require `Authorization: Bearer <token>` when `server.addr` is not a loopback address. The token value comes from the env var named by `server.admin_token_env` (default `MATRIX_PROXY_ADMIN_TOKEN`).

```bash
curl -X POST http://localhost:8080/api/v1/play \
  -H "Authorization: Bearer your-secret" \
  -H "Content-Type: application/json" \
  -d '{"animation":"alert_pulse"}'
```

Auth is **not required** when `server.addr` is `127.0.0.1:port` or `localhost:port`.

---

## 6. Quick Smoke Test

```bash
# Liveness
curl http://localhost:8080/healthz

# Readiness (503 if matrix not connected)
curl http://localhost:8080/readyz

# List animations
curl http://localhost:8080/api/v1/animations

# Send a notification (triggers rules with source:http / type:notify)
curl -X POST http://localhost:8080/api/v1/notify \
  -H "Content-Type: application/json" \
  -d '{"message":"Hello"}'

# Publish a generic event
curl -X POST http://localhost:8080/api/v1/events \
  -H "Content-Type: application/json" \
  -d '{"source":"external","type":"notify"}'

# Prometheus metrics
curl http://localhost:8080/metrics | grep matrix_proxy
```

---

## 7. Key Prometheus Metrics

| Metric | Description |
|---|---|
| `matrix_proxy_matrix_connected` | `1` when TCP connection to the matrix is up |
| `matrix_proxy_event_queue_depth` | Events buffered in the worker channel |
| `matrix_proxy_event_worker_inflight` | `1` while an event is being processed |
| `matrix_proxy_play_items_total` | Play items by kind and outcome |
| `matrix_proxy_background_dirty{kind}` | `1` when background needs restoring |
| `matrix_proxy_background_converged{kind}` | `1` when background is applied |
| `matrix_proxy_background_state{kind,state}` | One-hot current background state |
| `matrix_proxy_matrix_commands_total` | TCP commands by command type and status |
| `matrix_proxy_matrix_reconnects_total` | Reconnect attempts by source and error kind |

---

## 8. Troubleshooting

**`MATRIX_PROXY_ADMIN_TOKEN is required` on startup**
The server address resolves to a non-loopback interface. Either set the env var or change `server.addr` to `127.0.0.1:8080` in the config for local-only access.

**`readyz` returns 503 with `matrix_connected: false`**
The service cannot reach the matrix device at `matrix.host:matrix.port`. This is normal when running without hardware. The scheduler retries automatically.

**Config load error on startup**
Check that `configs/config.yaml`, `animations_file`, and `rules_file` all exist and are valid YAML. Unknown or misspelled keys are rejected. Use the example files as a reference.

**`background.restore_on_idle` keeps overwriting manual matrix commands**
This is intentional — the configured background is the desired idle state. Set `restore_on_idle: false` in the config for unattended hardware testing, or set `background.animation` to the state you want to validate.
