package paginate

import (
	"encoding/base64"
	"errors"
	"net/url"
	"testing"
)

// ── Pagination.Validated ─────────────────────────────────────────────

func TestValidated_AppliesDefaults(t *testing.T) {
	got := Pagination{}.Validated()
	if got.Page != DefaultPage {
		t.Errorf("Page = %d; want %d", got.Page, DefaultPage)
	}
	if got.PageSize != DefaultPageSize {
		t.Errorf("PageSize = %d; want %d", got.PageSize, DefaultPageSize)
	}
	if got.SortOrder != SortDesc {
		t.Errorf("SortOrder = %q; want SortDesc", got.SortOrder)
	}
	if got.Mode != ModeOffset {
		t.Errorf("Mode = %q; want ModeOffset (no cursor)", got.Mode)
	}
}

func TestValidated_ClampsPageSize(t *testing.T) {
	got := Pagination{PageSize: 5000}.Validated()
	if got.PageSize != MaxPageSize {
		t.Errorf("PageSize = %d; want %d (clamped)", got.PageSize, MaxPageSize)
	}
}

func TestValidated_PreservesValidValues(t *testing.T) {
	got := Pagination{Page: 5, PageSize: 50, SortBy: "created_at", SortOrder: SortAsc}.Validated()
	if got.Page != 5 || got.PageSize != 50 || got.SortOrder != SortAsc {
		t.Errorf("validated changed valid values: %+v", got)
	}
}

func TestValidated_CursorImpliesCursorMode(t *testing.T) {
	got := Pagination{Cursor: "abc"}.Validated()
	if got.Mode != ModeCursor {
		t.Errorf("Mode = %q; want ModeCursor", got.Mode)
	}
}

// ── OffsetSQL ────────────────────────────────────────────────────────

func TestOffsetSQL_StandardCase(t *testing.T) {
	p := Pagination{Mode: ModeOffset, Page: 3, PageSize: 20}.Validated()
	got := p.OffsetSQL()
	if got != " LIMIT 20 OFFSET 40" {
		t.Errorf("OffsetSQL = %q; want ' LIMIT 20 OFFSET 40'", got)
	}
}

func TestOffsetSQL_FirstPage(t *testing.T) {
	p := Pagination{Mode: ModeOffset, Page: 1, PageSize: 10}.Validated()
	got := p.OffsetSQL()
	if got != " LIMIT 10 OFFSET 0" {
		t.Errorf("OffsetSQL = %q; want ' LIMIT 10 OFFSET 0'", got)
	}
}

func TestOffsetSQL_CursorModeReturnsEmpty(t *testing.T) {
	p := Pagination{Mode: ModeCursor, Cursor: "x"}
	if got := p.OffsetSQL(); got != "" {
		t.Errorf("cursor-mode OffsetSQL should be empty; got %q", got)
	}
}

// ── Cursor encode/decode ─────────────────────────────────────────────

var allowedTestCols = []string{"created_at", "name", "id"}

func TestEncodeCursor_RoundTrip(t *testing.T) {
	cursor := EncodeCursor("created_at", "2026-04-30T12:34:56Z")
	p := Pagination{Cursor: cursor}
	col, val, err := p.CursorBefore(allowedTestCols)
	if err != nil {
		t.Fatalf("CursorBefore: %v", err)
	}
	if col != "created_at" {
		t.Errorf("column = %q; want 'created_at'", col)
	}
	if val != "2026-04-30T12:34:56Z" {
		t.Errorf("value = %q; want timestamp", val)
	}
}

func TestCursorBefore_EmptyCursor(t *testing.T) {
	col, val, err := Pagination{}.CursorBefore(allowedTestCols)
	if err != nil || col != "" || val != "" {
		t.Errorf("empty cursor: col=%q val=%q err=%v; want all zero", col, val, err)
	}
}

func TestCursorBefore_MalformedBase64(t *testing.T) {
	_, _, err := Pagination{Cursor: "!!not-base64!!"}.CursorBefore(allowedTestCols)
	if !errors.Is(err, ErrMalformedCursor) {
		t.Errorf("err = %v; want ErrMalformedCursor", err)
	}
}

func TestCursorBefore_MissingSeparator(t *testing.T) {
	// Base64-encode a payload without ":" — separator missing, even
	// before the allow-list check.
	bad := base64.URLEncoding.EncodeToString([]byte("noseparator"))
	_, _, err := Pagination{Cursor: bad}.CursorBefore(allowedTestCols)
	if !errors.Is(err, ErrMalformedCursor) {
		t.Errorf("err = %v; want ErrMalformedCursor", err)
	}
}

// TestCursorBefore_InjectionRejected proves the allow-list guards against
// a hand-crafted cursor referencing a column the repo never intended to
// expose. Without this check, a caller could craft a cursor for
// "secret_column" and bypass the FromQuery sort_by allow-list.
func TestCursorBefore_InjectionRejected(t *testing.T) {
	malicious := EncodeCursor("password_hash", "x")
	_, _, err := Pagination{Cursor: malicious}.CursorBefore(allowedTestCols)
	if !errors.Is(err, ErrUnknownSortColumn) {
		t.Errorf("err = %v; want ErrUnknownSortColumn for non-allow-listed column", err)
	}
}

// ── FromQuery ────────────────────────────────────────────────────────

