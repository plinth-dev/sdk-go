# `github.com/plinth-dev/sdk-go/audit`

Plinth's non-blocking audit emission. Wraps a pluggable `Producer` with a buffered, draining `Publisher` so the request path never waits on audit ingestion.

Design rationale: <https://plinth.run/sdk/go/audit/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/audit@latest
```

## Minimum example

```go
import (
    "context"
    "github.com/plinth-dev/sdk-go/audit"
)

func main() {
    // Tests use MemoryProducer; production wires a NATS / Kafka / SigNoz
    // producer (implement Producer interface; ships separately).
    pub := audit.New(audit.Options{
        Producer:    audit.NewMemoryProducer(),
        ServiceName: "items-api",
    })
    defer pub.Close(context.Background())

    // From a handler, after a successful mutation:
    pub.Publish(ctx, audit.Event{
        Actor:    audit.Actor{ID: userID, Type: "user", Roles: []string{"editor"}},
        Action:   "items.update",
        Resource: audit.Resource{Kind: "Item", ID: itemID},
        Outcome:  audit.OutcomeSuccess,
        Severity: audit.SeverityInfo,
        Before:   beforeSnapshot,
        After:    afterSnapshot,
    })
}
```

## Behaviour

- **Publish never blocks the request path.** Returns synchronously. If the buffer is full (default 1024), drops the oldest event with a `slog.Error` and increments `Stats.Dropped`. Monitor on a non-zero drop rate.
- **CloudEvents 1.0 envelope.** The `Producer` sees a `CloudEvent` with id (UUIDv7), source (`plinth.run/<service>`), type (`plinth.audit.<action>.v1`), time, datacontenttype, and `data` (the platform `Event`).
- **Drain on Close.** `Close(ctx)` waits up to `DrainTimeout` (default 5s) for the queue to drain, then closes the underlying Producer. Returns an error if drain timed out (events lost, count logged).
- **Trace-ID injection.** If `Event.TraceID` is empty and `TraceIDFunc` is set, the publisher populates it from the request context. Pre-set TraceIDs are preserved.
- **Required Reason.** `OutcomeDenied` or `OutcomeError` with empty `Reason` logs a `slog.Warn` but still publishes.

## Plugging in OpenTelemetry

This package is dependency-free; trace ID extraction is a callback:

```go
import "go.opentelemetry.io/otel/trace"

pub := audit.New(audit.Options{
    Producer:    prod,
    ServiceName: "items-api",
    TraceIDFunc: func(ctx context.Context) string {
        if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
            return span.SpanContext().TraceID().String()
        }
        return ""
    },
})
```

## Compatibility

- **Go 1.23+**.
- **No external dependencies.** Pure standard library. UUIDv7 generated inline.
- A NATS-backed `Producer` ships in a follow-on once Phase D's NATS chart is wired. Until then, modules either use `MemoryProducer` (tests) or implement their own `Producer` (production).

## License

MIT — see [LICENSE](./LICENSE).
