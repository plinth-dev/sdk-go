package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubProbe lets tests script status + delay per probe.
type stubProbe struct {
	name   string
	status Status
	delay  time.Duration
	detail string
	calls  atomic.Int64
}

func (s *stubProbe) Name() string { return s.name }
func (s *stubProbe) Check(ctx context.Context) Result {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return Result{Status: StatusFailing, Detail: "timeout"}
		}
	}
	return Result{Status: s.status, Detail: s.detail}
}

// ── Aggregate ────────────────────────────────────────────────────────

func TestAggregate_AllOK(t *testing.T) {
	r := New()
	r.Register(&stubProbe{name: "a", status: StatusOK})
	r.Register(&stubProbe{name: "b", status: StatusOK})
	got, _ := r.CheckAll(context.Background())
	if got != StatusOK {
		t.Errorf("got %q; want OK", got)
	}
}

func TestAggregate_AnyFailing(t *testing.T) {
	r := New()
	r.Register(&stubProbe{name: "a", status: StatusOK})
	r.Register(&stubProbe{name: "b", status: StatusFailing})
	r.Register(&stubProbe{name: "c", status: StatusOK})
	got, _ := r.CheckAll(context.Background())
	if got != StatusFailing {
		t.Errorf("got %q; want Failing (one failing in mix)", got)
	}
}

func TestAggregate_DegradedWithoutFailing(t *testing.T) {
	r := New()
	r.Register(&stubProbe{name: "a", status: StatusOK})
	r.Register(&stubProbe{name: "b", status: StatusDegraded})
	got, _ := r.CheckAll(context.Background())
	if got != StatusDegraded {
		t.Errorf("got %q; want Degraded", got)
	}
}

func TestAggregate_EmptyRegistryIsOK(t *testing.T) {
	r := New()
	got, results := r.CheckAll(context.Background())
	if got != StatusOK || len(results) != 0 {
		t.Errorf("empty: got %q with %d results", got, len(results))
	}
}

// ── Parallelism ──────────────────────────────────────────────────────

func TestProbesRunInParallel(t *testing.T) {
	r := New(WithProbeTimeout(time.Second))
	for i := 0; i < 5; i++ {
		r.Register(&stubProbe{name: "p", status: StatusOK, delay: 100 * time.Millisecond})
	}
	start := time.Now()
	r.CheckAll(context.Background())
	elapsed := time.Since(start)
	// 5 × 100ms serial = 500ms. Parallel should be ~100ms + overhead.
	if elapsed > 300*time.Millisecond {
		t.Errorf("CheckAll took %v; expected near 100ms (probes should run in parallel)", elapsed)
	}
}

// ── Per-probe timeout ────────────────────────────────────────────────

func TestProbeTimeoutMarksAsFailing(t *testing.T) {
	r := New(WithProbeTimeout(50 * time.Millisecond))
	r.Register(&stubProbe{name: "slow", status: StatusOK, delay: 200 * time.Millisecond})
	got, results := r.CheckAll(context.Background())
	if got != StatusFailing {
		t.Errorf("aggregate = %q; want Failing", got)
	}
	if len(results) != 1 || results[0].Status != StatusFailing {
		t.Errorf("results = %+v; want one Failing", results)
	}
	if results[0].Detail != "timeout" {
		t.Errorf("Detail = %q; want 'timeout'", results[0].Detail)
	}
}

func TestProbeNameInResultEvenIfProbeForgets(t *testing.T) {
	// A buggy probe that returns Result without setting Name should still
	// be identified by the registry from probe.Name().
	r := New()
	r.Register(&stubProbe{name: "real-name", status: StatusOK})
	_, results := r.CheckAll(context.Background())
	if results[0].Name != "real-name" {
		t.Errorf("Name = %q; want 'real-name'", results[0].Name)
	}
}

// ── HTTP handlers ────────────────────────────────────────────────────

func TestHTTPHandler_OKReturns200(t *testing.T) {
	r := New()
	r.Register(&stubProbe{name: "a", status: StatusOK})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	r.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var body response
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != StatusOK {
		t.Errorf("body.Status = %q; want OK", body.Status)
	}
}

func TestHTTPHandler_FailingReturns503(t *testing.T) {
	r := New()
	r.Register(&stubProbe{name: "a", status: StatusFailing})

	rec := httptest.NewRecorder()
	r.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", rec.Code)
	}
}

