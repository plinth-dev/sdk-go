# `github.com/plinth-dev/sdk-go/otel`

Plinth's OpenTelemetry SDK initialisation. One `Init` call wires the global tracer provider with Plinth's standard resource attributes (`service.name`, `service.version`, `module.name`, `deployment.environment`), the OTLP exporter, and the W3C trace-context propagator.

Design rationale: <https://plinth.run/sdk/go/otel/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/otel@latest
```

## Minimum example

```go
package main

import (
    "context"
    "log"
    "net/http"

    plinthotel "github.com/plinth-dev/sdk-go/otel"
)

func main() {
    shutdown, err := plinthotel.Init(context.Background(), plinthotel.Options{
        ServiceName:    "items-api",
        ServiceVersion: version, // injected via -ldflags '-X main.version=...'
        ModuleName:     "items",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer func() { _ = shutdown(context.Background()) }()

    mux := http.NewServeMux()
    mux.Handle("/", plinthotel.HTTPMiddleware(myHandler()))
    http.ListenAndServe(":8080", mux)
}
```

The package name is `otel` (matching its directory). To avoid colliding with the official `go.opentelemetry.io/otel` package, alias the import: `plinthotel`, `pothel`, or whatever fits your style.

## Defaults

- **`ExporterEndpoint`**: `http://otel-collector.observability:4318` (the in-cluster collector address).
- **`ExporterProtocol`**: `"http"` (OTLP/HTTP). Switch to `"grpc"` if you have gRPC available end-to-end.
- **`Environment`**: `os.Getenv("ENV")`, falling back to `"dev"`.
- **Sampling**: parent-based ratio, `0.05` in production / `0.5` in staging / `1.0` elsewhere. Override via `TracesSamplerArg` or env var `OTEL_TRACES_SAMPLER_ARG`.
- **Propagator**: W3C TraceContext + Baggage.

## Recording errors and module attributes

```go
import plinthotel "github.com/plinth-dev/sdk-go/otel"

// In a handler — tags the current span with the error and sets status.
if err := svc.Do(ctx, in); err != nil {
    plinthotel.RecordError(ctx, err)
    return err
}

// Manual attribute on a custom span.
ctx, span := tracer.Start(ctx, "items.publish",
    trace.WithAttributes(plinthotel.AttrModule("items")))
defer span.End()
```

## Testing

`Options.Exporter` accepts any `sdktrace.SpanExporter`. Pass `tracetest.NewInMemoryExporter()` for unit tests; `Init` automatically uses a `SimpleSpanProcessor` (synchronous flush) when `Exporter` is set, so spans are visible immediately after `span.End()`.

```go
import (
    plinthotel "github.com/plinth-dev/sdk-go/otel"
    "go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSomething(t *testing.T) {
    rec := tracetest.NewInMemoryExporter()
    shutdown, _ := plinthotel.Init(context.Background(), plinthotel.Options{
        ServiceName:      "test",
        Exporter:         rec,
        TracesSamplerArg: 1.0, // pin sampler so single-span tests aren't dropped
    })
    defer shutdown(context.Background())

    // … emit a span …

    spans := rec.GetSpans()
    // … assert …
}
```

**Don't call `shutdown()` before reading spans** — `tracetest.InMemoryExporter.Shutdown` calls `Reset()` and clears recorded spans. Defer it; it runs after the assertions.

## Compatibility

- **Go 1.23+**.
- Wraps `go.opentelemetry.io/otel` (v1.43+) and `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` (v0.68+).
- Uses semconv v1.40.0 (matches the SDK's default detector schema URL).

## License

MIT — see [LICENSE](./LICENSE).
