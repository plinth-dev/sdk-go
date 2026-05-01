package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Default option values; exposed so callers can compute "is this the
// default?" if they want to.
const (
	DefaultBufferSize     = 1024
	DefaultDrainTimeout   = 5 * time.Second
	DefaultPublishTimeout = 2 * time.Second
)

// Options configure a [Publisher]. Producer is required. Other fields use
// sensible defaults when zero.
type Options struct {
	// Producer is the transport. Required. See [MemoryProducer] for tests.
	Producer Producer

	// ServiceName populates the CloudEvents `source` field. Recommended:
	// the running module's name (e.g. "items-api"). Defaults to "unknown".
	ServiceName string

	// BufferSize is the in-memory queue depth. When the buffer is full,
	// [Publisher.Publish] drops the oldest event with a slog.Error and
	// increments Stats.Dropped. Defaults to 1024.
	BufferSize int

	// DrainTimeout caps how long [Publisher.Close] waits for the queue to
	// drain before forcing the underlying Producer to close. Defaults to 5s.
	DrainTimeout time.Duration

	// PublishTimeout caps how long the drain goroutine gives the Producer
	// for each call. Defaults to 2s.
	PublishTimeout time.Duration

	// Logger receives drop / error / shutdown messages. Defaults to slog.Default().
	Logger *slog.Logger

	// TraceIDFunc extracts a trace ID from the request context. Called for
	// each Publish; the returned ID is stored on Event.TraceID if Event.TraceID
	// is empty. Plug in OpenTelemetry from the caller (we don't import otel
	// here to keep audit dependency-free):
	//
	//	WithTraceIDFunc(func(ctx context.Context) string {
	//	    if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
	//	        return span.SpanContext().TraceID().String()
	//	    }
	//	    return ""
	//	})
	//
	// Defaults to a no-op (returns "").
	TraceIDFunc func(ctx context.Context) string
}

// Publisher is the surface modules use. Wraps a [Producer] with non-blocking
// publish semantics and a background drain goroutine.
//
// Safe for concurrent use by multiple goroutines.
type Publisher struct {
	producer       Producer
	serviceName    string
	logger         *slog.Logger
	traceIDFunc    func(ctx context.Context) string
	publishTimeout time.Duration
	drainTimeout   time.Duration

	queue     chan Event
	drainDone chan struct{}
	mu        sync.Mutex // serializes drop-oldest swap
	closed    atomic.Bool

	published atomic.Uint64
	dropped   atomic.Uint64
	errored   atomic.Uint64
}

