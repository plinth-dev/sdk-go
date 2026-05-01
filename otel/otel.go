// Package otel is Plinth's OpenTelemetry SDK initialisation.
//
// One [Init] call in main configures the global tracer provider with the
// resource attributes Plinth expects (service.name, service.version,
// module.name, deployment.environment), wires the OTLP exporter, and
// installs the W3C trace-context propagator. The caller defers the
// returned shutdown.
//
// The package name is `otel`, identical to the official OpenTelemetry
// Go module's root package. Consumers naming their import alias `otel`
// gets our package; our own source file aliases the SDK as `sdkotel`
// internally to disambiguate. See https://plinth.run/sdk/go/otel/.
package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// Defaults; exposed so callers can compute "is this the default?" if needed.
const (
	DefaultExporterEndpoint = "http://otel-collector.observability:4318"
	DefaultExporterProtocol = "http"

	// EnvOTELSamplerArg overrides Options.TracesSamplerArg.
	EnvOTELSamplerArg = "OTEL_TRACES_SAMPLER_ARG"
	// EnvENV is read when Options.Environment is empty.
	EnvENV = "ENV"
)

// AttrModuleKey is the OTel attribute key for "module.name" — Plinth's
// per-module dimension. Use [AttrModule] to build the attribute.
const AttrModuleKey = attribute.Key("module.name")

// Options configure [Init]. ServiceName is the only required field; sensible
// defaults fill the rest.
type Options struct {
	// ServiceName is the OTel `service.name` resource attribute. Required.
	// Use the running module's identifier (e.g. "items-api").
	ServiceName string

	// ServiceVersion populates `service.version`. Defaults to "unknown" if empty.
	// Inject from build-time ldflags: -X 'main.version=$(git describe --tags)'.
	ServiceVersion string

	// Environment populates `deployment.environment.name`. Defaults to
	// os.Getenv(EnvENV) → "dev". Recognised values: "production", "staging", "dev".
	Environment string

	// ModuleName populates the Plinth-specific `module.name` attribute.
	// Optional; matches the convention used by sdk-go/audit and sdk-go/errors.
	ModuleName string

	// ExporterEndpoint is the OTLP target. Defaults to the in-cluster
	// collector at http://otel-collector.observability:4318.
	ExporterEndpoint string

	// ExporterProtocol selects "http" (OTLP/HTTP, default) or "grpc".
	ExporterProtocol string

	// ExporterHeaders are sent on every export. Useful for hosted backends.
	ExporterHeaders map[string]string

	// TracesSamplerArg is the parent-based ratio sampler arg in [0, 1].
	// Defaults: 1.0 in dev, 0.5 in staging, 0.05 in production.
	// Set OTEL_TRACES_SAMPLER_ARG in the env to override this (and any
	// explicit value passed in code — env wins, matching the OpenTelemetry
	// spec). Pointer so zero is distinguishable from unset; pass nil
	// (or leave zero-value) to use env-or-default, &0 for explicit
	// no-sampling.
	TracesSamplerArg *float64

	// BatchTimeout caps how long the BatchSpanProcessor buffers before flushing.
	// Defaults to the SDK's default (5 seconds).
	BatchTimeout time.Duration

	// Logger receives init / exporter / shutdown messages. Defaults to slog.Default().
	Logger *slog.Logger

	// Exporter overrides the OTLP exporter selection. Tests pass in-memory
	// recorders here; production typically leaves this nil and lets Init
	// build an OTLP exporter from ExporterEndpoint / ExporterProtocol.
	Exporter sdktrace.SpanExporter
}

