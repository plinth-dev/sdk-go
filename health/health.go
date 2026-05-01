// Package health is Plinth's dependency-probe registry.
//
// Modules register one [Probe] per dependency (DB, Cerbos, NATS, downstream
// HTTP service); the [Registry] runs them in parallel on each request and
// reports per-dependency status. Use [Registry.HTTPHandler] for K8s readiness
// and [Registry.LivenessHandler] for K8s liveness — they're deliberately
// different.
//
// See https://plinth.run/sdk/go/health/ for the design rationale.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Status of a probe or the aggregate. The three-state enum captures
// "degraded but serving" — a real state in production (slow Cerbos,
// read-only DB failover) — that boolean health flattens.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
	StatusFailing  Status = "failing"
)

// Result is what a [Probe] returns. LatencyMs measures the probe's own
// duration, capped by the registry's per-probe timeout.
type Result struct {
	Name      string `json:"name"`
	Status    Status `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Detail    string `json:"detail,omitempty"`
}

// Probe is anything that knows how to check itself in bounded time.
// Implementations MUST honor ctx cancellation — the registry passes a
// per-probe timeout via context.
type Probe interface {
	// Name returns a stable short identifier ("postgres", "cerbos", etc.).
	// Used for logs, metrics, and the response JSON.
	Name() string

	// Check runs the probe and returns its Result. Implementations should
	// return Status:Failing with a Detail describing the cause on error;
	// returning early on ctx.Done() is required.
	Check(ctx context.Context) Result
}

// ── Registry ─────────────────────────────────────────────────────────

// Registry owns the probes and serves the HTTP responses.
type Registry struct {
	logger       *slog.Logger
	probeTimeout time.Duration

	mu     sync.RWMutex
	probes []Probe
}

// Option configures a [Registry] at construction.
type Option func(*Registry)

// WithLogger sets the slog.Logger used for probe-failure logs.
// Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(r *Registry) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithProbeTimeout caps the per-probe budget. Defaults to 2 seconds.
// Probes that exceed the budget are reported as Failing with Detail "timeout".
func WithProbeTimeout(d time.Duration) Option {
	return func(r *Registry) {
		if d > 0 {
			r.probeTimeout = d
		}
	}
}

// New returns a Registry ready to accept probes.
func New(opts ...Option) *Registry {
	r := &Registry{
		logger:       slog.Default(),
		probeTimeout: 2 * time.Second,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register adds a probe. Threadsafe.
//
// Probes are checked in registration order; aggregate Status is the worst.
// Re-registering a probe with the same name doesn't replace — it adds a
// second entry. Caller should construct with a unique name per dependency.
func (r *Registry) Register(p Probe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes = append(r.probes, p)
}

// CheckAll runs every registered probe in parallel, with each probe bounded
// by the registry's WithProbeTimeout. Returns aggregate Status (worst of any)
// and per-probe Results in registration order.
//
// Honors ctx cancellation: if the caller's ctx ends, in-flight probes
// see their per-probe ctx cancelled too.
func (r *Registry) CheckAll(ctx context.Context) (Status, []Result) {
	r.mu.RLock()
	probes := make([]Probe, len(r.probes))
	copy(probes, r.probes)
	timeout := r.probeTimeout
	r.mu.RUnlock()

	results := make([]Result, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func(i int, p Probe) {
			defer wg.Done()
			pCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			res := p.Check(pCtx)
			elapsed := time.Since(start).Milliseconds()
			res.Name = p.Name()
			if res.LatencyMs == 0 {
				res.LatencyMs = elapsed
			}
			// If the probe didn't notice the ctx deadline, classify as Failing.
			if pCtx.Err() != nil && res.Status == StatusOK {
				res.Status = StatusFailing
				if res.Detail == "" {
					res.Detail = "timeout"
				}
			}
			results[i] = res
		}(i, p)
	}
	wg.Wait()

	return aggregate(results), results
}

// aggregate computes the worst-of-any across results. Empty results = OK.
func aggregate(results []Result) Status {
	worst := StatusOK
	for _, r := range results {
		switch r.Status {
		case StatusFailing:
			return StatusFailing // immediate short-circuit
		case StatusDegraded:
			worst = StatusDegraded
		}
	}
	return worst
}

// HTTPHandler returns an http.Handler that runs CheckAll on every request
// and emits the result as JSON. Status code: 200 for OK or Degraded, 503
// for Failing.
//
// Wire this on the K8s readiness probe path (default `/health`). 503 tells
// the kubelet to remove the pod from service endpoints.
func (r *Registry) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		status, results := r.CheckAll(req.Context())

		body := response{
			Status:  status,
			Results: results,
		}
		w.Header().Set("Content-Type", "application/json")
		if status == StatusFailing {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(body)
	})
}

// LivenessHandler is a separate cheap-check handler suitable for K8s
// liveness probes. It does NOT run the dependency probes — it just returns
// 200 if the server is responsive.
//
// The split matters: liveness fails → kubelet restarts the pod. Readiness
// fails → kubelet removes from service. A flapping Cerbos shouldn't restart
// every pod in the fleet.
func (r *Registry) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// response is the JSON body served by HTTPHandler.
type response struct {
	Status  Status   `json:"status"`
	Results []Result `json:"results"`
}

// ── Built-in probes ──────────────────────────────────────────────────

// PingableDB is anything that can be PingContext'd. Both database/sql.DB
// and pgx pools satisfy this.
type PingableDB interface {
	PingContext(ctx context.Context) error
}

type pgPing struct {
	name string
	db   PingableDB
}

func (p pgPing) Name() string { return p.name }
func (p pgPing) Check(ctx context.Context) Result {
	if err := p.db.PingContext(ctx); err != nil {
		return Result{Status: StatusFailing, Detail: err.Error()}
	}
	return Result{Status: StatusOK}
}

// PgPing pings a database connection. Suitable for any type implementing
// PingContext (database/sql.DB, pgxpool.Pool, etc.).
func PgPing(name string, db PingableDB) Probe { return pgPing{name: name, db: db} }

type httpGet struct {
	name    string
	url     string
	timeout time.Duration
}

func (h httpGet) Name() string { return h.name }
func (h httpGet) Check(ctx context.Context) Result {
	probeCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, h.url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{Status: StatusFailing, Detail: err.Error()}
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return Result{Status: StatusOK}
	case res.StatusCode >= 500:
		return Result{Status: StatusFailing, Detail: res.Status}
	default:
		return Result{Status: StatusDegraded, Detail: res.Status}
	}
}

// HTTPGet probes an upstream HTTP endpoint. 2xx → OK, 5xx → Failing,
// other non-2xx (3xx/4xx) → Degraded. The timeout caps the GET; the
// registry's per-probe timeout still applies.
func HTTPGet(name, url string, timeout time.Duration) Probe {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return httpGet{name: name, url: url, timeout: timeout}
}

// Pinger is anything with a Ping method that takes a context. The
// Cerbos client satisfies this; we accept it as an interface to keep
// the package dependency-free.
type Pinger interface {
	Ping(ctx context.Context) error
}

type pingerProbe struct {
	name string
	p    Pinger
}

func (pp pingerProbe) Name() string { return pp.name }
func (pp pingerProbe) Check(ctx context.Context) Result {
	if err := pp.p.Ping(ctx); err != nil {
		return Result{Status: StatusFailing, Detail: err.Error()}
	}
	return Result{Status: StatusOK}
}

// CerbosCheck probes a Cerbos PDP via any value with a Ping(ctx) method.
// The sdk-go/authz Client satisfies the interface; we don't import it to
// avoid a cycle (and to let callers wire bespoke pingers).
func CerbosCheck(name string, p Pinger) Probe { return pingerProbe{name: name, p: p} }

type funcProbe struct {
	name string
	fn   func(ctx context.Context) error
}

func (f funcProbe) Name() string { return f.name }
func (f funcProbe) Check(ctx context.Context) Result {
	if err := f.fn(ctx); err != nil {
		return Result{Status: StatusFailing, Detail: err.Error()}
	}
	return Result{Status: StatusOK}
}

// Func wraps a closure as a Probe. Convenient when the dependency is
// bespoke and a one-line check is enough.
//
//	reg.Register(health.Func("kafka-stream", func(ctx context.Context) error {
//	    return kafka.IsReady(ctx, streamName)
//	}))
func Func(name string, fn func(ctx context.Context) error) Probe {
	return funcProbe{name: name, fn: fn}
}
