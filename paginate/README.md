# `github.com/plinth-dev/sdk-go/paginate`

Plinth's pagination toolkit. Two modes (cursor preferred, offset for small datasets), a generic `Page[T]` response wrapper, and a query-string parser that prevents SQL injection via sort-column allow-listing.

Design rationale: <https://plinth.run/sdk/go/paginate/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/paginate@latest
```

## Minimum example

```go
import (
    "net/http"
    "github.com/plinth-dev/sdk-go/paginate"
)

var allowedSortColumns = []string{"created_at", "name", "status"}

func ListItems(w http.ResponseWriter, r *http.Request) {
    p, err := paginate.FromQuery(r.URL.Query(), allowedSortColumns)
    if err != nil {
        // wrap as apperrors.Validation at the handler boundary
        http.Error(w, err.Error(), http.StatusUnprocessableEntity)
        return
    }

    // p.OffsetSQL() → " LIMIT 20 OFFSET 40" (offset mode)
    // p.CursorBefore() → ("created_at", "2026-04-30T...", nil) (cursor mode)
    items, totalCount, _ := repo.List(r.Context(), p)

    page := paginate.NewOffsetPage(items, p, totalCount)
    json.NewEncoder(w).Encode(page)
}
```

## Behaviour

- **Two modes, decided by query.** `?cursor=...` → cursor mode. `?page=N` → offset mode. Both → 400 (mixed-mode error).
- **Sort allow-list is mandatory.** Pass an exhaustive list of allowed column names to `FromQuery`; anything not in the list returns `ErrUnknownSortColumn`. This is the single line standing between user input and `ORDER BY`.
- **Defaults baked in.** `PageSize` defaults to 20, clamped to `[1, 100]`. `Page` defaults to 1. `SortOrder` defaults to `desc`.
- **Cursor format is opaque.** Base64 of `column:value`. Don't depend on the format — round-trip the string via `PageMeta.NextCursor`.

## Compatibility

- **Go 1.23+** (uses `slices`).
- **No external dependencies.** Pure standard library.
- Wire-format JSON keys (`items`, `meta`, `mode`, `page`, `page_size`, `total_count`, `total_pages`, `next_cursor`, `has_next`) are part of the public contract — `@plinth-dev/tables` consumes them.

## License

MIT — see [LICENSE](./LICENSE).
