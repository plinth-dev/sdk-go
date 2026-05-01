// Package paginate is Plinth's pagination toolkit.
//
// Two modes (cursor preferred, offset for small datasets), a generic
// response wrapper [Page], and a query-string parser that prevents SQL
// injection via sort-column allow-listing.
//
// See https://plinth.run/sdk/go/paginate/ for the design rationale.
package paginate

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Mode selects the pagination strategy. Offset is friendlier for fixed-size
// admin lists with stable totals; cursor is the only sane choice for large
// mutating lists.
type Mode string

const (
	ModeOffset Mode = "offset"
	ModeCursor Mode = "cursor"
)

// SortOrder is the sort direction. "desc" by default — most internal-tooling
// lists are reverse-chronological.
type SortOrder string

const (
	SortDesc SortOrder = "desc"
	SortAsc  SortOrder = "asc"
)

// Defaults applied by [Pagination.Validated] and [FromQuery].
const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Sentinel errors. Match via errors.Is. Translate to apperrors.Validation at
// the handler boundary if you want a 422.
var (
	ErrUnknownSortColumn = errors.New("unknown sort column")
	ErrMixedMode         = errors.New("cannot specify both page and cursor")
	ErrInvalidPagination = errors.New("invalid pagination parameter")
	ErrMalformedCursor   = errors.New("malformed cursor")
)

// Pagination is the request shape, parsed from the query string by [FromQuery].
//
// Mode is implied: cursor-mode if Cursor is set, offset-mode otherwise.
// SortBy must come from an allow-list; FromQuery enforces that.
type Pagination struct {
	Mode      Mode
	Page      int       // 1-based; offset mode only
	PageSize  int       // [1, MaxPageSize]; defaults to DefaultPageSize
	Cursor    string    // opaque base64-encoded cursor; cursor mode only
	SortBy    string    // column name; must come from FromQuery's allow-list
	SortOrder SortOrder // SortDesc by default
}

// Validated returns p with defaults applied and bounds enforced. Used internally
// by [FromQuery]; exposed for tests and direct use.
func (p Pagination) Validated() Pagination {
	if p.Page < 1 {
		p.Page = DefaultPage
	}
	if p.PageSize <= 0 {
		p.PageSize = DefaultPageSize
	}
	if p.PageSize > MaxPageSize {
		p.PageSize = MaxPageSize
	}
	if p.SortOrder != SortAsc && p.SortOrder != SortDesc {
		p.SortOrder = SortDesc
	}
	if p.Mode == "" {
		if p.Cursor != "" {
			p.Mode = ModeCursor
		} else {
			p.Mode = ModeOffset
		}
	}
	return p
}

// OffsetSQL returns a SQL fragment for offset/limit, e.g. " LIMIT 20 OFFSET 40".
// Returns an empty string when Mode is not [ModeOffset]. Caller is responsible
// for wiring this into a parameterized query — the values come from validated
// fields, not user input.
func (p Pagination) OffsetSQL() string {
	if p.Mode != ModeOffset {
		return ""
	}
	v := p.Validated()
	offset := (v.Page - 1) * v.PageSize
	return fmt.Sprintf(" LIMIT %d OFFSET %d", v.PageSize, offset)
}

// CursorBefore decodes [Pagination.Cursor] into a (column, value) pair the
// repository layer can use to filter. Returns zero values + nil error if
// Cursor is empty. Returns an [ErrMalformedCursor]-wrapped error if the
// cursor doesn't decode.
//
// The cursor format is opaque base64 of "<column>:<value>". Don't depend on
// the format directly; pass cursors through unchanged from one request to
// the next via [PageMeta.NextCursor].
func (p Pagination) CursorBefore() (column, value string, err error) {
	if p.Cursor == "" {
		return "", "", nil
	}
	raw, err := base64.URLEncoding.DecodeString(p.Cursor)
	if err != nil {
		return "", "", fmt.Errorf("%w: base64 decode: %v", ErrMalformedCursor, err)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%w: missing separator", ErrMalformedCursor)
	}
	if parts[0] == "" {
		return "", "", fmt.Errorf("%w: empty column", ErrMalformedCursor)
	}
	return parts[0], parts[1], nil
}

