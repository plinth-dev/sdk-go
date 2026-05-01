package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── UUIDv7 ───────────────────────────────────────────────────────────

func TestUUIDV7_Format(t *testing.T) {
	id := uuidV7()
	// 8-4-4-4-12 hex format
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID = %q; expected 5 hyphen-separated parts", id)
	}
	wantLengths := []int{8, 4, 4, 4, 12}
	for i, want := range wantLengths {
		if len(parts[i]) != want {
			t.Errorf("part %d len = %d; want %d (%q)", i, len(parts[i]), want, parts[i])
		}
	}
}

func TestUUIDV7_VersionBitsAreSeven(t *testing.T) {
	id := uuidV7()
	parts := strings.Split(id, "-")
	if parts[2][0] != '7' {
		t.Errorf("version nibble = %c; want '7' (uuid: %s)", parts[2][0], id)
	}
}

func TestUUIDV7_IsTimeOrderedRoughly(t *testing.T) {
	a := uuidV7()
	time.Sleep(2 * time.Millisecond)
	b := uuidV7()
	// First 6 bytes encode milliseconds, so b > a lexicographically.
	if a >= b {
		t.Errorf("UUIDv7 should be time-ordered: a=%s b=%s", a, b)
	}
}

func TestUUIDV7_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := uuidV7()
		if seen[id] {
			t.Fatalf("duplicate UUID after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}

// ── CloudEvent envelope ──────────────────────────────────────────────

func TestEnvelope_PopulatesRequiredFields(t *testing.T) {
	e := Event{Action: "items.update", Actor: Actor{ID: "u1"}, Resource: Resource{Kind: "Item", ID: "i1"}}
	env := envelope("items-api", e)

	if env.SpecVersion != "1.0" {
		t.Errorf("specversion = %q; want '1.0'", env.SpecVersion)
	}
	if env.Source != "plinth.run/items-api" {
		t.Errorf("source = %q; want 'plinth.run/items-api'", env.Source)
	}
	if env.Type != "plinth.audit.items.update.v1" {
		t.Errorf("type = %q; want 'plinth.audit.items.update.v1'", env.Type)
	}
	if env.DataContentType != "application/json" {
		t.Errorf("datacontenttype = %q; want 'application/json'", env.DataContentType)
	}
	if env.ID == "" {
		t.Errorf("id should be populated")
	}
	if env.Time.IsZero() {
		t.Errorf("time should be populated")
	}
	if env.Data.Action != "items.update" {
		t.Errorf("data.action mismatch")
	}
}

func TestEnvelope_JSONShape(t *testing.T) {
	e := Event{Action: "items.update", Actor: Actor{ID: "u1"}, Resource: Resource{Kind: "Item", ID: "i1"}}
	env := envelope("svc", e)
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{`"specversion":"1.0"`, `"id":`, `"source":"plinth.run/svc"`, `"type":"plinth.audit.items.update.v1"`, `"datacontenttype":"application/json"`, `"data":`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope JSON missing %q; body=%s", want, body)
		}
	}
}

// ── MemoryProducer ───────────────────────────────────────────────────

func TestMemoryProducer_AccumulatesEvents(t *testing.T) {
	mp := NewMemoryProducer()
	for i := 0; i < 5; i++ {
		mp.Publish(context.Background(), envelope("svc", Event{Action: "x"}))
	}
	if mp.Len() != 5 {
		t.Errorf("Len = %d; want 5", mp.Len())
	}
	if got := len(mp.Events()); got != 5 {
		t.Errorf("Events() len = %d; want 5", got)
	}
}

func TestMemoryProducer_Reset(t *testing.T) {
	mp := NewMemoryProducer()
	mp.Publish(context.Background(), envelope("svc", Event{Action: "x"}))
	mp.Reset()
	if mp.Len() != 0 {
		t.Errorf("Len after Reset = %d; want 0", mp.Len())
	}
}

func TestMemoryProducer_ConcurrentPublish(t *testing.T) {
	mp := NewMemoryProducer()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				mp.Publish(context.Background(), envelope("svc", Event{Action: "x"}))
			}
		}()
	}
	wg.Wait()
	if mp.Len() != 1000 {
		t.Errorf("Len = %d; want 1000 events", mp.Len())
	}
}

