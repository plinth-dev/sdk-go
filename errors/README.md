# `github.com/plinth-dev/sdk-go/errors`

Plinth's typed error vocabulary. Six sentinel categories, factory helpers, and an HTTP middleware that maps any of them into RFC 7807 `application/problem+json`.

Design rationale: <https://plinth.run/sdk/go/errors/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/errors@latest
```

The package is named `apperrors` (not `errors`) to avoid colliding with the standard library. Standard import alias:

```go
import apperrors "github.com/plinth-dev/sdk-go/errors"
```

## Minimum example

```go
package items

import (
    "context"
    apperrors "github.com/plinth-dev/sdk-go/errors"
)

func (s *Service) Get(ctx context.Context, id string) (*Item, error) {
    item, err := s.repo.Get(ctx, id)
    if err != nil {
        if errors.Is(err, repository.ErrNotFound) {
            return nil, apperrors.NotFound("item", id)  // 404 + not_found code
        }
        return nil, apperrors.Wrap(err, apperrors.CodeInternal, "items.get failed")
    }
    return item, nil
}
```

In the HTTP layer, mount the middleware once and call `apperrors.SetError` from handlers:

```go
mux := chi.NewRouter()
mux.Use(func(next http.Handler) http.Handler {
    return apperrors.HTTPMiddleware(next)
})

mux.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {
    item, err := svc.Get(r.Context(), chi.URLParam(r, "id"))
    if err != nil {
        apperrors.SetError(r, err)
        return
    }
    json.NewEncoder(w).Encode(item)
})
```

A `GET /items/missing` request gets back:

```http
HTTP/1.1 404 Not Found
Content-Type: application/problem+json

{
  "type":   "https://plinth.run/errors/not_found",
  "title":  "Not found",
  "status": 404,
  "detail": "item missing not found",
  "code":   "not_found"
}
```

## API at a glance

| Symbol | Purpose |
|---|---|
| `Err{NotFound,Conflict,PermissionDenied,Validation,Unauthenticated,Internal}` | Sentinels — match via `errors.Is`. |
| `Code{NotFound,…}` | Wire-stable string label. Maps 1:1 to a sentinel and HTTP status. |
| `*AppError` | Concrete struct with `Code`, `Message`, `Fields`, and `HTTPStatus()`. |
| `NotFound(resource, id) / Conflict(fmt, ...) / PermissionDenied(action) / Validation(msg, fields) / Unauthenticated(msg) / Internal(fmt, ...)` | Factory helpers. |
| `Wrap(cause, code, fmt, ...)` | Wrap a lower-level error; `errors.Is` works for both Code-sentinel and the wrapped cause. |
| `HTTPMiddleware(next, opts...)` | net/http middleware that reads `SetError` from the request context and emits problem+json. |
| `SetError(r, err) / GetError(r) / WriteProblem(w, r, err, opts...)` | The plumbing for the middleware path, plus a standalone escape hatch. |
| `WithLogger(l) / WithTypeURIPrefix(s) / WithTraceIDFunc(fn)` | Middleware options. |

## Behaviour notes

- **Internal errors are sanitized on the wire.** `Code == CodeInternal` has its `Message` replaced with `"an internal error occurred"` before serialization. The full original is logged at `slog.Error` with the trace ID for ops correlation.
- **Trace correlation is opt-in.** Pass `WithTraceIDFunc` to plug in your tracing system (typically OpenTelemetry); without it, `Problem.TraceID` is empty.
- **The middleware does not overwrite a handler that already wrote a response.** If the handler called `w.Write` *and* `SetError`, the response wins. Buggy handlers don't get clobbered.

## Compatibility

- **Go 1.23+** (uses `log/slog`).
- **No external dependencies.** Pure standard library.
- **Semver from v0.1.0.** Code constant string values are part of the public contract; changing one is a major-version bump.

## License

MIT — see [LICENSE](./LICENSE).