func TestHTTPHandler_DegradedStillReturns200(t *testing.T) {
	r := New()
	r.Register(&stubProbe{name: "a", status: StatusDegraded})

	rec := httptest.NewRecorder()
	r.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (Degraded still serves)", rec.Code)
	}
}

func TestLivenessHandler_AlwaysOK(t *testing.T) {
	r := New()
	// Register a failing probe — liveness should still return 200.
	r.Register(&stubProbe{name: "fails", status: StatusFailing})

	rec := httptest.NewRecorder()
	r.LivenessHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("liveness should be 200 even with failing probes; got %d", rec.Code)
	}
}

// ── Built-in probes ──────────────────────────────────────────────────

type fakeDB struct{ err error }

func (f fakeDB) PingContext(context.Context) error { return f.err }

func TestPgPing(t *testing.T) {
	probe := PgPing("postgres", fakeDB{})
	res := probe.Check(context.Background())
	if res.Status != StatusOK {
		t.Errorf("ok DB: status = %q", res.Status)
	}

	probe = PgPing("postgres", fakeDB{err: errors.New("connection refused")})
	res = probe.Check(context.Background())
	if res.Status != StatusFailing {
		t.Errorf("failing DB: status = %q; want Failing", res.Status)
	}
	if res.Detail != "connection refused" {
		t.Errorf("Detail = %q; want 'connection refused'", res.Detail)
	}
}

func TestHTTPGet_ResponseClassification(t *testing.T) {
	cases := []struct {
		code int
		want Status
	}{
		{200, StatusOK},
		{204, StatusOK},
		{301, StatusDegraded},
		{404, StatusDegraded},
		{500, StatusFailing},
		{503, StatusFailing},
	}
	for _, c := range cases {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
		}))
		probe := HTTPGet("upstream", ts.URL, time.Second)
		res := probe.Check(context.Background())
		if res.Status != c.want {
			t.Errorf("HTTP %d → %q; want %q", c.code, res.Status, c.want)
		}
		ts.Close()
	}
}

func TestHTTPGet_NetworkError(t *testing.T) {
	probe := HTTPGet("upstream", "http://127.0.0.1:1/nope", 100*time.Millisecond)
	res := probe.Check(context.Background())
	if res.Status != StatusFailing {
		t.Errorf("network error: status = %q; want Failing", res.Status)
	}
}

// fakePinger satisfies the Pinger interface.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestCerbosCheck(t *testing.T) {
	res := CerbosCheck("cerbos", fakePinger{}).Check(context.Background())
	if res.Status != StatusOK {
		t.Errorf("status = %q; want OK", res.Status)
	}

	res = CerbosCheck("cerbos", fakePinger{err: errors.New("PDP unreachable")}).Check(context.Background())
	if res.Status != StatusFailing {
		t.Errorf("status = %q; want Failing", res.Status)
	}
}

func TestFunc_Wrapper(t *testing.T) {
	called := false
	probe := Func("custom", func(ctx context.Context) error {
		called = true
		return nil
	})
	res := probe.Check(context.Background())
	if !called {
		t.Errorf("function not called")
	}
	if res.Status != StatusOK {
		t.Errorf("status = %q; want OK", res.Status)
	}
	if probe.Name() != "custom" {
		t.Errorf("Name = %q; want 'custom'", probe.Name())
	}
}

func TestFunc_PropagatesError(t *testing.T) {
	probe := Func("custom", func(ctx context.Context) error { return errors.New("boom") })
	res := probe.Check(context.Background())
	if res.Status != StatusFailing || res.Detail != "boom" {
		t.Errorf("got %+v; want Failing+'boom'", res)
	}
}

// ── Concurrent registration during checks ────────────────────────────

func TestRegister_DuringCheckAll(t *testing.T) {
	// CheckAll snapshots probes under RLock; registration during checks
	// shouldn't cause races. -race will catch any locking bug.
	r := New()
	for i := 0; i < 5; i++ {
		r.Register(&stubProbe{name: "init", status: StatusOK})
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			r.Register(&stubProbe{name: "added", status: StatusOK})
		}
		close(done)
	}()

	for i := 0; i < 50; i++ {
		r.CheckAll(context.Background())
	}
	<-done
}