// New returns a Publisher with a background drain goroutine running.
// Callers must call Close on shutdown to drain pending events.
//
// Panics if opts.Producer is nil — that's a programmer error, not a runtime
// failure mode.
func New(opts Options) *Publisher {
	if opts.Producer == nil {
		panic("audit: Options.Producer is required")
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "unknown"
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = DefaultBufferSize
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	if opts.PublishTimeout <= 0 {
		opts.PublishTimeout = DefaultPublishTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.TraceIDFunc == nil {
		opts.TraceIDFunc = noTraceID
	}

	p := &Publisher{
		producer:       opts.Producer,
		serviceName:    opts.ServiceName,
		logger:         opts.Logger,
		traceIDFunc:    opts.TraceIDFunc,
		publishTimeout: opts.PublishTimeout,
		drainTimeout:   opts.DrainTimeout,
		queue:          make(chan Event, opts.BufferSize),
		drainDone:      make(chan struct{}),
	}

	go p.drain()
	return p
}

func noTraceID(context.Context) string { return "" }

// Publish enqueues an event and returns immediately. Never blocks; never
// errors. If the buffer is full, the oldest event is dropped (FIFO) and
// Stats.Dropped is incremented; monitoring should alert on a non-zero
// drop rate.
//
// If e.TraceID is empty, the publisher's TraceIDFunc is called with ctx
// to populate it.
//
// Calling Publish on a closed Publisher silently increments Dropped — the
// caller can't tell which side of Close they're on, and we'd rather log
// noise than block.
func (p *Publisher) Publish(ctx context.Context, e Event) {
	if p.closed.Load() {
		p.dropped.Add(1)
		return
	}
	if e.TraceID == "" {
		e.TraceID = p.traceIDFunc(ctx)
	}
	if (e.Outcome == OutcomeDenied || e.Outcome == OutcomeError) && e.Reason == "" {
		p.logger.Warn("audit event missing reason for non-success outcome",
			slog.String("action", e.Action),
			slog.String("outcome", string(e.Outcome)))
	}

	// Fast path: room in the buffer.
	select {
	case p.queue <- e:
		return
	default:
	}

	// Slow path: drop-oldest under lock.
	p.mu.Lock()
	defer p.mu.Unlock()

	// Re-check after lock — drain may have made room.
	select {
	case p.queue <- e:
		return
	default:
	}

	// Still full. Drop the oldest and write the new.
	dropped := false
	select {
	case <-p.queue:
		dropped = true
	default:
		// Drain consumed between our checks; channel is now empty.
	}
	if dropped {
		p.dropped.Add(1)
		p.logger.Error("audit buffer full — dropped oldest event",
			slog.String("dropped_for_action", e.Action))
	}

	select {
	case p.queue <- e:
	default:
		// Pathological: another producer wrote while we were dropping.
		// Drop the new event we tried to write.
		p.dropped.Add(1)
	}
}

// drain reads events off the queue and forwards them to the Producer.
// Exits when the queue channel is closed (which Close does).
func (p *Publisher) drain() {
	defer close(p.drainDone)
	for e := range p.queue {
		ctx, cancel := context.WithTimeout(context.Background(), p.publishTimeout)
		env := envelope(p.serviceName, e)
		if err := p.producer.Publish(ctx, env); err != nil {
			p.errored.Add(1)
			p.logger.Error("audit publish failed",
				slog.String("error", err.Error()),
				slog.String("action", e.Action),
				slog.String("event_id", env.ID))
		} else {
			p.published.Add(1)
		}
		cancel()
	}
}

// Close drains pending events up to DrainTimeout, then closes the Producer.
// Returns the first error encountered (drain timeout or Producer.Close error).
//
// Subsequent calls return nil — Close is idempotent.
func (p *Publisher) Close(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}

	close(p.queue)

	// Wait for drain, bounded by both ctx and DrainTimeout.
	drainCtx, cancel := context.WithTimeout(ctx, p.drainTimeout)
	defer cancel()

	select {
	case <-p.drainDone:
		// drain completed cleanly
	case <-drainCtx.Done():
		// timeout — log how many events are still queued; they'll be lost
		stillQueued := len(p.queue)
		p.logger.Error("audit drain timed out",
			slog.Int("events_lost", stillQueued),
			slog.String("error", drainCtx.Err().Error()))
		// Close the producer anyway; return the timeout error.
		_ = p.producer.Close(ctx)
		return fmt.Errorf("audit drain timeout: %w", drainCtx.Err())
	}

	if err := p.producer.Close(ctx); err != nil {
		return fmt.Errorf("audit producer close: %w", err)
	}
	return nil
}

// Stats is a runtime snapshot of the publisher's counters. Read it from
// monitoring to alert on Dropped or Errored.
type Stats struct {
	Published uint64
	Dropped   uint64
	Errored   uint64
}

// Stats returns a snapshot. Safe to call concurrently with Publish.
func (p *Publisher) Stats() Stats {
	return Stats{
		Published: p.published.Load(),
		Dropped:   p.dropped.Load(),
		Errored:   p.errored.Load(),
	}
}

// ErrPublisherClosed is returned by Producer implementations when called
// after the Publisher has shut down. Producers SHOULD return this for
// post-Close calls; the Publisher won't make any more, but third-party
// producers may.
var ErrPublisherClosed = errors.New("audit: publisher closed")
