# 3xui-exporter

A production-ready, open-source [Prometheus](https://prometheus.io/) exporter for the
[3X-UI](https://github.com/MHSanaei/3x-ui) VPN management panel and its underlying
[Xray-core](https://github.com/XTLS/Xray-core) engine.

Instead of tailing fragile log files, this exporter polls two JSON endpoints **at scrape
time** (only when Prometheus asks), keeping load on your panel proportional to the scrape
interval:

1. **3X-UI Panel API** — authenticates with a session cookie and reads inbound / per-client
   traffic accounting.
2. **Xray-core expvar API** — reads `observatory` outbound health and Go runtime memstats
   from `/debug/vars`.

## Metrics

### Panel metrics (`xui_`)

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_users_online_total` | Gauge | – | Concurrent online clients across all inbounds |
| `xui_user_traffic_used_bytes` | Gauge | `email`, `inbound_tag` | Bytes used (up+down) by a client |
| `xui_user_traffic_limit_bytes` | Gauge | `email`, `inbound_tag` | Data limit in bytes (0 = unlimited) |
| `xui_user_expire_timestamp` | Gauge | `email`, `inbound_tag` | Unix expiry timestamp in seconds (0 = never) |
| `xui_user_enabled` | Gauge | `email`, `inbound_tag` | 1 if the client is enabled |
| `xui_inbound_up_bytes` | Gauge | `inbound_tag`, `protocol` | Uplink bytes for an inbound |
| `xui_inbound_down_bytes` | Gauge | `inbound_tag`, `protocol` | Downlink bytes for an inbound |

### Xray engine metrics (`xray_`)

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xray_observatory_outbound_alive` | Gauge | `outbound_tag` | 1 if outbound is healthy |
| `xray_observatory_outbound_delay_ms` | Gauge | `outbound_tag` | Outbound RTT latency (ms) |
| `xray_core_memory_alloc_bytes` | Gauge | – | Allocated heap bytes (`memstats.Alloc`) |
| `xray_core_memory_sys_bytes` | Gauge | – | Memory obtained from OS (`memstats.Sys`) |
| `xray_core_memory_heap_inuse_bytes` | Gauge | – | In-use heap bytes (`memstats.HeapInuse`) |
| `xray_core_gc_total` | Counter | – | Completed GC cycles (`memstats.NumGC`) |

### Exporter health

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `xui_exporter_up` | Gauge | – | 1 if the last scrape of all sources succeeded |
| `xui_exporter_scrape_errors_total` | Counter | `source` | Failed sources during the scrape |

## Configuration

Configuration is via environment variables only.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `XUI_URI` | yes | – | Panel base URL, e.g. `http://127.0.0.1:2053` (include the web base path if set) |
| `XUI_API_TOKEN` | one of* | – | Panel API token (Bearer). **Recommended** for v3+. From Settings → Security → API Token |
| `XUI_USERNAME` | one of* | – | Panel username (used only if `XUI_API_TOKEN` is empty) |
| `XUI_PASSWORD` | one of* | – | Panel password (used only if `XUI_API_TOKEN` is empty) |
| `XRAY_METRICS_URI` | no | – | Xray expvar URL, e.g. `http://127.0.0.1:11111/debug/vars` (empty disables) |
| `LISTEN_ADDR` | no | `:9808` | Exporter bind address |
| `METRICS_PATH` | no | `/metrics` | Metrics HTTP path |
| `HTTP_TIMEOUT` | no | `10s` | Per-request timeout |
| `INSECURE_SKIP_VERIFY` | no | `false` | Skip TLS verification (self-signed panel certs) |

\* Configure **either** `XUI_API_TOKEN` **or** both `XUI_USERNAME` + `XUI_PASSWORD`.

### Authentication (3X-UI v3+)

The panel supports two auth modes; this exporter handles both:

- **API token (recommended).** Create one in the panel under **Settings → Security → API Token** and set `XUI_API_TOKEN`. It is sent as `Authorization: Bearer <token>` on every `/panel/api/*` request and **bypasses the panel's CSRF protection**, so no `/login` round-trip happens. When set, the username/password are ignored.
- **Username / password.** If you only set `XUI_USERNAME`/`XUI_PASSWORD`, the exporter logs in via `/login`. Because v3 guards `/login` (and unsafe API calls) with CSRF, the exporter automatically mints a token from `/csrf-token` and replays it in the `X-CSRF-Token` header. (A bare username/password POST without this returns **HTTP 403**.)

> If your panel uses a custom web base path, include it in `XUI_URI` (e.g. `https://host:2053/myrandompath`). Otherwise requests hit `404`.

## Running

### From source

```bash
go build -o 3xui-exporter .
XUI_URI=http://127.0.0.1:2053 \
XUI_USERNAME=admin \
XUI_PASSWORD=admin \
XRAY_METRICS_URI=http://127.0.0.1:11111/debug/vars \
./3xui-exporter
```

Then scrape `http://localhost:9808/metrics`.

### Docker

```bash
docker build -t 3xui-exporter .
docker run --rm -p 9808:9808 \
  -e XUI_URI=http://host.docker.internal:2053 \
  -e XUI_USERNAME=admin \
  -e XUI_PASSWORD=admin \
  -e XRAY_METRICS_URI=http://host.docker.internal:11111/debug/vars \
  3xui-exporter
```

### Prometheus scrape config

```yaml
scrape_configs:
  - job_name: 3xui
    static_configs:
      - targets: ["localhost:9808"]
```

## Enabling the Xray expvar endpoint

Xray-core must be started with the `metrics` (and optionally `observatory`) features so
that `/debug/vars` is published. Add a metrics inbound/tag to your Xray config, e.g.:

```json
{
  "metrics": { "tag": "metrics_out" },
  "stats": {},
  "policy": {
    "system": { "statsInboundUplink": true, "statsInboundDownlink": true }
  }
}
```

and route the metrics tag to a `dokodemo-door` inbound bound to `127.0.0.1:11111`. See the
Xray-core docs for the exact configuration matching your version.

## Project layout

```
config/      Environment-variable configuration loader
client/      HTTP clients
  xui.go       3X-UI panel API (cookie auth + auto re-auth on 401)
  xray.go      Xray-core expvar fetch + parse
collector/   prometheus.Collector implementation (scrape-time polling)
main.go      Entrypoint and HTTP server
Dockerfile   Multi-stage, distroless, static binary build
```

## Extending

The codebase is intentionally modular so contributors can add new data points:

- **New panel endpoint:** add a response struct and a method to `client/xui.go`, then emit
  the metric in `collector.collectXUI`.
- **New Xray metric:** extend the structs in `client/xray.go` and emit it in
  `collector.collectXray`.
- **New metric descriptor:** add a `*prometheus.Desc` field in `collector/collector.go`,
  list it in `Describe`, and populate it in the relevant `collect*` method.

## License

MIT
