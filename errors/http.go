package apperrors

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Problem is the RFC 7807 application/problem+json wire shape that
// HTTPMiddleware emits. Field names match the RFC; Code and Fields are
// Plinth-specific extensions.
//
// See https://datatracker.ietf.org/doc/html/rfc7807.
type Problem struct {
	Type    string            `json:"type"`              // "https://plinth.run/errors/<code>"
	Title   string            `json:"title"`             // human-readable; matches the Code's Title
	Status  int                `json:"status"`            // HTTP status
	Detail  string             `json:"detail,omitempty"`  // sanitized for CodeInternal
	Code    Code               `json:"code"`              // machine-readable; the contract
	TraceID string             `json:"trace_id,omitempty"`
	Fields  map[string]string  `json:"fields,omitempty"`  // validation only
}

// errorContextKey is the context key under which SetError stashes the error
// for HTTPMiddleware to retrieve. Unexported — callers go through SetError /
// GetError, not the key directly.
type errorContextKey struct{}

// SetError stashes an error on the request's context so that HTTPMiddleware
// emits the canonical problem+json response. Handlers call this and return.
//
// Safe to call multiple times: the most recent error wins.
func SetError(r *http.Request, err error) {
	if err == nil {
		return
	}
	ctx := context.WithValue(r.Context(), errorContextKey{}, err)
	*r = *r.WithContext(ctx)
}

// GetError returns the error previously set by SetError, or nil if none.
// Useful in deeper middleware that wants to observe the handler's outcome.
func GetError(r *http.Request) error {
	v := r.Context().Value(errorContextKey{})
	if v == nil {
		return nil
	}
	if e, ok := v.(error); ok {
		return e
	}
	return nil
}

// HTTPMiddleware wraps an http.Handler. When the wrapped handler returns,
// the middleware checks the request context for an error stashed via SetError
// and, if present, emits the canonical problem+json response.
//
// Errors with CodeInternal have their Message replaced with a generic
// "an internal error occurred" before serialization, to prevent leaking
// implementation details to clients. The original error is logged at
// slog.Error with the full message and trace ID for ops correlation.
//
// MiddlewareOption customizes behavior; pass none for sensible defaults.
func HTTPMiddleware(next http.Handler, opts ...MiddlewareOption) http.Handler {
	cfg := middlewareConfig{
		logger:        slog.Default(),
		typeURIPrefix: "https://plinth.run/errors/",
		traceIDFunc:   noTraceID,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap ResponseWriter to detect whether the handler already wrote a
		// response. If it did, we don't overwrite — handlers that bypass
		// SetError keep working. SetError is the opt-in path.
		ww := &responseObserver{ResponseWriter: w}
		next.ServeHTTP(ww, r)

		err := GetError(r)
		if err == nil || ww.wroteHeader {
			return
		}
		writeProblem(ww, r, err, &cfg)
	})
}

// MiddlewareOption configures HTTPMiddleware. Apply with HTTPMiddleware(next, WithLogger(l), ...).
type MiddlewareOption func(*middlewareConfig)

type middlewareConfig struct {
	logger        *slog.Logger
	typeURIPrefix string
	traceIDFunc   func(ctx context.Context) string
}

// WithLogger overrides the slog.Logger used to log Internal errors before
// sanitization. Defaults to slog.Default().
func WithLogger(l *slog.Logger) MiddlewareOption {
	return func(c *middlewareConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithTypeURIPrefix overrides the "type" URL prefix in the Problem document.
// Default is "https://plinth.run/errors/<code>".
func WithTypeURIPrefix(prefix string) MiddlewareOption {
	return func(c *middlewareConfig) {
		c.typeURIPrefix = prefix
	}
}

// WithTraceIDFunc lets callers plug in their tracing system. The function
// is called with the request context; if it returns a non-empty string,
// it's included as Problem.TraceID. Default returns "".
//
// Wire it in main():
//
//	apperrors.HTTPMiddleware(handler,
//	    apperrors.WithTraceIDFunc(func(ctx context.Context) string {
//	        if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
//	            return span.SpanContext().TraceID().String()
//	        }
//	        return ""
//	    }),
//	)
func WithTraceIDFunc(fn func(ctx context.Context) string) MiddlewareOption {
	return func(c *middlewareConfig) {
		if fn != nil {
			c.traceIDFunc = fn
		}
	}
}

func noTraceID(context.Context) string { return "" }

// responseObserver wraps http.ResponseWriter and remembers whether
// WriteHeader / Write was ever called. Used by HTTPMiddleware to avoid
// double-writing if the handler already produced a response.
type responseObserver struct {
	http.ResponseWriter
	wroteHeader bool
}

func (r *responseObserver) WriteHeader(code int) {
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseObserver) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// writeProblem serializes err as RFC 7807 problem+json. Caller guarantees
// no response has been written yet.
func writeProblem(w http.ResponseWriter, r *http.Request, err error, cfg *middlewareConfig) {
	var ae *AppError
	if !errors.As(err, &ae) || ae == nil {
		// Anything that isn't an AppError is treated as a 500 Internal error.
		ae = Internal("non-app error: %v", err)
	}

	traceID := cfg.traceIDFunc(r.Context())

	// Internal errors: log full detail, sanitize the wire response.
	detail := ae.Message
	if ae.Code == CodeInternal {
		cfg.logger.Error("internal error",
			slog.String("error", ae.Error()),
			slog.String("trace_id", traceID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
		detail = "an internal error occurred"
	}

	p := Problem{
		Type:    cfg.typeURIPrefix + string(ae.Code),
		Title:   titleFor(ae.Code),
		Status:  ae.HTTPStatus(),
		Detail:  detail,
		Code:    ae.Code,
		TraceID: traceID,
		Fields:  ae.Fields,
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteProblem is a public escape hatch — emits the canonical problem+json
// response for err without going through SetError + HTTPMiddleware. Useful
// for handlers that don't sit behind the middleware (panics, custom routers,
// etc.). Most call sites should prefer SetError.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error, opts ...MiddlewareOption) {
	cfg := middlewareConfig{
		logger:        slog.Default(),
		typeURIPrefix: "https://plinth.run/errors/",
		traceIDFunc:   noTraceID,
	}
	for _, o := range opts {
		o(&cfg)
	}
	writeProblem(w, r, err, &cfg)
}
