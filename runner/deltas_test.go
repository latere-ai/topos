// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// published records what a forwarder publishes.
type published struct {
	mu     sync.Mutex
	deltas []session.Delta
	ids    []string
}

func (p *published) PublishDelta(id string, d session.Delta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, id)
	p.deltas = append(p.deltas, d)
}

func (p *published) all() []session.Delta {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.deltas)
}

// observed records what reaches the Observer a forwarder passes on to.
type observed struct {
	mu             sync.Mutex
	deltas, resets int
}

func (o *observed) OnDelta(harness.Delta) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deltas++
}

func (o *observed) OnReset(string, int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resets++
}

func fragment(thread string, step int, typ ir.EventType, index int, text string) harness.Delta {
	return harness.Delta{Thread: thread, Turn: 1, Step: step, Event: ir.Event{Type: typ, Index: index, Delta: text}}
}

// TestTheForwarderJoinsFragments: the fragments of a block are joined
// into one delta, published at the end of the block; a fragment of
// another thread is its own delta; the events a client shows nothing of
// are left out; and every fragment still reaches the configured
// Observer.
func TestTheForwarderJoinsFragments(t *testing.T) {
	pub, next := &published{}, &observed{}
	f := newForwarder("ses_a", pub, next)
	f.OnDelta(harness.Delta{Turn: 1, Step: 1, Event: ir.Event{Type: ir.EventMessageStart}})
	for range 100 {
		f.OnDelta(fragment("", 1, ir.EventThinkingDelta, 0, "ab"))
	}
	f.OnDelta(fragment("evt_t", 1, ir.EventTextDelta, 0, "other thread"))
	f.OnDelta(fragment("", 1, ir.EventSignatureDelta, 0, "sig"))
	f.OnDelta(fragment("", 1, ir.EventTextDelta, 1, ""))
	if got := pub.all(); len(got) != 0 {
		t.Fatalf("published before the block ended: %+v", got)
	}
	f.OnDelta(fragment("", 1, ir.EventBlockStop, 0, ""))
	want := []session.Delta{
		{Turn: 1, Step: 1, Block: 0, Kind: session.DeltaThinking, Text: strings.Repeat("ab", 100)},
		{Thread: "evt_t", Turn: 1, Step: 1, Block: 0, Kind: session.DeltaText, Text: "other thread"},
	}
	if got := pub.all(); !slices.Equal(got, want) {
		t.Fatalf("published %+v, want %+v", got, want)
	}
	f.OnDelta(fragment("", 1, ir.EventArgsDelta, 2, `{"path":"a"}`))
	f.OnDelta(fragment("", 1, ir.EventMessageStop, 0, ""))
	if got := pub.all(); len(got) != 3 || got[2] != (session.Delta{Turn: 1, Step: 1, Block: 2, Kind: session.DeltaToolInput, Text: `{"path":"a"}`}) {
		t.Fatalf("the end of the response published %+v", got)
	}
	if next.deltas != 107 || slices.ContainsFunc(pub.ids, func(id string) bool { return id != "ses_a" }) {
		t.Fatalf("the configured Observer saw %d fragments; ids %v", next.deltas, pub.ids)
	}
}

// TestTheForwarderPublishesEachInterval: fragments of a block that has
// not ended go out once DeltaInterval passes.
func TestTheForwarderPublishesEachInterval(t *testing.T) {
	pub := &published{}
	f := newForwarder("ses_a", pub, nil)
	f.OnDelta(fragment("", 1, ir.EventTextDelta, 0, "Hel"))
	f.OnDelta(fragment("", 1, ir.EventTextDelta, 0, "lo"))
	deadline := time.Now().Add(5 * time.Second)
	for len(pub.all()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing was published after the interval")
		}
		time.Sleep(DeltaInterval / 5)
	}
	if got := pub.all(); len(got) != 1 || got[0].Text != "Hello" {
		t.Fatalf("published %+v", got)
	}
	f.OnDelta(fragment("", 1, ir.EventTextDelta, 0, ", world"))
	f.close()
	f.OnDelta(fragment("", 1, ir.EventTextDelta, 0, "after the drive"))
	f.OnReset("", 1, 1)
	time.Sleep(2 * DeltaInterval)
	if got := pub.all(); len(got) != 2 || got[1].Text != ", world" {
		t.Fatalf("after close: %+v", got)
	}
}

// TestTheForwarderCutsLongTextBetweenRunes: text past MaxDeltaText goes
// out at once as several deltas, each at most MaxDeltaText bytes, each
// whole runes, which join back into the text.
func TestTheForwarderCutsLongTextBetweenRunes(t *testing.T) {
	pub := &published{}
	f := newForwarder("ses_a", pub, nil)
	long := strings.Repeat("aé漢🙂", 300)
	f.OnDelta(fragment("", 1, ir.EventTextDelta, 0, long))
	got := pub.all()
	if len(got) < 2 {
		t.Fatalf("%d deltas for %d bytes", len(got), len(long))
	}
	var joined strings.Builder
	for _, d := range got {
		if len(d.Text) > session.MaxDeltaText || !utf8.ValidString(d.Text) || !d.Valid() {
			t.Fatalf("a delta of %d bytes, valid UTF-8 %v", len(d.Text), utf8.ValidString(d.Text))
		}
		joined.WriteString(d.Text)
	}
	if joined.String() != long {
		t.Fatal("the deltas do not join back into the text")
	}
	if n := runeCut("\xff\xff\xff", 2); n != 2 {
		t.Fatalf("text that is not UTF-8 is cut at %d", n)
	}
}

