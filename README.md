# light-udp-proxy

A high-performance UDP proxy written in Go, designed to complement HAProxy in environments where the open-source edition does not support UDP proxying.

---

## Features

| Feature | Details |
|---|---|
| **Frontends** | Bind to one or more UDP ports, apply routing rules |
| **Backend pools** | Group multiple servers with weight-based load balancing |
| **Load balancing** | `round_robin`, `least_conn`, `random`, `hash` (client-IP) |
| **Session affinity** | Sticky sessions keyed on `client_ip + client_port + frontend` |
| **Health checking** | Periodic UDP probes; automatic server removal & re-addition |
| **Metrics** | HTTP JSON endpoint at `/metrics` with packet/byte/session counters |
| **Hot config reload** | Send `SIGHUP` to reload the configuration without downtime |
| **Graceful shutdown** | `SIGINT`/`SIGTERM` drains in-flight work before exiting |
| **High performance** | `sync.Pool` buffer reuse, tunable OS socket buffers, multi-worker read loops |
| **Structured logging** | JSON log output at `debug`, `info`, `warn`, `error` levels |
| **Packet validation** | Configurable per-frontend maximum packet size; oversized packets are dropped |
| **Session limits** | Per-frontend maximum concurrent session cap |
| **Traffic shaping** | Token-bucket shaping at global/frontend/backend/per-client levels |
| **QoS priorities** | `critical`, `high`, `normal`, `low`, `bulk` frontend priorities |
| **Abuse protection** | Lightweight per-IP packet-rate and active-session limits |
| **Fragment handling** | Optional fragmented IPv4 packet dropping |

---

## Architecture

```
Client UDP traffic
       │
       ▼
 ┌─────────────┐
 │  Frontend   │  binds a UDP port, tracks client sessions
 └──────┬──────┘
        │  picks server via load-balancing algorithm
        ▼
 ┌─────────────┐
 │Backend Pool │  weighted list of servers, health-checked
 └──────┬──────┘
        │
  ┌─────┴──────┐
  │            │
  ▼            ▼
Server 1    Server 2   ...
```

---

## Requirements

* Go 1.24 or later

---

## Build

```bash
git clone https://github.com/ErwinsExpertise/light-udp-proxy.git
cd light-udp-proxy
go build -o udp-proxy ./cmd/udp-proxy
```

---

## Run

```bash
./udp-proxy -config config.example.yaml
```

Available flags:

| Flag | Description |
|---|---|
| `-config` | Path to YAML configuration file (required) |
| `-version` | Print version, commit, and build date, then exit |

---

## Configuration

See [config.example.yaml](config.example.yaml) for a fully annotated example.

### `global`

| Key | Default | Description |
|---|---|---|
| `max_packet_size` | `65535` | Maximum UDP payload size in bytes |
| `worker_threads` | `4` | Goroutines per frontend reading from the socket |
| `session_timeout` | `60s` | Idle session expiry duration |
| `session_cleanup_interval` | `10s` | How often the background reaper evicts expired sessions |
| `log_level` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `metrics_addr` | `0.0.0.0:9090` | HTTP metrics listen address |
| `socket.rcvbuf` | `0` | SO_RCVBUF kernel receive buffer in bytes (`0` = OS default) |
| `socket.sndbuf` | `0` | SO_SNDBUF kernel send buffer in bytes (`0` = OS default) |
| `socket.reuse_port` | `false` | SO_REUSEPORT (Linux): each worker goroutine gets its own socket for kernel-level multi-core distribution |
| `traffic_shaping.*` | – | Global token-bucket shaping (`packets_per_second`, `bytes_per_second`, `burst_packets`) |
| `client_limits.*` | – | Per-client IP shaping (`packets_per_second`, `burst_packets`) |
| `abuse_protection.*` | – | Per-IP temporary flood protection (`max_packets_per_second_per_ip`, `max_sessions_per_ip`) |
| `fragmentation.drop_fragments` | `false` | Drop IPv4 fragmented packets |

