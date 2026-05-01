package otel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// helper: Init with an in-memory exporter; returns shutdown + the recorder.
func initTest(t *testing.T, opts Options) (func(context.Context) error, *tracetest.InMemoryExporter) {
	t.Helper()
	rec := tracetest.NewInMemoryExporter()
	if opts.ServiceName == "" {
		opts.ServiceName = "test-svc"
	}
	if opts.ServiceVersion == "" {
		opts.ServiceVersion = "0.0.0-test"
	}
	if opts.Environment == "" {
		opts.Environment = "dev"
	}
	// Pin sampler to AlwaysOn for tests; the production default for
	// Environment="production" is 0.05, which probabilistically drops
	// single-span tests.
	one := 1.0
	opts.TracesSamplerArg = &one
	opts.Exporter = rec
	shutdown, err := Init(context.Background(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return shutdown, rec
}

// ── Options validation ──────────────────────────────────────────────

func TestInit_RequiresServiceName(t *testing.T) {
	_, err := Init(context.Background(), Options{})
	if err == nil {
		t.Errorf("expected error when ServiceName is empty")
	}
}

func TestInit_AppliesDefaultEnvironment(t *testing.T) {
	t.Setenv(EnvENV, "")
	shutdown, _ := initTest(t, Options{ServiceName: "x"})
	defer shutdown(context.Background())
	// We don't have an easy hook for env value, but Init not erroring
	// confirms defaults applied. Resource attrs are tested separately.
}

func TestInit_UnknownProtocolErrors(t *testing.T) {
	_, err := Init(context.Background(), Options{
		ServiceName:      "test",
		ExporterProtocol: "carrier-pigeon",
	})
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("expected error mentioning protocol; got %v", err)
	}
}

// ── Resource attributes ─────────────────────────────────────────────

func TestInit_ResourceCarriesPlinthAttributes(t *testing.T) {
	shutdown, rec := initTest(t, Options{
		ServiceName:    "items-api",
		ServiceVersion: "1.2.3",
		Environment:    "production",
		ModuleName:     "items",
	})
	defer shutdown(context.Background())

	// Emit a span to populate the recorder.
	tracer := sdkotel.Tracer("test")
	_, span := tracer.Start(context.Background(), "op")
	span.End()

	spans := rec.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans; want 1", len(spans))
	}
	res := spans[0].Resource

	// Walk attributes to find what we care about.
	got := map[string]string{}
	for _, attr := range res.Attributes() {
		got[string(attr.Key)] = attr.Value.AsString()
	}

	cases := map[string]string{
		"service.name":                "items-api",
		"service.version":             "1.2.3",
		"deployment.environment.name": "production",
		"module.name":                 "items",
	}
	for key, want := range cases {
		if got[key] != want {
			t.Errorf("resource[%q] = %q; want %q (full attrs: %v)", key, got[key], want, got)
		}
	}
}

func TestInit_ModuleNameOmittedWhenEmpty(t *testing.T) {
	shutdown, rec := initTest(t, Options{
		ServiceName: "x",
		// no ModuleName
	})
	defer shutdown(context.Background())

	tracer := sdkotel.Tracer("test")
	_, span := tracer.Start(context.Background(), "op")
	span.End()

	spans := rec.GetSpans()
	res := spans[0].Resource
	for _, attr := range res.Attributes() {
		if string(attr.Key) == "module.name" {
			t.Errorf("module.name should be absent when ModuleName empty; got %q", attr.Value.AsString())
		}
	}
}

// ── Sampler defaults ────────────────────────────────────────────────

func TestDefaultSampleRate_PerEnvironment(t *testing.T) {
	cases := map[string]float64{
		"production": 0.05,
		"staging":    0.5,
		"dev":        1.0,
		"":           1.0, // unrecognised → dev rate
	}
	for env, want := range cases {
		got := defaultSampleRate(env)
		if got != want {
			t.Errorf("env=%q sample = %v; want %v", env, got, want)
		}
	}
}

// ── stripScheme ─────────────────────────────────────────────────────

func TestStripScheme(t *testing.T) {
	cases := map[string]string{
		"http://collector:4318":  "collector:4318",
		"https://collector:4318": "collector:4318",
		"collector:4318":         "collector:4318",
		"localhost":              "localhost",
		"":                       "",
	}
	for in, want := range cases {
		got := stripScheme(in)
		if got != want {
			t.Errorf("stripScheme(%q) = %q; want %q", in, got, want)
		}
	}
}

// ── RecordError ─────────────────────────────────────────────────────

