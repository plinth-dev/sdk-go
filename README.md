# Plinth — Go SDK

> **Status: not yet released — Phase B in progress.**
> The repo and the seven package directories are reserved; **no `go get` import path resolves yet**. API surfaces are being designed and reviewed; the first tag will be `v0.1.0` per package. Track design ADRs at [plinth.run/sdk](https://plinth.run/sdk/) and progress on the [roadmap](https://github.com/plinth-dev/.github/blob/main/ROADMAP.md).

A multi-module Go monorepo. One independent module per package, semver-tagged from `v0.1.0` once each design is locked, installable via `go get`.

## Planned packages

| Package | Responsibility | Import path |
| --- | --- | --- |
| `audit` | Emit CloudEvents-shaped audit events to a pluggable transport | `github.com/plinth-dev/sdk-go/audit` |
| `authz` | Cerbos PDP client wrapper with explicit `Decision` and fail-closed semantics | `github.com/plinth-dev/sdk-go/authz` |
| `errors` | Typed error vocabulary; sentinel errors via `errors.Is`; RFC 7807 mapping | `github.com/plinth-dev/sdk-go/errors` |
| `health` | Dependency probe registry with parallel execution | `github.com/plinth-dev/sdk-go/health` |
| `otel` | OpenTelemetry SDK initialisation with standard resource attributes | `github.com/plinth-dev/sdk-go/otel` |
| `paginate` | Cursor + offset pagination types and parsers | `github.com/plinth-dev/sdk-go/paginate` |
| `vault` | Secret reader: `/run/secrets/<name>` first, env var fallback, in-memory cache | `github.com/plinth-dev/sdk-go/vault` |

Once shipped, each package will have its own `go.mod`, README, semver tag, and minimal dependency surface.

## Install (once shipped)

```bash
go get github.com/plinth-dev/sdk-go/authz@latest
```

## Design intent

The API surface for each package is being documented at [plinth.run/sdk](https://plinth.run/sdk/) ahead of implementation. This repo will hold the implementations.

## Planned layout

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
