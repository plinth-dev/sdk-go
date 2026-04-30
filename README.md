# Plinth — Go SDK

A multi-module Go monorepo. One independent module per package, semver-tagged from `v0.1.0`, installable via `go get`.

> **Status: v0.1.0 — Phase B in progress.** API surfaces are being designed; expect breaking changes until each package is tagged at `v0.1.0`.

## Packages

| Package | Responsibility | Import path |
| --- | --- | --- |
| `audit` | Emit CloudEvents-shaped audit events to a pluggable transport | `github.com/plinth-dev/sdk-go/audit` |
| `authz` | Cerbos PDP client wrapper with explicit `Decision` and fail-closed semantics | `github.com/plinth-dev/sdk-go/authz` |
| `errors` | Typed error vocabulary; sentinel errors via `errors.Is`; RFC 7807 mapping | `github.com/plinth-dev/sdk-go/errors` |
| `health` | Dependency probe registry with parallel execution | `github.com/plinth-dev/sdk-go/health` |
| `otel` | OpenTelemetry SDK initialisation with standard resource attributes | `github.com/plinth-dev/sdk-go/otel` |
| `paginate` | Cursor + offset pagination types and parsers | `github.com/plinth-dev/sdk-go/paginate` |
| `vault` | Secret reader: `/run/secrets/<name>` first, env var fallback, in-memory cache | `github.com/plinth-dev/sdk-go/vault` |

Each package has its own `go.mod`, README, semver tag, and minimal dependency surface.

## Install

```bash
go get github.com/plinth-dev/sdk-go/authz@latest
```

## Design intent

The API surface for each package is documented in detail at [plinth.run/sdk](https://plinth.run/sdk/). This repo holds the implementations.

## Layout

```
.
├── audit/         # go.mod, audit.go, audit_test.go, README.md
├── authz/
├── errors/
├── health/
├── otel/
├── paginate/
└── vault/
```

## Versioning

Each package is tagged independently as `<package>/vX.Y.Z`. Breaking changes within `0.x` are batched into minor versions; v1.0 freezes APIs for a year.

## Related

- [`sdk-ts`](https://github.com/plinth-dev/sdk-ts) — the TypeScript SDK.
- [`starter-api`](https://github.com/plinth-dev/starter-api) — Go module starter that imports these packages.
- [`plinth.run`](https://plinth.run) — design ADRs and tutorials.

## License

MIT — see [LICENSE](./LICENSE).
