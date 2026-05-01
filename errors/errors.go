package apperrors

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors. Match these via errors.Is — never string-compare.
//
// The sentinel value is the wire-stable Code string so that callers writing
// errors.Is(err, ErrNotFound) match both AppErrors constructed with
// CodeNotFound and arbitrary errors that wrap one of the sentinels.
var (
	ErrNotFound         = errors.New("not_found")
	ErrConflict         = errors.New("conflict")
	ErrPermissionDenied = errors.New("permission_denied")
	ErrValidation       = errors.New("validation")
	ErrUnauthenticated  = errors.New("unauthenticated")
	ErrInternal         = errors.New("internal")
)

// Code is the public, stable, machine-readable error label.
//
// Codes are part of the wire contract: clients can switch on them.
// They map 1:1 to a sentinel and an HTTP status. The constant string
// values must not change without a major-version bump.
type Code string

// Code constants — every public AppError carries exactly one of these.
const (
	CodeNotFound         Code = "not_found"
	CodeConflict         Code = "conflict"
	CodePermissionDenied Code = "permission_denied"
	CodeValidation       Code = "validation"
	CodeUnauthenticated  Code = "unauthenticated"
	CodeInternal         Code = "internal"
)

// AppError is the structured error type. Implements the error interface
// and unwraps to its sentinel so errors.Is(err, ErrNotFound) works.
//
// The cause field is unexported because the interesting cross-layer signal
// is the (Code, Message, Fields) triple, not the underlying transport error.
// Use Unwrap or errors.Is/As to traverse the chain when you need it.
type AppError struct {
	Code    Code
	Message string            // diagnostic; not user-facing
	Fields  map[string]string // for Validation errors; field-name -> human reason
	cause   error             // unwraps to a sentinel by default; may be a wrapped lower-level error
}