// ── Publisher: happy path ────────────────────────────────────────────

func TestPublisher_BasicPublishDrains(t *testing.T) {
	mp := NewMemoryProducer()
	pub := New(Options{Producer: mp, ServiceName: "test"})

	pub.Publish(context.Background(), Event{Action: "items.create", Outcome: OutcomeSuccess, Severity: SeverityInfo})
	pub.Publish(context.Background(), Event{Action: "items.update", Outcome: OutcomeSuccess, Severity: SeverityInfo})

	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if mp.Len() != 2 {
		t.Errorf("memory producer received %d; want 2", mp.Len())
	}
	if got := pub.Stats().Published; got != 2 {
		t.Errorf("Stats.Published = %d; want 2", got)
	}
}

func TestPublisher_TraceIDPopulation(t *testing.T) {
	mp := NewMemoryProducer()
	pub := New(Options{
		Producer:    mp,
		ServiceName: "test",
		TraceIDFunc: func(ctx context.Context) string { return "fake-trace-123" },
	})
	pub.Publish(context.Background(), Event{Action: "x", Outcome: OutcomeSuccess})
	_ = pub.Close(context.Background())

	events := mp.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	if events[0].Data.TraceID != "fake-trace-123" {
		t.Errorf("TraceID = %q; want fake-trace-123", events[0].Data.TraceID)
	}
}

func TestPublisher_TraceIDPreservesExisting(t *testing.T) {
	mp := NewMemoryProducer()
	pub := New(Options{
		Producer:    mp,
		TraceIDFunc: func(ctx context.Context) string { return "would-overwrite" },
	})
	pub.Publish(context.Background(), Event{Action: "x", Outcome: OutcomeSuccess, TraceID: "existing"})
	_ = pub.Close(context.Background())

	events := mp.Events()
	if events[0].Data.TraceID != "existing" {
		t.Errorf("TraceID = %q; existing should not be overwritten", events[0].Data.TraceID)
	}
}

// ── Publisher: non-blocking + drop-oldest ────────────────────────────

// blockingProducer holds Publish until releaseAll is called. Lets tests
// force the buffer to fill while drain is paused inside the producer.
type blockingProducer struct {
	mp      *MemoryProducer
	release chan struct{}
}

func newBlockingProducer() *blockingProducer {
	return &blockingProducer{
		mp:      NewMemoryProducer(),
		release: make(chan struct{}),
	}
}

func (b *blockingProducer) Publish(ctx context.Context, env CloudEvent) error {
	// Block until releaseAll closes the channel, then return zero-value.
	// Once closed, this read becomes non-blocking for all future calls.
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.mp.Publish(ctx, env)
}

func (b *blockingProducer) Close(ctx context.Context) error {
	return b.mp.Close(ctx)
}

// releaseAll unblocks every pending and future Publish call.
func (b *blockingProducer) releaseAll() {
	close(b.release)
}

func TestPublisher_PublishIsNonBlocking(t *testing.T) {
	bp := newBlockingProducer()
	pub := New(Options{Producer: bp, BufferSize: 4, PublishTimeout: 10 * time.Millisecond})
	defer func() {
		bp.releaseAll()
		_ = pub.Close(context.Background())
	}()

	// Holding all publishes — buffer will fill with 4 events, then drops.
	// Verify we never block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			pub.Publish(context.Background(), Event{Action: "x", Outcome: OutcomeSuccess})
		}
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Publish blocked when buffer was full — should have dropped instead")
	}

	// Some events should have been dropped (buffer is 4, we sent 20).
	if pub.Stats().Dropped == 0 {
		t.Errorf("Stats.Dropped = 0; want > 0 after overflow")
	}
}

func TestPublisher_PublishAfterCloseIsDropped(t *testing.T) {
	mp := NewMemoryProducer()
	pub := New(Options{Producer: mp})
	_ = pub.Close(context.Background())

	for i := 0; i < 10; i++ {
		pub.Publish(context.Background(), Event{Action: "after-close"})
	}
	if mp.Len() != 0 {
		t.Errorf("memory producer received %d; want 0 after Close", mp.Len())
	}
	if got := pub.Stats().Dropped; got != 10 {
		t.Errorf("Stats.Dropped = %d; want 10", got)
	}
}