// Init configures the global tracer provider, propagator, and resource.
// Returns a shutdown closure that flushes pending spans; defer it from main.
//
// Standard usage:
//
//	shutdown, err := otel.Init(ctx, otel.Options{
//	    ServiceName:    "items-api",
//	    ServiceVersion: version,
//	    ModuleName:     "items",
//	})
//	if err != nil { log.Fatal(err) }
//	defer func() { _ = shutdown(context.Background()) }()
//
// Init returns an error for any configuration problem (missing ServiceName,
// unknown protocol, malformed endpoint). The OTLP exporter doesn't dial
// eagerly; an unreachable collector at startup doesn't fail Init — the SDK
// retries each batch internally.
func Init(ctx context.Context, opts Options) (shutdown func(context.Context) error, err error) {
	if opts.ServiceName == "" {
		return nil, errors.New("otel: Options.ServiceName is required")
	}
	if opts.ServiceVersion == "" {
		opts.ServiceVersion = "unknown"
	}
	if opts.Environment == "" {
		opts.Environment = os.Getenv(EnvENV)
		if opts.Environment == "" {
			opts.Environment = "dev"
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	samplerArg := resolveSamplerArg(opts.TracesSamplerArg, opts.Environment)

	res, err := buildResource(opts)
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	// When the caller provides an Exporter explicitly (typical in tests),
	// use a SimpleSpanProcessor for immediate visibility. Production paths
	// (Exporter == nil → built from ExporterEndpoint) get a BatchSpanProcessor.
	var processor sdktrace.SpanProcessor
	if opts.Exporter != nil {
		processor = sdktrace.NewSimpleSpanProcessor(opts.Exporter)
	} else {
		exporter, err := buildOTLPExporter(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("otel: %w", err)
		}
		var bspOpts []sdktrace.BatchSpanProcessorOption
		if opts.BatchTimeout > 0 {
			bspOpts = append(bspOpts, sdktrace.WithBatchTimeout(opts.BatchTimeout))
		}
		processor = sdktrace.NewBatchSpanProcessor(exporter, bspOpts...)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(processor),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(samplerArg),
		)),
	)

	sdkotel.SetTracerProvider(tp)
	sdkotel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// defaultSampleRate returns the sane default sampling ratio for the given env.
func defaultSampleRate(env string) float64 {
	switch env {
	case "production":
		return 0.05
	case "staging":
		return 0.5
	default:
		return 1.0
	}
}

// resolveSamplerArg returns the effective sampler ratio. Precedence:
// OTEL_TRACES_SAMPLER_ARG env var (matches OpenTelemetry spec) → explicit
// Options.TracesSamplerArg → defaultSampleRate for the environment.
//
// Returning a value clamped to [0, 1] — TraceIDRatioBased treats values
// outside that range identically to 1.0 / 0.0 respectively, but explicit
// clamping makes the intent clear and avoids subtle surprises if the SDK
// changes that behavior.
func resolveSamplerArg(explicit *float64, environment string) float64 {
	if raw := os.Getenv(EnvOTELSamplerArg); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return clampSampleRate(f)
		}
	}
	if explicit != nil {
		return clampSampleRate(*explicit)
	}
	return defaultSampleRate(environment)
}

func clampSampleRate(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// buildResource composes the Plinth standard resource attributes with the
// SDK's default detector (host, OS, runtime, etc.).
func buildResource(opts Options) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.ServiceVersion),
		semconv.DeploymentEnvironmentName(opts.Environment),
	}
	if opts.ModuleName != "" {
		attrs = append(attrs, AttrModuleKey.String(opts.ModuleName))
	}
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
}

// buildOTLPExporter constructs the OTLP exporter for the configured protocol.
// The endpoint string is parsed as host[:port]; the otlptracehttp/grpc options
// expect that form (no scheme).
func buildOTLPExporter(ctx context.Context, opts Options) (sdktrace.SpanExporter, error) {
	endpoint := opts.ExporterEndpoint
	if endpoint == "" {
		endpoint = DefaultExporterEndpoint
	}
	protocol := opts.ExporterProtocol
	if protocol == "" {
		protocol = DefaultExporterProtocol
	}

	host := stripScheme(endpoint)
	switch protocol {
	case "http":
		return otlptracehttp.New(ctx,
			otlptracehttp.WithEndpoint(host),
			otlptracehttp.WithInsecure(),
			otlptracehttp.WithHeaders(opts.ExporterHeaders),
		)
	case "grpc":
		return otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(host),
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithHeaders(opts.ExporterHeaders),
		)
	default:
		return nil, fmt.Errorf("otel: unknown ExporterProtocol %q (want 'http' or 'grpc')", protocol)
	}
}

// stripScheme removes a leading "http://" or "https://" from s. The OTLP
// exporter options take host[:port] only, but operators usually configure
// the full URL — be friendly to both.
func stripScheme(s string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if strings.HasPrefix(s, prefix) {
			return s[len(prefix):]
		}
	}
	return s
}

// RecordError tags the current span with the error and sets its status.
// No-op if ctx has no span or err is nil.
//
// Convenience over the verbose otel/codes + span.RecordError + span.SetStatus
// combination most call sites end up writing.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// AttrModule returns the [AttrModuleKey] attribute populated with name.
// Use for manual span tagging when the module name isn't already on the
// resource (e.g. cross-module spans).
func AttrModule(name string) attribute.KeyValue {
	return AttrModuleKey.String(name)
}

// HTTPMiddleware wraps an http.Handler with otelhttp.NewHandler plus
// Plinth-specific span-name customisation (METHOD path).
//
// For chi routers, callers can wire WithRouteFromContext (future option)
// to use the matched route pattern instead of the literal path — that's
// more useful for cardinality control.
func HTTPMiddleware(next http.Handler, opts ...HTTPOption) http.Handler {
	cfg := httpConfig{operationName: "http.server"}
	for _, o := range opts {
		o(&cfg)
	}
	return otelhttp.NewHandler(next, cfg.operationName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

// HTTPOption customizes [HTTPMiddleware].
type HTTPOption func(*httpConfig)

type httpConfig struct {
	operationName string
}

// WithOperationName overrides the default span-name prefix ("http.server").
func WithOperationName(name string) HTTPOption {
	return func(c *httpConfig) {
		if name != "" {
			c.operationName = name
		}
	}
}