// Error returns the human-readable diagnostic message.
//
// Format: "<code>: <message>" — code first so logs are easy to grep.
// Wrapped causes (from Wrap) are appended as ": <cause>" for transparency;
// factory-constructed errors (NotFound, Conflict, etc.) skip the suffix
// because their cause is the sentinel itself, which would just repeat the code.
func (e *AppError) Error() string {
	if e == nil {
		return "<nil *AppError>"
	}
	// A wrappedCause carries an underlying error worth surfacing.
	// Factory errors have cause == sentinel, which is uninteresting to print.
	if wc, ok := e.cause.(wrappedCause); ok && wc.inner != nil {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Message, wc.inner)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the cause for errors.Is / errors.As traversal.
//
// For factory-constructed errors (NotFound, Conflict, etc.) the cause is
// the sentinel matching the Code, so errors.Is(err, ErrNotFound) returns
// true for any AppError built with CodeNotFound. For Wrap-constructed
// errors the cause may be the underlying transport error; sentinel
// matching still works because Unwrap walks one step and the rest is
// errors.Is's responsibility.
func (e *AppError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// HTTPStatus returns the canonical HTTP status code for the AppError's Code.
// Used by HTTPMiddleware; safe for callers to use directly.
func (e *AppError) HTTPStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	return statusFor(e.Code)
}

// sentinelFor returns the sentinel that matches a Code. Internal helper.
// New Codes must be added here; the test in errors_test.go enforces this.
func sentinelFor(c Code) error {
	switch c {
	case CodeNotFound:
		return ErrNotFound
	case CodeConflict:
		return ErrConflict
	case CodePermissionDenied:
		return ErrPermissionDenied
	case CodeValidation:
		return ErrValidation
	case CodeUnauthenticated:
		return ErrUnauthenticated
	case CodeInternal:
		return ErrInternal
	default:
		return ErrInternal
	}
}

// statusFor maps a Code to its canonical HTTP status. Internal helper.
func statusFor(c Code) int {
	switch c {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeValidation:
		return http.StatusUnprocessableEntity
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// titleFor maps a Code to the human-readable Title field used in the
// RFC 7807 Problem document. Always English; i18n is the frontend's job.
func titleFor(c Code) string {
	switch c {
	case CodeNotFound:
		return "Not found"
	case CodeConflict:
		return "Conflict"
	case CodePermissionDenied:
		return "Permission denied"
	case CodeValidation:
		return "Validation failed"
	case CodeUnauthenticated:
		return "Unauthenticated"
	case CodeInternal:
		return "Internal error"
	default:
		return "Internal error"
	}
}

// ── Factory helpers ──────────────────────────────────────────────────

// NotFound builds a 404 AppError for a missing resource by kind + id.
// Example: NotFound("item", "abc-123") → "not_found: item abc-123 not found".
func NotFound(resource, id string) *AppError {
	return &AppError{
		Code:    CodeNotFound,
		Message: fmt.Sprintf("%s %s not found", resource, id),
		cause:   ErrNotFound,
	}
}

// Conflict builds a 409 AppError. Format-string-style for ergonomics.
func Conflict(format string, args ...any) *AppError {
	return &AppError{
		Code:    CodeConflict,
		Message: fmt.Sprintf(format, args...),
		cause:   ErrConflict,
	}
}

// PermissionDenied builds a 403 AppError naming the action that was denied.
// Example: PermissionDenied("items:delete") → "permission_denied: not allowed: items:delete".
func PermissionDenied(action string) *AppError {
	return &AppError{
		Code:    CodePermissionDenied,
		Message: fmt.Sprintf("not allowed: %s", action),
		cause:   ErrPermissionDenied,
	}
}

// Validation builds a 422 AppError. Fields is the per-field-error map the
// frontend uses to highlight specific inputs. Pass nil if the validation
// failure is whole-body (rare).
func Validation(msg string, fields map[string]string) *AppError {
	return &AppError{
		Code:    CodeValidation,
		Message: msg,
		Fields:  fields,
		cause:   ErrValidation,
	}
}

// Unauthenticated builds a 401 AppError. Distinct from PermissionDenied:
// "we don't know who you are" vs "we know who you are and you can't do this".
func Unauthenticated(msg string) *AppError {
	return &AppError{
		Code:    CodeUnauthenticated,
		Message: msg,
		cause:   ErrUnauthenticated,
	}
}

// Internal builds a 500 AppError. The Message will be sanitized away by
// HTTPMiddleware before serialization to prevent leaking internals to clients.
// Format-string-style for ergonomics; the full text is preserved in logs.
func Internal(format string, args ...any) *AppError {
	return &AppError{
		Code:    CodeInternal,
		Message: fmt.Sprintf(format, args...),
		cause:   ErrInternal,
	}
}

// Wrap builds an AppError around an existing cause. Use when crossing layers
// (e.g. repository pgx.ErrNoRows → Wrap(err, CodeNotFound, "item not found")).
//
// errors.Is(wrapped, sentinel) works for both the cause's own sentinels
// (the original error's chain) and the AppError's Code-derived sentinel.
func Wrap(cause error, code Code, format string, args ...any) *AppError {
	return &AppError{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		cause:   wrappedCause{sentinel: sentinelFor(code), inner: cause},
	}
}

// wrappedCause makes errors.Is succeed for both the Code's sentinel AND
// any sentinel inside the wrapped cause. Internal type, never exposed.
type wrappedCause struct {
	sentinel error
	inner    error
}

func (w wrappedCause) Error() string {
	if w.inner == nil {
		return w.sentinel.Error()
	}
	return w.inner.Error()
}

// Is is the errors.Is hook. Returns true for either the sentinel or
// anything inside the inner cause.
func (w wrappedCause) Is(target error) bool {
	if errors.Is(w.sentinel, target) {
		return true
	}
	return errors.Is(w.inner, target)
}

// Unwrap exposes the inner cause for errors.As traversal.
func (w wrappedCause) Unwrap() error { return w.inner }

// ── Public introspection helpers ─────────────────────────────────────

// CodeOf returns the Code of err if it is an *AppError (or wraps one),
// or CodeInternal otherwise. Useful for logging or branching on error type
// without a manual type assertion.
func CodeOf(err error) Code {
	var ae *AppError
	if errors.As(err, &ae) && ae != nil {
		return ae.Code
	}
	return CodeInternal
}

// HTTPStatusOf returns the HTTP status code that HTTPMiddleware would
// emit for err. Returns 500 for non-AppError values.
func HTTPStatusOf(err error) int {
	return statusFor(CodeOf(err))
}
