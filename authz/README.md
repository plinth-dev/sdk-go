# `github.com/plinth-dev/sdk-go/authz`

Plinth's fail-closed Cerbos PDP client. Every authorization decision in a Plinth backend module flows through this package; modules never talk to Cerbos directly. The fail-closed contract: any failure (PDP unreachable, timeout, gRPC error, ctx-cancel) returns `Decision{Allowed: false, Reason: Unreachable}` — never an error to the caller.

Design rationale: <https://plinth.run/sdk/go/authz/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/authz@latest
```

## Minimum example

```go
import (
    "context"
    "github.com/plinth-dev/sdk-go/authz"
)

func main() {
    client, err := authz.New(context.Background(), authz.Options{
        Address: "cerbos.cerbos.svc:3593",
        EnvName: "production", // CERBOS_ALLOW_BYPASS=1 will be rejected here
    })
    if err != nil {
        log.Fatal(err) // ErrBypassInProduction is one possibility
    }
    defer client.Close()

    // Single-action check.
    d := client.CheckAction(ctx,
        authz.Principal{ID: "u1", Roles: []string{"editor"}},
        authz.Resource{Kind: "Item", ID: "i1"},
        "update")
    if !d.Allowed {
        // d.Reason is Allowed | Denied | Unreachable | Bypassed
        // for logs/audit, never bare bool.
        return apperrors.PermissionDenied(d.Action)
    }

    // Batched check — one round-trip for the layout's full permission set.
    perms := client.PermissionMap(ctx,
        authz.Principal{ID: "u1", Roles: []string{"editor"}},
        authz.Resource{Kind: "Item", ID: "i1"},
        []string{"read", "update", "delete", "comment"},
    )
    // perms is a map[string]bool; pass to @plinth-dev/authz-react.
}
```

## Behaviour

- **Fail-closed.** Any failure returns `Decision{Allowed: false, Reason: Unreachable}`. The caller writes one branch (`if !d.Allowed`) and never has to remember to check an error.
- **Bypass mode.** Setting env `CERBOS_ALLOW_BYPASS=1` makes every `CheckAction*` return `{Allowed: true, Reason: Bypassed}` and emits a `slog.Warn` per call. **`New` refuses to construct a Client with bypass enabled when `EnvName="production"`** — startup fails with [`ErrBypassInProduction`]. There is no way to enable bypass at runtime in production.
- **Batched check is a primary, not a convenience.** `CheckActions` makes one gRPC round-trip for N actions on the same resource. The frontend's `@plinth-dev/authz-react` `<PermissionsProvider>` consumes `PermissionMap`'s output directly.
- **Pinger interface compatibility.** `Client.Ping(ctx) error` satisfies `sdk-go/health.Pinger` for use with `health.CerbosCheck`.

## Boundaries

- **Does not load Cerbos policies.** Policies are deployed separately (Argo CD → Cerbos pod).
- **Does not cache decisions.** Cerbos is fast (~1ms p99 in-cluster) and stateless; caching breaks policy hot-reload.
- **Does not validate JWTs.** Pass the raw token via `Principal.AuxData.JWT`; Cerbos's `$jwtClaims` accessor reads it directly.

## Compatibility

- **Go 1.25+** (matches sdk-go/otel; the cerbos-sdk-go transitive deps require it).
- Wraps `github.com/cerbos/cerbos-sdk-go` (v0.3.18+).
- The `cerbosBackend` interface is intentionally unexported. Tests in the same package use a fake; external callers can't bypass `New`'s safety checks.

## License

MIT — see [LICENSE](./LICENSE).