// EncodeCursor builds a base64-URL-encoded cursor from a column name and value.
// The repository layer typically calls this on the last item of a result page.
//
// The format is intentionally opaque to consumers — they only round-trip the
// string. We can change the encoding in a future major version.
func EncodeCursor(column string, value any) string {
	raw := fmt.Sprintf("%s:%v", column, value)
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// Page is the canonical paginated-response wrapper. T is the item type.
type Page[T any] struct {
	Items []T      `json:"items"`
	Meta  PageMeta `json:"meta"`
}

// PageMeta is the metadata block of a [Page]. JSON field names match the
// [@plinth-dev/tables] frontend table component.
type PageMeta struct {
	Mode       Mode   `json:"mode"`
	Page       int    `json:"page,omitempty"`         // offset mode only
	PageSize   int    `json:"page_size"`
	TotalCount int64  `json:"total_count,omitempty"`  // offset mode only
	TotalPages int    `json:"total_pages,omitempty"`  // offset mode only
	NextCursor string `json:"next_cursor,omitempty"`  // cursor mode only
	HasNext    bool   `json:"has_next"`
}

// NewOffsetPage builds a [Page] from items + total count. Caller is responsible
// for running a separate COUNT query — cursor mode avoids this cost.
func NewOffsetPage[T any](items []T, p Pagination, totalCount int64) Page[T] {
	v := p.Validated()
	totalPages := 0
	if totalCount > 0 && v.PageSize > 0 {
		totalPages = int((totalCount + int64(v.PageSize) - 1) / int64(v.PageSize))
	}
	return Page[T]{
		Items: items,
		Meta: PageMeta{
			Mode:       ModeOffset,
			Page:       v.Page,
			PageSize:   v.PageSize,
			TotalCount: totalCount,
			TotalPages: totalPages,
			HasNext:    v.Page < totalPages,
		},
	}
}

// NewCursorPage builds a [Page] from items + a function that produces the
// next-page cursor from the last item. The cursor is set only if the result
// page is full (len(items) >= PageSize) — partial pages signal "end of list".
func NewCursorPage[T any](items []T, p Pagination, lastItemCursor func(T) string) Page[T] {
	v := p.Validated()
	var nextCursor string
	if len(items) > 0 && len(items) >= v.PageSize {
		nextCursor = lastItemCursor(items[len(items)-1])
	}
	return Page[T]{
		Items: items,
		Meta: PageMeta{
			Mode:       ModeCursor,
			PageSize:   v.PageSize,
			NextCursor: nextCursor,
			HasNext:    nextCursor != "",
		},
	}
}

// FromQuery parses [Pagination] from a URL query string. Returns a wrapped
// sentinel error if the query is invalid.
//
// allowedSortColumns is mandatory: the only thing standing between user input
// and a SQL ORDER BY clause. Pass an exhaustive list — anything else is
// rejected with [ErrUnknownSortColumn].
//
// Recognised query parameters:
//   - page         (offset mode; ≥1)
//   - page_size    ([1, MaxPageSize]; clamped)
//   - cursor       (cursor mode; opaque base64)
//   - sort_by      (must be in allowedSortColumns)
//   - sort_order   ("asc" | "desc"; case-insensitive)
//
// Mixed-mode (page + cursor) → [ErrMixedMode].
func FromQuery(q url.Values, allowedSortColumns []string) (Pagination, error) {
	var p Pagination

	if cur := q.Get("cursor"); cur != "" {
		p.Cursor = cur
		p.Mode = ModeCursor
	}
	if pg := q.Get("page"); pg != "" {
		if p.Mode == ModeCursor {
			return p, fmt.Errorf("%w: page=%q and cursor present", ErrMixedMode, pg)
		}
		n, err := strconv.Atoi(pg)
		if err != nil || n < 1 {
			return p, fmt.Errorf("%w: page=%q", ErrInvalidPagination, pg)
		}
		p.Page = n
		p.Mode = ModeOffset
	}
	if ps := q.Get("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 {
			return p, fmt.Errorf("%w: page_size=%q", ErrInvalidPagination, ps)
		}
		p.PageSize = n
	}
	if sb := q.Get("sort_by"); sb != "" {
		if !slices.Contains(allowedSortColumns, sb) {
			return p, fmt.Errorf("%w: %q (allowed: %v)", ErrUnknownSortColumn, sb, allowedSortColumns)
		}
		p.SortBy = sb
	}
	if so := q.Get("sort_order"); so != "" {
		switch strings.ToLower(so) {
		case "asc":
			p.SortOrder = SortAsc
		case "desc":
			p.SortOrder = SortDesc
		default:
			return p, fmt.Errorf("%w: sort_order=%q", ErrInvalidPagination, so)
		}
	}
	return p.Validated(), nil
}
