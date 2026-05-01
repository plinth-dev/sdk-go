# `github.com/plinth-dev/sdk-go/health`

Plinth's dependency-probe registry. Register one probe per dependency; the registry runs them in parallel on every request, returns 200 (OK or Degraded) or 503 (Failing). Liveness and readiness are deliberately separate handlers.

Design rationale: <https://plinth.run/sdk/go/health/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/health@latest
```

## Minimum example

```go
import (
    "net/http"
    "github.com/plinth-dev/sdk-go/health"
)

func main() {
    reg := health.New()
    reg.Register(health.PgPing("postgres", db))
    reg.Register(health.HTTPGet("items-api", "http://items-api/livez", 1*time.Second))
    reg.Register(health.CerbosCheck("cerbos", cerbosClient))
    reg.Register(health.Func("kafka-stream", func(ctx context.Context) error {
        return kafka.IsReady(ctx, "items-events")
    }))

    mux := http.NewServeMux()
    mux.Handle("/health", reg.HTTPHandler())   // K8s readiness — runs probes
    mux.Handle("/livez",  reg.LivenessHandler()) // K8s liveness — always 200
    http.ListenAndServe(":8080", mux)
}
```

A request to `/health` returns:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "status": "ok",
  "results": [
    {"name": "postgres",     "status": "ok",       "latency_ms":  3},
    {"name": "items-api",    "status": "ok",       "latency_ms": 12},
    {"name": "cerbos",       "status": "ok",       "latency_ms":  1},
    {"name": "kafka-stream", "status": "degraded", "latency_ms":  8, "detail": "rebalancing"}
  ]
}
```

## Behaviour

- **Probes run in parallel.** Total latency = max(probe latency), not sum.
- **Per-probe timeout via context.** Default 2s per probe; configurable via `WithProbeTimeout`. Slow probes are reported as `Failing` with `Detail: "timeout"`.
- **Aggregate is the worst.** Any `Failing` → aggregate `Failing` (HTTP 503). Otherwise any `Degraded` → aggregate `Degraded` (HTTP 200). Otherwise `OK` (HTTP 200).
- **Readiness ≠ liveness.** `HTTPHandler` is for K8s readiness (process is ready to serve traffic; fail when deps are down). `LivenessHandler` is for K8s liveness (process is alive; never fails on dep state). Wiring them on the same path will cause the kubelet to restart pods on transient dep blips.

## Built-in probes

- `PgPing(name, db)` — calls `db.PingContext(ctx)`. Works for both `database/sql.DB` and `pgxpool.Pool`.
- `HTTPGet(name, url, timeout)` — GET. 2xx → OK; 5xx → Failing; other non-2xx → Degraded.
- `CerbosCheck(name, pinger)` — calls `pinger.Ping(ctx)`. The `sdk-go/authz` Client satisfies the Pinger interface; we accept the interface to avoid a package cycle.
- `Func(name, fn)` — wraps a closure. Use for bespoke dependencies.

## Compatibility

- **Go 1.23+**.
- **No external dependencies.** Pure standard library.

## License

MIT — see [LICENSE](./LICENSE).