func TestFromQuery_DefaultsWhenEmpty(t *testing.T) {
	p, err := FromQuery(url.Values{}, []string{"created_at"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.Page != DefaultPage || p.PageSize != DefaultPageSize {
		t.Errorf("defaults not applied: %+v", p)
	}
}

func TestFromQuery_PageParameter(t *testing.T) {
	q := url.Values{"page": {"5"}, "page_size": {"50"}}
	p, err := FromQuery(q, []string{"created_at"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.Page != 5 || p.PageSize != 50 {
		t.Errorf("page=%d page_size=%d; want 5/50", p.Page, p.PageSize)
	}
	if p.Mode != ModeOffset {
		t.Errorf("Mode = %q; want ModeOffset", p.Mode)
	}
}

func TestFromQuery_CursorParameter(t *testing.T) {
	cursor := EncodeCursor("created_at", "2026-04-30")
	q := url.Values{"cursor": {cursor}}
	p, err := FromQuery(q, []string{"created_at"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.Mode != ModeCursor {
		t.Errorf("Mode = %q; want ModeCursor", p.Mode)
	}
	if p.Cursor != cursor {
		t.Errorf("cursor mismatch")
	}
}

func TestFromQuery_RejectsMixedMode(t *testing.T) {
	q := url.Values{"page": {"2"}, "cursor": {"someblob"}}
	_, err := FromQuery(q, nil)
	if !errors.Is(err, ErrMixedMode) {
		t.Errorf("err = %v; want ErrMixedMode", err)
	}
}

func TestFromQuery_RejectsBadPage(t *testing.T) {
	for _, bad := range []string{"-1", "abc", "0"} {
		q := url.Values{"page": {bad}}
		_, err := FromQuery(q, nil)
		if !errors.Is(err, ErrInvalidPagination) {
			t.Errorf("page=%q: err = %v; want ErrInvalidPagination", bad, err)
		}
	}
}

func TestFromQuery_AllowListEnforcement(t *testing.T) {
	q := url.Values{"sort_by": {"id; DROP TABLE users;--"}}
	_, err := FromQuery(q, []string{"created_at", "name"})
	if !errors.Is(err, ErrUnknownSortColumn) {
		t.Errorf("err = %v; want ErrUnknownSortColumn (the SQL-injection vector must be blocked)", err)
	}
}

func TestFromQuery_AllowedSortPasses(t *testing.T) {
	q := url.Values{"sort_by": {"created_at"}, "sort_order": {"asc"}}
	p, err := FromQuery(q, []string{"created_at", "name"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.SortBy != "created_at" || p.SortOrder != SortAsc {
		t.Errorf("sort: %+v", p)
	}
}

func TestFromQuery_SortOrderCaseInsensitive(t *testing.T) {
	q := url.Values{"sort_order": {"DESC"}}
	p, err := FromQuery(q, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.SortOrder != SortDesc {
		t.Errorf("sort_order = %q; want SortDesc", p.SortOrder)
	}
}

func TestFromQuery_RejectsBadSortOrder(t *testing.T) {
	q := url.Values{"sort_order": {"random"}}
	_, err := FromQuery(q, nil)
	if !errors.Is(err, ErrInvalidPagination) {
		t.Errorf("err = %v; want ErrInvalidPagination", err)
	}
}

// ── Page builders ────────────────────────────────────────────────────

type fakeItem struct {
	ID        string
	CreatedAt string
}

func TestNewOffsetPage_PageMath(t *testing.T) {
	items := []fakeItem{{ID: "a"}, {ID: "b"}}
	p := Pagination{Page: 2, PageSize: 10}
	page := NewOffsetPage(items, p, 25)

	if page.Meta.TotalPages != 3 {
		t.Errorf("TotalPages = %d; want 3 (25/10 ceil)", page.Meta.TotalPages)
	}
	if !page.Meta.HasNext {
		t.Errorf("HasNext should be true on page 2 of 3")
	}
}

func TestNewOffsetPage_LastPage(t *testing.T) {
	items := []fakeItem{{ID: "a"}}
	p := Pagination{Page: 3, PageSize: 10}
	page := NewOffsetPage(items, p, 21)

	if page.Meta.TotalPages != 3 {
		t.Errorf("TotalPages = %d; want 3", page.Meta.TotalPages)
	}
	if page.Meta.HasNext {
		t.Errorf("HasNext on last page should be false")
	}
}

func TestNewOffsetPage_EmptyResult(t *testing.T) {
	p := Pagination{Page: 1, PageSize: 10}
	page := NewOffsetPage([]fakeItem{}, p, 0)

	if page.Meta.TotalPages != 0 {
		t.Errorf("TotalPages on empty = %d; want 0", page.Meta.TotalPages)
	}
	if page.Meta.HasNext {
		t.Errorf("HasNext on empty should be false")
	}
}

func TestNewCursorPage_FullPageHasNextCursor(t *testing.T) {
	items := []fakeItem{
		{ID: "a", CreatedAt: "2026-01-01"},
		{ID: "b", CreatedAt: "2026-01-02"},
	}
	p := Pagination{PageSize: 2}
	page := NewCursorPage(items, p, func(it fakeItem) string {
		return EncodeCursor("created_at", it.CreatedAt)
	})

	if !page.Meta.HasNext {
		t.Errorf("full page should have HasNext = true")
	}
	if page.Meta.NextCursor == "" {
		t.Errorf("NextCursor should be populated on full page")
	}
}

func TestNewCursorPage_PartialPageHasNoCursor(t *testing.T) {
	items := []fakeItem{{ID: "a"}}
	p := Pagination{PageSize: 10}
	page := NewCursorPage(items, p, func(it fakeItem) string { return "x" })

	if page.Meta.HasNext {
		t.Errorf("partial page should signal end of list")
	}
	if page.Meta.NextCursor != "" {
		t.Errorf("NextCursor should be empty on partial page")
	}
}
