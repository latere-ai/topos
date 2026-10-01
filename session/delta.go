// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// DeltaKind names what a delta's text is part of.
type DeltaKind string

// The kinds of a delta's text: a text block, a thinking block, and the
// arguments of a tool call as the model writes them.
const (
	DeltaText      DeltaKind = "text"
	DeltaThinking  DeltaKind = "thinking"
	DeltaToolInput DeltaKind = "tool_input"
)

// MaxDeltaText is the most text one delta carries, in bytes; a runner
// splits a longer run of fragments into several deltas. JSON escaping
// grows text at most sixfold, so one delta with its session id encodes
// well under the 8000 bytes a Postgres notification carries.
const MaxDeltaText = 1024

// DeltaBuffer is how many deltas a subscriber holds unread; the next one
// is dropped until it reads.
const DeltaBuffer = 256

// Delta is one live frame of a session's response as it arrives (spec
// 005): a run of fragments of one content block, or a reset, which
// discards a step's partial output because its request is sent again.
// Deltas are never appended and never replayed: the agent.message that
// follows is the record, so a client that misses one loses nothing.
type Delta struct {
	// Thread is the thread whose response it is, empty for the session's
	// own thread, as on events.
	Thread string `json:"thread,omitempty"`
	Turn   int    `json:"turn"`
	Step   int    `json:"step"`
	// Block is the index of the response's content block the text
	// belongs to.
	Block int       `json:"block"`
	Kind  DeltaKind `json:"kind"`
	Text  string    `json:"text"`
	// Reset discards what the step's deltas carried so far.
	Reset bool `json:"reset,omitempty"`
}

// MarshalJSON writes a reset as {thread, turn, step, reset} alone, since
// it belongs to no block, and any other delta as {thread, turn, step,
// block, kind, text}.
func (d Delta) MarshalJSON() ([]byte, error) {
	if d.Reset {
		return Marshal(struct {
			Thread string `json:"thread,omitempty"`
			Turn   int    `json:"turn"`
			Step   int    `json:"step"`
			Reset  bool   `json:"reset"`
		}{d.Thread, d.Turn, d.Step, true})
	}
	type plain Delta
	return Marshal(plain(d))
}

// Valid reports whether d is a delta a runner sends: a reset, or text of
// a known kind of at most MaxDeltaText bytes.
func (d Delta) Valid() bool {
	if d.Turn < 1 || d.Step < 1 {
		return false
	}
	if d.Reset {
		return true
	}
	switch d.Kind {
	case DeltaText, DeltaThinking, DeltaToolInput:
	default:
		return false
	}
	return d.Block >= 0 && d.Text != "" && len(d.Text) <= MaxDeltaText
}

// DeltaPublisher is the optional interface of a store that carries a
// session's deltas from the runner that drives it to the streams that
// follow it (spec 016). PublishDelta never blocks the caller and reports
// nothing: a delta it cannot deliver is dropped and counted.
type DeltaPublisher interface {
	PublishDelta(id string, d Delta)
}

// DeltaSubscriber is the optional interface of a store whose streams
// follow a session's deltas, published in this process or, through a
// store several processes share, in any of them. The channel holds
// DeltaBuffer deltas, receives the ones published from the call on, and
// closes when ctx ends.
type DeltaSubscriber interface {
	SubscribeDeltas(ctx context.Context, id string) <-chan Delta
}

// DeltaHub hands deltas to the subscribers of their session in this
// process: the whole carrier of a store one process serves, and the
// local end of one whose deltas cross processes. A subscriber that does
// not keep up loses deltas, never the publisher's time. The zero value
// counts its drops and logs nothing.
type DeltaHub struct {
	// Log receives a debug line for each dropped delta; nil logs none.
	Log *slog.Logger

	mu      sync.Mutex
	subs    map[string]map[chan Delta]struct{}
	dropped atomic.Uint64
}

// PublishDelta hands d to every subscriber of session id, and drops it
// for each one whose buffer is full.
func (h *DeltaHub) PublishDelta(id string, d Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[id] {
		select {
		case ch <- d:
		default:
			h.Drop(id, 1, "a subscriber's buffer is full", nil)
		}
	}
}

// SubscribeDeltas follows the deltas of session id published from now
// on, until ctx ends.
func (h *DeltaHub) SubscribeDeltas(ctx context.Context, id string) <-chan Delta {
	ch := make(chan Delta, DeltaBuffer)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan Delta]struct{}{}
	}
	if h.subs[id] == nil {
		h.subs[id] = map[chan Delta]struct{}{}
	}
	h.subs[id][ch] = struct{}{}
	h.mu.Unlock()
	// A publish sends only under h.mu, so once the channel is out of the
	// map no send reaches it and it can close.
	context.AfterFunc(ctx, func() {
		h.mu.Lock()
		delete(h.subs[id], ch)
		if len(h.subs[id]) == 0 {
			delete(h.subs, id)
		}
		h.mu.Unlock()
		close(ch)
	})
	return ch
}

// Subscribed reports whether any session has a subscriber, so a carrier
// that would decode deltas only to drop them can skip them.
func (h *DeltaHub) Subscribed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// Drop counts n deltas of session id that were not delivered, and logs
// why at debug level.
func (h *DeltaHub) Drop(id string, n int, why string, err error) {
	h.dropped.Add(uint64(n))
	if h.Log != nil {
		h.Log.Debug("dropped live deltas", "session", id, "deltas", n, "reason", why, "err", err)
	}
}

// DroppedDeltas is how many deltas were dropped since the hub was made.
func (h *DeltaHub) DroppedDeltas() uint64 { return h.dropped.Load() }
