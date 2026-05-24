package hooks

import (
	"log/slog"
	"sync"
	"time"
)

// Consumer is a function registered to handle dispatched events. It receives
// the event name and the typed payload.  For decision-bearing events
// (PreToolUse, PermissionRequest), consumers return a Decision; for all
// others the Decision return value is ignored.
type Consumer func(name EventName, payload any) Decision

// LogEntry records a single dispatched event in the session event log.
type LogEntry struct {
	At        time.Time `json:"at"`
	EventName EventName `json:"event"`
	// PayloadJSON is the JSON-marshalled payload (for audit/replay). May be
	// nil if marshalling failed.
	Payload any `json:"payload,omitempty"`
}

// Bus is the hook registry and dispatcher. Consumers are called in
// deterministic registration order on every Dispatch call.
//
// Bus is safe for concurrent use.
type Bus struct {
	mu        sync.RWMutex
	consumers []registeredConsumer
	log       []LogEntry // session-scoped append-only event log
}

type registeredConsumer struct {
	name     string      // logical name for debugging
	events   []EventName // nil = all events
	consumer Consumer
}

// New returns a ready-to-use Bus with no consumers and an empty event log.
func New() *Bus {
	return &Bus{}
}

// Register adds a consumer to the bus. Consumers are called in registration
// order.  events restricts which EventNames trigger the consumer; nil/empty
// means "all events".  name is a debug label (need not be unique).
func (b *Bus) Register(name string, events []EventName, c Consumer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consumers = append(b.consumers, registeredConsumer{
		name:     name,
		events:   events,
		consumer: c,
	})
}

// Dispatch sends the event to all matching consumers in registration order and
// records the dispatch to the session event log.
//
// For decision-bearing events the Decision from each consumer is merged:
// a single Deny from any consumer results in a net Deny; the first non-nil
// modified payload wins (applied before the next consumer sees it).
//
// Every dispatch is logged regardless of whether any consumer processed it.
func (b *Bus) Dispatch(name EventName, payload any) Decision {
	b.mu.Lock()
	b.log = append(b.log, LogEntry{
		At:        time.Now().UTC(),
		EventName: name,
		Payload:   payload,
	})
	consumers := make([]registeredConsumer, len(b.consumers))
	copy(consumers, b.consumers)
	b.mu.Unlock()

	result := Decision{Verdict: VerdictAllow}
	current := payload

	for _, rc := range consumers {
		if !rc.matches(name) {
			continue
		}
		d := rc.consumer(name, current)
		switch d.Verdict {
		case VerdictDeny:
			result = d
			// Short-circuit: any deny is final.
			return result
		case VerdictModify:
			if d.ModifiedPayload != nil {
				current = d.ModifiedPayload
				result = d
			}
		default:
			// VerdictAllow: accumulate but don't override a prior Modify.
			if result.Verdict == VerdictAllow {
				result = d
			}
		}
	}

	return result
}

// EventLog returns a snapshot of the session event log in append order.
// Safe to call concurrently.
func (b *Bus) EventLog() []LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]LogEntry, len(b.log))
	copy(out, b.log)
	return out
}

// LogWith returns a *slog.Logger annotated with the bus's event-log size for
// structured observability. Callers may use this instead of slog.Default().
func (b *Bus) LogWith(logger *slog.Logger) *slog.Logger {
	b.mu.RLock()
	n := len(b.log)
	b.mu.RUnlock()
	return logger.With("hook_events_logged", n)
}

// matches returns true when the consumer applies to the given event name.
func (rc *registeredConsumer) matches(name EventName) bool {
	if len(rc.events) == 0 {
		return true
	}
	for _, e := range rc.events {
		if e == name {
			return true
		}
	}
	return false
}
