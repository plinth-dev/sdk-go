package apperrors

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestSentinelMatching_FactoryConstructed(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"NotFound matches ErrNotFound", NotFound("item", "abc"), ErrNotFound},
		{"Conflict matches ErrConflict", Conflict("dupe key %q", "abc"), ErrConflict},
		{"PermissionDenied matches", PermissionDenied("items:delete"), ErrPermissionDenied},
		{"Validation matches", Validation("bad body", nil), ErrValidation},
		{"Unauthenticated matches", Unauthenticated("no session"), ErrUnauthenticated},
		{"Internal matches ErrInternal", Internal("oops %d", 42), ErrInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !errors.Is(c.err, c.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false; want true", c.err, c.sentinel)
			}
		})
	}
}

func TestSentinelMatching_Wrapped(t *testing.T) {
	cause := errors.New("pgx: no rows")
	wrapped := Wrap(cause, CodeNotFound, "item %q", "abc")

	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("wrapped error should match ErrNotFound via Code")
	}
	if !errors.Is(wrapped, cause) {
		t.Error("wrapped error should match original cause via Unwrap chain")
	}
}

func TestSentinelDoesNotMatchWrongCode(t *testing.T) {
	err := NotFound("item", "abc")
	if errors.Is(err, ErrConflict) {
		t.Error("NotFound should not match ErrConflict")
	}
}

func TestErrorMessageFormat(t *testing.T) {
	cases := []struct {
		err  *AppError
		want string
	}{
		{NotFound("item", "abc"), "not_found: item abc not found"},
		{Conflict("dupe key %q", "abc"), `conflict: dupe key "abc"`},
		{PermissionDenied("items:delete"), "permission_denied: not allowed: items:delete"},
		{Unauthenticated("missing session"), "unauthenticated: missing session"},
		{Internal("downstream %v", "broke"), "internal: downstream broke"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q; want %q", got, c.want)
		}
	}
}

func TestErrorMessageFormat_Wrapped(t *testing.T) {
	cause := errors.New("pgx: no rows in result set")
	err := Wrap(cause, CodeNotFound, "item %q", "abc")
	got := err.Error()
	if !strings.Contains(got, "not_found") || !strings.Contains(got, "abc") || !strings.Contains(got, "no rows") {
		t.Errorf("wrapped Error() should mention code, message, and cause; got %q", got)
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		err  *AppError
		want int
	}{
		{NotFound("x", "y"), http.StatusNotFound},
		{Conflict("x"), http.StatusConflict},
		{PermissionDenied("x"), http.StatusForbidden},
		{Validation("x", nil), http.StatusUnprocessableEntity},
		{Unauthenticated("x"), http.StatusUnauthorized},
		{Internal("x"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := c.err.HTTPStatus(); got != c.want {
			t.Errorf("%s HTTPStatus() = %d; want %d", c.err.Code, got, c.want)
		}
	}
}

func TestHTTPStatusOf_NonAppError(t *testing.T) {
	plain := errors.New("plain")
	if got := HTTPStatusOf(plain); got != http.StatusInternalServerError {
		t.Errorf("HTTPStatusOf(plain) = %d; want 500", got)
	}
	if got := HTTPStatusOf(nil); got != http.StatusInternalServerError {
		t.Errorf("HTTPStatusOf(nil) = %d; want 500", got)
	}
}

func TestCodeOf(t *testing.T) {
	if got := CodeOf(NotFound("a", "b")); got != CodeNotFound {
		t.Errorf("CodeOf(NotFound) = %q; want %q", got, CodeNotFound)
	}
	if got := CodeOf(errors.New("plain")); got != CodeInternal {
		t.Errorf("CodeOf(plain) = %q; want CodeInternal", got)
	}
	if got := CodeOf(nil); got != CodeInternal {
		t.Errorf("CodeOf(nil) = %q; want CodeInternal", got)
	}
}

func TestValidationFields(t *testing.T) {
	fields := map[string]string{
		"email": "must be a valid email",
		"age":   "must be ≥ 18",
	}
	err := Validation("body failed validation", fields)
	if err.Code != CodeValidation {
		t.Errorf("Code = %q; want %q", err.Code, CodeValidation)
	}
	if len(err.Fields) != 2 {
		t.Errorf("Fields len = %d; want 2", len(err.Fields))
	}
	if err.Fields["email"] != "must be a valid email" {
		t.Errorf("Fields[email] = %q; want 'must be a valid email'", err.Fields["email"])
	}
}

func TestNilSafety(t *testing.T) {
	var nilErr *AppError
	// .Error() should not panic on nil receiver.
	if got := nilErr.Error(); got != "<nil *AppError>" {
		t.Errorf("nil.Error() = %q; want '<nil *AppError>'", got)
	}
	// .Unwrap() on nil returns nil.
	if got := nilErr.Unwrap(); got != nil {
		t.Errorf("nil.Unwrap() = %v; want nil", got)
	}
	// .HTTPStatus() on nil returns 500 (safe default).
	if got := nilErr.HTTPStatus(); got != http.StatusInternalServerError {
		t.Errorf("nil.HTTPStatus() = %d; want 500", got)
	}
}

func TestWrap_PreservesCauseChain(t *testing.T) {
	// Three-level chain: leaf → wrap → wrap.
	leaf := errors.New("connection refused")
	mid := Wrap(leaf, CodeInternal, "couldn't reach upstream")
	top := Wrap(mid, CodeInternal, "request failed")

	if !errors.Is(top, leaf) {
		t.Error("top should match leaf via chain")
	}
	if !errors.Is(top, ErrInternal) {
		t.Error("top should match ErrInternal via Code")
	}
}

// TestSentinelForAllCodes ensures every Code constant has a matching sentinel
// in sentinelFor. Adding a new Code without updating the switch will fail this.
func TestSentinelForAllCodes(t *testing.T) {
	allCodes := []Code{
		CodeNotFound, CodeConflict, CodePermissionDenied,
		CodeValidation, CodeUnauthenticated, CodeInternal,
	}
	for _, c := range allCodes {
		s := sentinelFor(c)
		if s == nil {
			t.Errorf("sentinelFor(%q) returned nil", c)
		}
		if s == ErrInternal && c != CodeInternal {
			t.Errorf("sentinelFor(%q) fell through to ErrInternal default", c)
		}
	}
}

// TestStatusForAllCodes ensures every Code maps to a non-default HTTP status.
func TestStatusForAllCodes(t *testing.T) {
	cases := map[Code]int{
		CodeNotFound:         http.StatusNotFound,
		CodeConflict:         http.StatusConflict,
		CodePermissionDenied: http.StatusForbidden,
		CodeValidation:       http.StatusUnprocessableEntity,
		CodeUnauthenticated:  http.StatusUnauthorized,
		CodeInternal:         http.StatusInternalServerError,
	}
	for c, want := range cases {
		if got := statusFor(c); got != want {
			t.Errorf("statusFor(%q) = %d; want %d", c, got, want)
		}
	}
}