`bytes_per_second` and `burst_bytes` accept suffixes: decimal (`KB`, `MB`, `GB`, `TB`) and binary (`KiB`, `MiB`, `GiB`, `TiB`).

### `frontends[]`

| Key | Required | Description |
|---|---|---|
| `name` | ✅ | Unique frontend name |
| `listen` | ✅ | `host:port` to bind |
| `backend` | ✅ | Name of the backend pool |
| `session_affinity` | – | Sticky session routing (default `false`) |
| `max_sessions` | – | Maximum concurrent sessions (`0` = unlimited) |
| `max_packet_size` | – | Override global max packet size |
| `priority` | – | `critical` \| `high` \| `normal` \| `low` \| `bulk` (default `normal`) |
| `traffic_shaping.*` | – | Frontend-level token-bucket limits |

### `backends[]`

| Key | Required | Description |
|---|---|---|
| `name` | ✅ | Unique backend name |
| `load_balance` | – | `round_robin` (default) \| `least_conn` \| `random` \| `hash` |
| `health_check.enabled` | – | Enable periodic probes (default `false`) |
| `health_check.interval` | – | Probe interval (default `10s`) |
| `health_check.timeout` | – | Probe timeout (default `2s`) |
| `traffic_shaping.*` | – | Backend-level token-bucket limits |
| `servers[].address` | ✅ | `host:port` of the backend server |
| `servers[].weight` | – | Relative weight for weighted load balancing (default `1`) |

---

## Metrics

The `/metrics` endpoint returns JSON:

```json
{
  "packets_received": 1000000,
  "packets_forwarded": 999500,
  "packets_dropped": 500,
  "packets_shaped": 120,
  "packets_dropped_rate_limit": 230,
  "packets_dropped_fragment": 40,
  "packets_dropped_abuse": 70,
  "bytes_in": 524288000,
  "bytes_out": 524032000,
  "active_sessions": 4200,
  "backends": [
    { "address": "10.0.0.10:27015", "healthy": true,  "active_conns": 0 },
    { "address": "10.0.0.11:27015", "healthy": false, "active_conns": 0 }
  ]
}
```

A `/healthz` endpoint returns `ok` (HTTP 200) when the proxy process is alive.

---

## Signals

| Signal | Action |
|---|---|
| `SIGINT` / `SIGTERM` | Graceful shutdown |
| `SIGHUP` | Hot reload configuration |

---

## Releases

Pushing a tag that matches `v*` runs GoReleaser to build and publish a GitHub Release with binaries for:

| OS | Architectures |
|---|---|
| Linux | amd64, arm64, armv6, armv7 |
| macOS | amd64, arm64 |
| FreeBSD | amd64, arm64 |
| Windows | amd64 |

Each release includes SHA-256 checksums and a `config.example.yaml`.

---

## Tests

```bash
go test ./...
```

## CI

GitHub Actions CI runs formatting checks (`gofmt -l`), `go vet ./...`, and `go test ./... -count=1` on pushes and pull requests.

---

## Project Structure

```
cmd/
  udp-proxy/
    main.go            # Entry point; accepts -config flag
internal/
  config/              # YAML config loader and validator
  session/             # UDP session table with TTL expiry
  backend/             # Server pool, load-balancing algorithms
  frontend/            # UDP listener, packet routing
  shaping/             # Token-bucket rate limiting
  qos/                 # Frontend priority handling
  abuse/               # Lightweight per-IP abuse protection
  fragment/            # Fragment detection helpers
  healthcheck/         # Periodic UDP health probes
  metrics/             # HTTP /metrics JSON endpoint
  proxy/               # Top-level orchestrator, signal handling
pkg/
  logger/              # Structured JSON logger
config.example.yaml    # Annotated example configuration
```

---

## Performance Notes

* Each frontend runs `worker_threads` concurrent `ReadFromUDP` goroutines to utilise multi-core systems.
* Packet buffers are managed via `sync.Pool` to minimise allocations in the hot path.
* Session tracking uses a **256-shard striped map** to eliminate global locking; each shard has its own `sync.RWMutex`.
* Atomic counters are used for all metrics to avoid lock overhead.