func TestPublisher_CloseIsIdempotent(t *testing.T) {
	mp := NewMemoryProducer()
	pub := New(Options{Producer: mp})

	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := pub.Close(context.Background()); err != nil {
		t.Errorf("second Close should return nil; got %v", err)
	}
}

// ── Producer error handling ──────────────────────────────────────────

type erroringProducer struct {
	err error
}

func (e *erroringProducer) Publish(context.Context, CloudEvent) error { return e.err }
func (e *erroringProducer) Close(context.Context) error              { return nil }

func TestPublisher_ErrorIncrementsCounter(t *testing.T) {
	pub := New(Options{Producer: &erroringProducer{err: errors.New("boom")}})

	pub.Publish(context.Background(), Event{Action: "x"})
	pub.Publish(context.Background(), Event{Action: "y"})
	_ = pub.Close(context.Background())

	if got := pub.Stats().Errored; got != 2 {
		t.Errorf("Stats.Errored = %d; want 2", got)
	}
	if got := pub.Stats().Published; got != 0 {
		t.Errorf("Stats.Published = %d; want 0 (all errored)", got)
	}
}

// ── Drain timeout ────────────────────────────────────────────────────

type slowProducer struct {
	delay time.Duration
	mp    *MemoryProducer
}

func (s *slowProducer) Publish(ctx context.Context, env CloudEvent) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
	}
	return s.mp.Publish(ctx, env)
}

func (s *slowProducer) Close(ctx context.Context) error { return s.mp.Close(ctx) }

func TestPublisher_DrainTimeoutReturnsError(t *testing.T) {
	mp := NewMemoryProducer()
	sp := &slowProducer{delay: 200 * time.Millisecond, mp: mp}
	pub := New(Options{
		Producer:       sp,
		BufferSize:     32,
		DrainTimeout:   50 * time.Millisecond,
		PublishTimeout: 10 * time.Millisecond,
	})

	for i := 0; i < 16; i++ {
		pub.Publish(context.Background(), Event{Action: "x"})
	}

	err := pub.Close(context.Background())
	if err == nil {
		t.Errorf("expected drain timeout error; got nil")
	}
}

// ── New() validation ────────────────────────────────────────────────

func TestNew_PanicsWithoutProducer(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("New(opts{}) should panic with nil Producer")
		}
	}()
	_ = New(Options{})
}

// ── End-to-end: realistic pattern ────────────────────────────────────

func TestEndToEnd_RealisticUsage(t *testing.T) {
	mp := NewMemoryProducer()
	pub := New(Options{
		Producer:    mp,
		ServiceName: "items-api",
	})

	// Simulate a handler emitting denied + success patterns.
	for i := 0; i < 50; i++ {
		pub.Publish(context.Background(), Event{
			Actor:    Actor{ID: "u1", Type: "user", Roles: []string{"reader"}},
			Action:   "items.read",
			Resource: Resource{Kind: "Item", ID: "i1"},
			Outcome:  OutcomeSuccess,
			Severity: SeverityInfo,
		})
	}
	pub.Publish(context.Background(), Event{
		Actor:    Actor{ID: "u2", Type: "user"},
		Action:   "items.delete",
		Resource: Resource{Kind: "Item", ID: "i2"},
		Outcome:  OutcomeDenied,
		Severity: SeverityWarning,
		Reason:   "policy: not allowed for role 'reader'",
	})

	if err := pub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := mp.Events()
	if len(events) != 51 {
		t.Errorf("got %d events; want 51", len(events))
	}
	// Last event is the denied one — verify Reason flowed through.
	last := events[len(events)-1]
	if last.Data.Outcome != OutcomeDenied {
		t.Errorf("last Outcome = %q; want denied", last.Data.Outcome)
	}
	if !strings.Contains(last.Data.Reason, "policy") {
		t.Errorf("last Reason should be preserved; got %q", last.Data.Reason)
	}
}