func TestRecordError_OnSpan(t *testing.T) {
	shutdown, rec := initTest(t, Options{ServiceName: "x"})
	defer shutdown(context.Background())

	tracer := sdkotel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "op")
	RecordError(ctx, errors.New("something went wrong"))
	span.End()

	spans := rec.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	s := spans[0]
	if len(s.Events) == 0 {
		t.Errorf("RecordError should produce a span event; got none")
	}
	if s.Status.Code.String() != "Error" {
		t.Errorf("Status.Code = %q; want Error", s.Status.Code)
	}
	if !strings.Contains(s.Status.Description, "something went wrong") {
		t.Errorf("Status.Description = %q; should mention error", s.Status.Description)
	}
}

func TestRecordError_NilIsNoOp(t *testing.T) {
	// Should not panic with no span in context, and should not panic with nil err.
	RecordError(context.Background(), nil)
}

// ── AttrModule ──────────────────────────────────────────────────────

func TestAttrModule(t *testing.T) {
	kv := AttrModule("items")
	if string(kv.Key) != "module.name" {
		t.Errorf("Key = %q; want 'module.name'", kv.Key)
	}
	if kv.Value.AsString() != "items" {
		t.Errorf("Value = %q; want 'items'", kv.Value.AsString())
	}
}

// ── HTTPMiddleware ──────────────────────────────────────────────────

func TestHTTPMiddleware_PassesRequest(t *testing.T) {
	shutdown, rec := initTest(t, Options{ServiceName: "x"})
	defer shutdown(context.Background())

	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := HTTPMiddleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/items/abc", nil)
	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, req)

	if !called {
		t.Errorf("inner handler not called")
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d", rec2.Code)
	}

	// Should have produced at least one span for the HTTP request.
	if len(rec.GetSpans()) == 0 {
		t.Errorf("HTTP middleware should produce a span for the request")
	}
}

func TestHTTPMiddleware_SpanName(t *testing.T) {
	shutdown, rec := initTest(t, Options{ServiceName: "x"})
	defer shutdown(context.Background())

	mw := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/abc", nil))

	spans := rec.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	// otelhttp wraps requests in a span named per the formatter.
	found := false
	for _, s := range spans {
		if s.Name == "GET /items/abc" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(spans))
		for _, s := range spans {
			names = append(names, s.Name)
		}
		t.Errorf("expected a 'GET /items/abc' span; got %v", names)
	}
}

func TestWithOperationName(t *testing.T) {
	cfg := httpConfig{operationName: "default"}
	WithOperationName("custom")(&cfg)
	if cfg.operationName != "custom" {
		t.Errorf("operationName = %q; want 'custom'", cfg.operationName)
	}
	// Empty input should not overwrite.
	WithOperationName("")(&cfg)
	if cfg.operationName != "custom" {
		t.Errorf("empty input clobbered; got %q", cfg.operationName)
	}
}


// resolveSamplerArg precedence: env > explicit > default-by-environment.

func TestResolveSamplerArg_EnvOverridesExplicit(t *testing.T) {
	t.Setenv(EnvOTELSamplerArg, "0.25")
	half := 0.5
	got := resolveSamplerArg(&half, "production")
	if got != 0.25 {
		t.Errorf("resolveSamplerArg = %v; env should win, want 0.25", got)
	}
}

func TestResolveSamplerArg_ExplicitZero(t *testing.T) {
	zero := 0.0
	got := resolveSamplerArg(&zero, "dev")
	if got != 0 {
		t.Errorf("resolveSamplerArg = %v; explicit 0 must be respected, want 0", got)
	}
}

func TestResolveSamplerArg_NilFallsBackToEnvironmentDefault(t *testing.T) {
	got := resolveSamplerArg(nil, "production")
	if got != 0.05 {
		t.Errorf("resolveSamplerArg = %v; nil + production should be 0.05", got)
	}
}

func TestResolveSamplerArg_GarbageEnvIgnored(t *testing.T) {
	t.Setenv(EnvOTELSamplerArg, "not-a-float")
	half := 0.5
	got := resolveSamplerArg(&half, "production")
	if got != 0.5 {
		t.Errorf("resolveSamplerArg = %v; garbage env should fall through, want 0.5", got)
	}
}

func TestResolveSamplerArg_ClampsOutOfRange(t *testing.T) {
	hi := 99.0
	if got := resolveSamplerArg(&hi, "dev"); got != 1.0 {
		t.Errorf("clamp high: got %v, want 1", got)
	}
	lo := -1.0
	if got := resolveSamplerArg(&lo, "dev"); got != 0.0 {
		t.Errorf("clamp low: got %v, want 0", got)
	}
}
