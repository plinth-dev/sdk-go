package apperrors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helper: run a handler through HTTPMiddleware and return the response.
func runMiddleware(t *testing.T, opts []MiddlewareOption, h func(w http.ResponseWriter, r *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	mw := HTTPMiddleware(http.HandlerFunc(h), opts...)
	req := httptest.NewRequest(http.MethodGet, "/items/abc", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_NoErrorPassesThrough(t *testing.T) {
	rec := runMiddleware(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q; want 'ok'", rec.Body.String())
	}
}

func TestMiddleware_NotFound(t *testing.T) {
	rec := runMiddleware(t, nil, func(w http.ResponseWriter, r *http.Request) {
		SetError(r, NotFound("item", "abc"))
	})

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q; want application/problem+json", ct)
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem+json: %v", err)
	}
	if p.Code != CodeNotFound {
		t.Errorf("Problem.Code = %q; want %q", p.Code, CodeNotFound)
	}
	if p.Status != http.StatusNotFound {
		t.Errorf("Problem.Status = %d; want 404", p.Status)
	}
	if p.Title != "Not found" {
		t.Errorf("Problem.Title = %q; want 'Not found'", p.Title)
	}
	if !strings.Contains(p.Detail, "abc") {
		t.Errorf("Problem.Detail should mention id; got %q", p.Detail)
	}
	if p.Type != "https://plinth.run/errors/not_found" {
		t.Errorf("Problem.Type = %q; want plinth.run url", p.Type)
	}
}

func TestMiddleware_InternalSanitized(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rec := runMiddleware(t, []MiddlewareOption{WithLogger(logger)},
		func(w http.ResponseWriter, r *http.Request) {
			SetError(r, Internal("DB password rotated mid-flight: %s", "secret-token-xxxx"))
		})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}

	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)

	// Wire response must be sanitized.
	if strings.Contains(p.Detail, "secret-token") {
		t.Errorf("Problem.Detail leaked secret content: %q", p.Detail)
	}
	if p.Detail != "an internal error occurred" {
		t.Errorf("Problem.Detail = %q; want generic message", p.Detail)
	}

	// Logs MUST contain the full original.
	logged := buf.String()
	if !strings.Contains(logged, "secret-token") {
		t.Errorf("logger should have captured full detail; got %q", logged)
	}
}

func TestMiddleware_ValidationFields(t *testing.T) {
	fields := map[string]string{
		"email": "must be a valid email",
		"age":   "must be at least 18",
	}
	rec := runMiddleware(t, nil, func(w http.ResponseWriter, r *http.Request) {
		SetError(r, Validation("body validation failed", fields))
	})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422", rec.Code)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Fields) != 2 {
		t.Errorf("Fields len = %d; want 2", len(p.Fields))
	}
	if p.Fields["email"] != "must be a valid email" {
		t.Errorf("Fields[email] = %q; mismatch", p.Fields["email"])
	}
}

func TestMiddleware_TraceIDFromContext(t *testing.T) {
	// Plug in a fake trace-id provider.
	traceFn := func(ctx context.Context) string {
		return "0123456789abcdef"
	}
	rec := runMiddleware(t, []MiddlewareOption{WithTraceIDFunc(traceFn)},
		func(w http.ResponseWriter, r *http.Request) {
			SetError(r, NotFound("item", "abc"))
		})

	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.TraceID != "0123456789abcdef" {
		t.Errorf("TraceID = %q; want fake id", p.TraceID)
	}
}

func TestMiddleware_PlainErrorBecomesInternal(t *testing.T) {
	// An error that isn't an *AppError should still produce a 500 problem+json.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rec := runMiddleware(t, []MiddlewareOption{WithLogger(logger)},
		func(w http.ResponseWriter, r *http.Request) {
			SetError(r, errors.New("bare error"))
		})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Code != CodeInternal {
		t.Errorf("Problem.Code = %q; want CodeInternal", p.Code)
	}
}

func TestMiddleware_NoOverwriteIfHandlerWrote(t *testing.T) {
	// If the handler wrote a response AND set an error, the response wins.
	// (Buggy handlers shouldn't get clobbered by the middleware.)
	rec := runMiddleware(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "I am a teapot")
		SetError(r, NotFound("item", "abc"))
	})

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 (teapot, handler wrote first)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "teapot") {
		t.Errorf("body = %q; want teapot text", rec.Body.String())
	}
}

func TestSetGetError(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if got := GetError(req); got != nil {
		t.Errorf("GetError pre-set should be nil; got %v", got)
	}
	target := NotFound("x", "y")
	SetError(req, target)
	if got := GetError(req); !errors.Is(got, target) {
		t.Errorf("GetError after Set should return same value; got %v", got)
	}
	// Calling SetError(_, nil) is a no-op.
	SetError(req, nil)
	if got := GetError(req); !errors.Is(got, target) {
		t.Errorf("SetError(nil) should not clobber; got %v", got)
	}
}

func TestWriteProblem_Standalone(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	WriteProblem(rec, req, NotFound("item", "z"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("WriteProblem status = %d; want 404", rec.Code)
	}
	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Code != CodeNotFound {
		t.Errorf("Problem.Code = %q; want CodeNotFound", p.Code)
	}
}
