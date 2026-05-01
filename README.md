# Plinth — Go SDK

A multi-module Go monorepo. One independent module per package, semver-tagged from `v0.1.0` per module, installable via `go get`.

Design rationale per package: <https://plinth.run/sdk/>.

## Packages

| Package | Status | Responsibility |
| --- | --- | --- |
| [`errors`](./errors) | **shipped** · pre-release | Typed error vocabulary; sentinels via `errors.Is`; RFC 7807 problem+json middleware. |
| [`audit`](./audit) | **shipped** · pre-release | Non-blocking publisher with CloudEvents-shaped events and a pluggable transport. |
| [`paginate`](./paginate) | **shipped** · pre-release | Cursor + offset pagination types and parsers; allow-list-based sort safety. |
| [`vault`](./vault) | **shipped** · pre-release | Secret reader: `/run/secrets/<name>` first, env-var fallback, in-memory cache. |
| `authz` | not yet shipped | Cerbos PDP client wrapper with explicit `Decision` and fail-closed semantics. |
| `health` | not yet shipped | Dependency probe registry with parallel execution. |
| `otel` | not yet shipped | OpenTelemetry SDK initialisation with standard resource attributes. |

Each shipped package has its own `go.mod`, semver tag, README, and minimal dependency surface.

## Install

```bash
# Per package — semver-tagged independently.
go get github.com/plinth-dev/sdk-go/errors@latest
```

## Local development

Top-level [`go.work`](./go.work) joins all modules into one Go workspace, so cross-module refactors don't need `replace` directives.

```bash
go work sync                       # ensure workspace is current
go test -race -cover ./...         # run from any module directory
go vet ./...                       # check from any module directory
```

CI runs `go vet` + `go test -race -cover` per module on every push.

## Layout

```
.
├── go.work                        # Go workspace joining every module
├── errors/                        # github.com/plinth-dev/sdk-go/errors
│   ├── go.mod
│   ├── doc.go errors.go http.go
│   ├── errors_test.go http_test.go
│   ├── README.md  LICENSE
│   └── ...
├── audit/                         # (not yet shipped)
├── authz/                         # (not yet shipped)
└── ...
```

## Versioning

Each package is tagged independently as `<package>/vX.Y.Z`. Breaking changes within `0.x` are batched into minor versions; `v1.0` freezes APIs for a year.

## Related

- [`sdk-ts`](https://github.com/plinth-dev/sdk-ts) — the TypeScript SDK.
- [`starter-api`](https://github.com/plinth-dev/starter-api) — Go module starter that imports these packages.
- [`plinth.run`](https://plinth.run) — per-package design docs and tutorials.

## License

MIT — see [LICENSE](./LICENSE).