// TestTheForwarderResetsAStep: a reset discards the step's held
// fragments and publishes the reset, keeps another thread's, and reaches
// the configured Observer.
func TestTheForwarderResetsAStep(t *testing.T) {
	pub, next := &published{}, &observed{}
	f := newForwarder("ses_a", pub, next)
	f.OnDelta(fragment("", 2, ir.EventTextDelta, 0, "a draft"))
	f.OnDelta(fragment("evt_t", 2, ir.EventTextDelta, 0, "kept"))
	f.OnReset("", 1, 2)
	f.close()
	want := []session.Delta{
		{Turn: 1, Step: 2, Reset: true},
		{Thread: "evt_t", Turn: 1, Step: 2, Block: 0, Kind: session.DeltaText, Text: "kept"},
	}
	if got := pub.all(); !slices.Equal(got, want) || next.resets != 1 {
		t.Fatalf("published %+v, the Observer saw %d resets", got, next.resets)
	}
}

// plain is a store that carries no deltas.
type plain struct{ session.Store }

// another starts a second session like the fixture's and makes it the
// one the fixture's helpers act on.
func (f *fixture) another() {
	f.t.Helper()
	s := session.New(f.s.Agent, f.s.Initiator, f.s.Runner, f.s.Machine, t0)
	if err := f.store.Create(f.t.Context(), s, nil); err != nil {
		f.t.Fatal(err)
	}
	f.s = s
}

// received takes every delta a subscriber holds now.
func received(ch <-chan session.Delta) []session.Delta {
	var out []session.Delta
	for {
		select {
		case d := <-ch:
			out = append(out, d)
		default:
			return out
		}
	}
}

// TestDeltasAreNotAppended: a drive over a store that carries deltas
// publishes the thinking, the text and the tool input of each step as
// the model streams them, and appends the same events as a drive over a
// store that carries none.
func TestDeltasAreNotAppended(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	run := func(r *Runner) []session.Type {
		t.Helper()
		f.stub.Script(model,
			reply(ir.Block{Type: ir.BlockThinking, Text: "Echo first.", Signature: "sig"}, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "echo", Args: json.RawMessage(`{"text":"a"}`)}}),
			reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
		)
		f.message(ctx, "Echo.")
		if out, err := r.Drive(ctx, f.s.ID); err != nil || out.StopReason != session.StopEndTurn {
			t.Fatalf("drive %+v, %v", out, err)
		}
		evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		var types []session.Type
		for _, e := range evs {
			types = append(types, e.Type)
		}
		return types
	}
	sub := f.store.SubscribeDeltas(ctx, f.s.ID)
	with := run(f.r)
	want := []session.Delta{
		{Turn: 1, Step: 1, Block: 0, Kind: session.DeltaThinking, Text: "Echo first."},
		{Turn: 1, Step: 1, Block: 1, Kind: session.DeltaToolInput, Text: `{"text":"a"}`},
		{Turn: 1, Step: 2, Block: 0, Kind: session.DeltaText, Text: "Done."},
	}
	if got := received(sub); !slices.Equal(got, want) {
		t.Fatalf("the subscriber received %+v, want %+v", got, want)
	}

	f.another()
	o := f.r.o
	o.Store = plain{f.store}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if without := run(r); !slices.Equal(with, without) {
		t.Fatalf("with deltas the log is %v, without %v", with, without)
	}
}

// TestAResetFollowsARetriedRequest: a stream cut short publishes what it
// carried, then a reset for its step, then the deltas of the request
// sent again.
func TestAResetFollowsARetriedRequest(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	r := reply(ir.Block{Type: ir.BlockText, Text: "a draft the cut stream carried"})
	r.Fail = &luxstub.Failure{Cut: true, Times: 1}
	r.Respond = func(_ *ir.Request, resp *ir.Response) {
		resp.Blocks = []ir.Block{{Type: ir.BlockText, Text: "the final answer"}}
	}
	f.stub.Script(model, r)
	f.message(ctx, "Go.")
	sub := f.store.SubscribeDeltas(ctx, f.s.ID)
	if out, err := f.r.Drive(ctx, f.s.ID); err != nil || out.StopReason != session.StopEndTurn {
		t.Fatalf("drive %+v, %v", out, err)
	}
	want := []session.Delta{
		{Turn: 1, Step: 1, Block: 0, Kind: session.DeltaText, Text: "a draft the cut stream carried"},
		{Turn: 1, Step: 1, Reset: true},
		{Turn: 1, Step: 1, Block: 0, Kind: session.DeltaText, Text: "the final answer"},
	}
	if got := received(sub); !slices.Equal(got, want) {
		t.Fatalf("the subscriber received %+v, want %+v", got, want)
	}
}

// TestASlowSubscriberNeverDelaysTheTurn: a subscriber that never reads
// loses the deltas past its buffer, counted, and the turn ends as it
// would with no subscriber at all.
func TestASlowSubscriberNeverDelaysTheTurn(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	long := strings.Repeat("word ", (session.DeltaBuffer+40)*session.MaxDeltaText/5)
	drive := func() {
		t.Helper()
		f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: long}))
		f.message(ctx, "Write at length.")
		done := make(chan error, 1)
		go func() {
			_, err := f.r.Drive(ctx, f.s.ID)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the drive waited on a subscriber")
		}
	}
	drive()
	if n := f.store.DroppedDeltas(); n != 0 {
		t.Fatalf("%d dropped with no subscriber", n)
	}
	ignored := f.store.SubscribeDeltas(ctx, f.s.ID)
	drive()
	if n := f.store.DroppedDeltas(); n < 40 {
		t.Fatalf("%d dropped for a subscriber that never read", n)
	}
	if len(ignored) != session.DeltaBuffer {
		t.Fatalf("the subscriber holds %d deltas, its buffer %d", len(ignored), session.DeltaBuffer)
	}
}
