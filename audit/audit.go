// Package audit emits structured audit events from Plinth backend modules.
//
// The package wraps a pluggable [Producer] (the production transport — typically
// NATS JetStream, plugged in by the caller) with non-blocking [Publisher.Publish]
// semantics so the request path never waits on audit ingestion.
//
// See https://plinth.run/sdk/go/audit/ for the design rationale.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Outcome categorizes the result of an audited action. Audit consumers
// (Wazuh, OpenSearch, etc.) use this to filter and drive alerting.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeDenied  Outcome = "denied"
	OutcomeError   Outcome = "error"
)

// Severity drives downstream alerting and retention policies.
// Wazuh + OpenSearch ILM tier hot/warm/cold by Severity.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityNotice  Severity = "notice"
	SeverityWarning Severity = "warning"
	SeverityAlert   Severity = "alert"
)

// Event is the platform-specific data payload. The [Producer] wraps it in a
// CloudEvents 1.0 envelope at publish time; consumers see the envelope.
//
// Fields with omitempty serialize cleanly when unset, but Reason should be
// populated for OutcomeDenied and OutcomeError — a missing reason logs a
// warning but still publishes.
type Event struct {
	Actor     Actor          `json:"actor"`
	Action    string         `json:"action"`     // "items.update", "approvals.deny"
	Resource  Resource       `json:"resource"`
	Outcome   Outcome        `json:"outcome"`
	Severity  Severity       `json:"severity"`
	DataClass string         `json:"data_class,omitempty"` // "internal" | "confidential" | "regulated"
	Before    map[string]any `json:"before,omitempty"`
	After     map[string]any `json:"after,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"` // populated by Publisher if blank
}

// Actor identifies who performed the action.
type Actor struct {
	ID    string   `json:"id"`
	Type  string   `json:"type"`            // "user" | "service" | "system"
	Roles []string `json:"roles,omitempty"`
}

// Resource identifies the thing acted upon. Kind matches the Cerbos resource kind.
type Resource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ── Producer interface + implementations ─────────────────────────────

// Producer is the audit transport. Implementations:
//
//   - [MemoryProducer]: in-memory, for tests.
//   - NATS producer: ships in a follow-on (requires the NATS Go SDK; not yet
//     bundled to keep audit/v0.1.0 dependency-free). Modules can plug in
//     their own Producer that talks to whatever backend they have.
//
// Producers must be safe for concurrent use by multiple goroutines.
type Producer interface {
	// Publish ingests an envelope. Implementations may block.
	// Returning an error increments the Publisher's Errored counter
	// and logs at slog.Error, but doesn't stop the drain.
	Publish(ctx context.Context, env CloudEvent) error

	// Close releases transport resources. Called by [Publisher.Close]
	// after the buffer has fully drained.
	Close(ctx context.Context) error
}

// MemoryProducer is the test producer. Events accumulates in publish order;
// inspect from tests to assert which events a handler emitted.
//
// Threadsafe; can be inspected from any goroutine. Never errors on Publish.
type MemoryProducer struct {
	mu     sync.Mutex
	events []CloudEvent
}

// NewMemoryProducer returns a fresh MemoryProducer ready to use.
func NewMemoryProducer() *MemoryProducer {
	return &MemoryProducer{}
}

// Publish appends the envelope to Events.
func (m *MemoryProducer) Publish(_ context.Context, env CloudEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, env)
	return nil
}

// Close is a no-op for MemoryProducer.
func (m *MemoryProducer) Close(context.Context) error { return nil }

// Events returns a snapshot copy of accumulated envelopes. Safe to read
// while other goroutines are publishing.
func (m *MemoryProducer) Events() []CloudEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]CloudEvent, len(m.events))
	copy(out, m.events)
	return out
}

// Reset clears the accumulated events. Useful between test cases.
func (m *MemoryProducer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = nil
}

// Len returns the number of accumulated events without copying.
func (m *MemoryProducer) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// ── CloudEvents envelope ─────────────────────────────────────────────

// CloudEvent is the CloudEvents 1.0 envelope wrapping an [Event].
// Field names follow the CloudEvents spec (lowercase, no underscores).
//
// See https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/spec.md.
type CloudEvent struct {
	SpecVersion     string    `json:"specversion"`              // always "1.0"
	ID              string    `json:"id"`                       // UUIDv7
	Source          string    `json:"source"`                   // "plinth.run/<service>"
	Type            string    `json:"type"`                     // "plinth.audit.<action>.v1"
	Time            time.Time `json:"time"`                     // event creation time
	DataContentType string    `json:"datacontenttype"`          // "application/json"
	Data            Event     `json:"data"`                     // the platform-specific payload
}

// envelope wraps an [Event] in a [CloudEvent] using the given service name.
// The event is included by value; later mutations to the original Event
// won't affect the envelope.
func envelope(serviceName string, e Event) CloudEvent {
	return CloudEvent{
		SpecVersion:     "1.0",
		ID:              uuidV7(),
		Source:          "plinth.run/" + serviceName,
		Type:            fmt.Sprintf("plinth.audit.%s.v1", e.Action),
		Time:            time.Now().UTC(),
		DataContentType: "application/json",
		Data:            e,
	}
}

// MarshalJSON ensures the envelope serializes with the CloudEvents-required
// time format (RFC 3339 with timezone). Go's default time encoding does this
// already, but we pin it explicitly for safety against future Go changes.
func (c CloudEvent) MarshalJSON() ([]byte, error) {
	type alias CloudEvent
	return json.Marshal(alias(c))
}

// uuidV7 generates a UUID version 7 (timestamp-prefixed; sortable by time).
//
// Layout per RFC 9562:
//
//	48 bits: Unix milliseconds, big-endian
//	 4 bits: version (0x7)
//	12 bits: random
//	 2 bits: variant (0b10)
//	62 bits: random
//
// Falls back to time-based pseudo-random bytes if crypto/rand fails (very rare).
func uuidV7() string {
	var b [16]byte

	ms := time.Now().UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	if _, err := rand.Read(b[6:]); err != nil {
		// Pathological: entropy source unavailable. Use time-jittered
		// fallback. Audit IDs aren't security-critical (they're for
		// dedup, not auth), so this is acceptable.
		nano := time.Now().UnixNano()
		for i := 6; i < 16; i++ {
			b[i] = byte(nano >> (uint(i-6) * 7))
		}
	}

	// Version 7 in high nibble of byte 6.
	b[6] = (b[6] & 0x0F) | 0x70
	// RFC 4122 variant in high two bits of byte 8.
	b[8] = (b[8] & 0x3F) | 0x80

	return hex.EncodeToString(b[0:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}
