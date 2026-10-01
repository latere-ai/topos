// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
)

// DeltaInterval is how long a runner holds a fragment before it
// publishes it: a model that streams many small fragments makes about
// one delta per block per interval, not one per fragment.
const DeltaInterval = 50 * time.Millisecond

// forwarder is a drive's harness Observer over a store that carries
// live deltas (spec 016). It joins the fragments of each content block
// into deltas of at most session.MaxDeltaText bytes and publishes them
// every DeltaInterval, at the end of a block, and at the end of a
// response, so the last of a step's text goes out before the step's
// agent.message is appended. A reset discards the step's held fragments
// and publishes a reset. Every fragment and reset also reaches the
// Observer the harness configuration named.
//
// The harness calls it from inside its stream loop, from every thread
// of the turn at once, so it only appends under its lock; publishing
// never blocks, by the store's contract.
type forwarder struct {
	id   string
	pub  session.DeltaPublisher
	next harness.Observer

	mu      sync.Mutex
	pending []session.Delta
	timer   *time.Timer
	closed  bool
}

func newForwarder(id string, pub session.DeltaPublisher, next harness.Observer) *forwarder {
	return &forwarder{id: id, pub: pub, next: next}
}

// kinds maps the fragments a client renders to their delta kind; a
// signature, a block's header and the response's own events carry
// nothing a client shows.
var kinds = map[ir.EventType]session.DeltaKind{
	ir.EventTextDelta:     session.DeltaText,
	ir.EventThinkingDelta: session.DeltaThinking,
	ir.EventArgsDelta:     session.DeltaToolInput,
}

// OnDelta holds a fragment, or publishes what is held at the end of a
// block or a response.
func (f *forwarder) OnDelta(d harness.Delta) {
	if f.next != nil {
		f.next.OnDelta(d)
	}
	ev := d.Event
	kind, ok := kinds[ev.Type]
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.closed:
		return
	case ev.Type == ir.EventBlockStop || ev.Type == ir.EventMessageStop:
		f.flush()
		return
	case !ok || ev.Delta == "":
		return
	}
	i := slices.IndexFunc(f.pending, func(p session.Delta) bool {
		return p.Thread == d.Thread && p.Turn == d.Turn && p.Step == d.Step && p.Block == ev.Index && p.Kind == kind
	})
	if i < 0 {
		f.pending = append(f.pending, session.Delta{Thread: d.Thread, Turn: d.Turn, Step: d.Step, Block: ev.Index, Kind: kind})
		i = len(f.pending) - 1
	}
	f.pending[i].Text += ev.Delta
	if len(f.pending[i].Text) >= session.MaxDeltaText {
		f.flush()
		return
	}
	if f.timer == nil {
		f.timer = time.AfterFunc(DeltaInterval, f.tick)
	}
}

// OnReset discards the step's held fragments and publishes the reset.
func (f *forwarder) OnReset(thread string, turn, step int) {
	if f.next != nil {
		f.next.OnReset(thread, turn, step)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.pending = slices.DeleteFunc(f.pending, func(p session.Delta) bool {
		return p.Thread == thread && p.Turn == turn && p.Step == step
	})
	f.pub.PublishDelta(f.id, session.Delta{Thread: thread, Turn: turn, Step: step, Reset: true})
}

// tick publishes what the interval held.
func (f *forwarder) tick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flush()
}

// close publishes what is held and stops the forwarder: the drive is
// over, and nothing it streams after this is published.
func (f *forwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flush()
	f.closed = true
}

// flush publishes every held delta in the order its first fragment
// arrived, a long one as several of at most session.MaxDeltaText bytes
// cut between runes; f.mu is held.
func (f *forwarder) flush() {
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	for _, d := range f.pending {
		for text := d.Text; text != ""; {
			n := runeCut(text, session.MaxDeltaText)
			d.Text, text = text[:n], text[n:]
			f.pub.PublishDelta(f.id, d)
		}
	}
	f.pending = f.pending[:0]
}

// runeCut is the length of the longest prefix of s of at most n bytes
// that ends between two runes; text that is not UTF-8 there is cut at n.
func runeCut(s string, n int) int {
	if len(s) <= n {
		return len(s)
	}
	for i := n; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return i
		}
	}
	return n
}
