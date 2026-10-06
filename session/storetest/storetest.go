// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package storetest is the conformance suite of session.Store (spec
// 004). Every implementation runs it: the in-memory store, the directory
// store, the Postgres store and the client store.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

// Open returns an empty store for one subtest. The suite calls it once
// per subtest; cleanup is the caller's, through t.Cleanup.
type Open func(t *testing.T) session.Store

// Run runs the conformance suite against the stores open returns.
func Run(t *testing.T, open Open) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, session.Store)
	}{
		{"CreateGet", testCreateGet},
		{"CreateRejects", testCreateRejects},
		{"AppendSequences", testAppendSequences},
		{"AppendRejectsStaleSequence", testAppendRejectsStaleSequence},
		{"AppendRetryIsIdempotent", testAppendRetryIsIdempotent},
		{"AppendRejectsBadBatch", testAppendRejectsBadBatch},
		{"AppendMirrorsStatus", testAppendMirrorsStatus},
		{"AppendMirrorsSpend", testAppendMirrorsSpend},
		{"AppendedEventIsNeverRewritten", testNeverRewritten},
		{"EventsWindow", testEventsWindow},
		{"Watch", testWatch},
		{"Deltas", testDeltas},
		{"Blobs", testBlobs},
		{"Redact", testRedact},
		{"Lease", testLease},
		{"Delete", testDelete},
		{"List", testList},
		{"ListFilters", testListFilters},
		{"ListByRoot", testListByRoot},
		{"ListByParent", testListByParent},
		{"ListGroupedByTree", testListGroupedByTree},
		{"Archive", testArchive},
		{"Summary", testSummary},
		{"SearchMatches", testSearchMatches},
		{"SearchBounds", testSearchBounds},
		{"SearchScope", testSearchScope},
		{"UnknownTypeIsKept", testUnknownType},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open(t)) })
	}
}

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// NewSession returns a session the suite creates.
func NewSession() session.Session {
	return session.New(
		session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		session.Sender{Subject: "usr_1", Name: "Ada", Kind: session.SenderPerson},
		session.RunnerExternal,
		session.Machine{Kind: session.MachineHost, Workdir: "/work"},
		t0,
	)
}

// Message returns a user.message event.
func Message(t *testing.T, textBody string, at time.Time) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{
		Sender:  session.Sender{Subject: "usr_1", Name: "Ada", Kind: session.SenderPerson},
		Content: []lux.Block{{Type: ir.BlockText, Text: textBody}},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	e.Turn = 1
	return e
}

// Status returns a session.status event.
func Status(t *testing.T, st session.Status, reason session.StopReason, at time.Time) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: st, StopReason: reason}, at)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func create(t *testing.T, st session.Store) session.Session {
	t.Helper()
	s := NewSession()
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func appendAll(t *testing.T, st session.Store, id string, after uint64, evs ...session.Event) uint64 {
	t.Helper()
	session.Stamp(id, after, evs)
	last, err := st.Append(t.Context(), id, after, evs)
	if err != nil {
		t.Fatalf("Append after %d: %v", after, err)
	}
	return last
}

func testCreateGet(t *testing.T, st session.Store) {
	ctx := t.Context()
	s := NewSession()
	s.Title = "fix the build"
	s.Metadata = map[string]string{"k": "v"}
	s.Attended = true
	if err := st.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := session.Marshal(s)
	b, _ := session.Marshal(got)
	if !bytes.Equal(a, b) {
		t.Fatalf("Get returned\n%s\nwant\n%s", b, a)
	}
	if _, err := st.Get(ctx, session.NewID(session.PrefixSession)); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get of a missing session: %v, want ErrNotFound", err)
	}
}

func testCreateRejects(t *testing.T, st session.Store) {
	ctx := t.Context()
	s := create(t, st)
	if err := st.Create(ctx, s, nil); !errors.Is(err, session.ErrExists) {
		t.Fatalf("second Create: %v, want ErrExists", err)
	}
	bad := NewSession()
	bad.ID = "ses_nope"
	if err := st.Create(ctx, bad, nil); !errors.Is(err, session.ErrBadID) {
		t.Fatalf("Create with a bad id: %v, want ErrBadID", err)
	}
	other := NewSession()
	if err := st.Create(ctx, other, map[session.Digest][]byte{session.DigestOf([]byte("a")): []byte("b")}); !errors.Is(err, session.ErrBlobMismatch) {
		t.Fatalf("Create with a mismatched blob: %v, want ErrBlobMismatch", err)
	}
	manifest := []byte(`{"name":"builder"}`)
	withBlob := NewSession()
	d := session.DigestOf(manifest)
	if err := st.Create(ctx, withBlob, map[session.Digest][]byte{d: manifest}); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Blob(ctx, withBlob.ID, d)
	if err != nil {
		t.Fatalf("Blob created with the session: %v", err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Error(err)
		}
	}()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, manifest) {
		t.Fatalf("blob = %q", got)
	}
}

func testAppendSequences(t *testing.T, st session.Store) {
	s := create(t, st)
	last := appendAll(t, st, s.ID, 0, Message(t, "one", t0), Message(t, "two", t0))
	if last != 2 {
		t.Fatalf("last = %d, want 2", last)
	}
	last = appendAll(t, st, s.ID, 2, Message(t, "three", t0))
	if last != 3 {
		t.Fatalf("last = %d, want 3", last)
	}
	evs, err := st.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.CheckSequence(evs, 1); err != nil || len(evs) != 3 {
		t.Fatalf("events %d, %v", len(evs), err)
	}
	for _, e := range evs {
		if e.SessionID != s.ID {
			t.Fatalf("event %s names session %s", e.ID, e.SessionID)
		}
	}
	got, err := st.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeq != 3 || got.Turn != 1 {
		t.Fatalf("header last_seq %d turn %d, want 3 and 1", got.LastSeq, got.Turn)
	}
}

func testAppendRejectsStaleSequence(t *testing.T, st session.Store) {
	s := create(t, st)
	appendAll(t, st, s.ID, 0, Message(t, "one", t0))
	for _, after := range []uint64{0, 5} {
		evs := []session.Event{Message(t, "late", t0)}
		session.Stamp(s.ID, after, evs)
		if _, err := st.Append(t.Context(), s.ID, after, evs); !errors.Is(err, session.ErrSequenceConflict) {
			t.Fatalf("Append after %d with last 1: %v, want ErrSequenceConflict", after, err)
		}
	}
}

func testAppendRetryIsIdempotent(t *testing.T, st session.Store) {
	s := create(t, st)
	batch := []session.Event{Message(t, "one", t0), Message(t, "two", t0)}
	appendAll(t, st, s.ID, 0, batch...)
	session.Stamp(s.ID, 0, batch)
	last, err := st.Append(t.Context(), s.ID, 0, batch)
	if err != nil || last != 2 {
		t.Fatalf("retry: last %d, %v; want 2 and no error", last, err)
	}
	evs, err := st.Events(t.Context(), s.ID, 1, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("after a retry: %d events, %v; want 2", len(evs), err)
	}
	changed := []session.Event{batch[0], Message(t, "other", t0)}
	changed[1].ID = batch[1].ID
	session.Stamp(s.ID, 0, changed)
	if _, err := st.Append(t.Context(), s.ID, 0, changed); !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("retry with different content: %v, want ErrSequenceConflict", err)
	}
}

func testAppendRejectsBadBatch(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	if _, err := st.Append(ctx, s.ID, 0, nil); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("empty batch: %v, want ErrInvalid", err)
	}
	e := Message(t, "one", t0)
	session.Stamp(s.ID, 0, []session.Event{e})
	e.Seq = 7
	if _, err := st.Append(ctx, s.ID, 0, []session.Event{e}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("wrong seq: %v, want ErrInvalid", err)
	}
	e = Message(t, "one", t0)
	evs := []session.Event{e}
	session.Stamp("ses_other", 0, evs)
	if _, err := st.Append(ctx, s.ID, 0, evs); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("wrong session: %v, want ErrInvalid", err)
	}
	evs = []session.Event{Message(t, "one", t0)}
	session.Stamp(s.ID, 0, evs)
	evs[0].Payload = []byte(`[1]`)
	if _, err := st.Append(ctx, s.ID, 0, evs); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("non-object payload: %v, want ErrInvalid", err)
	}
	if _, err := st.Append(ctx, session.NewID(session.PrefixSession), 0, []session.Event{Message(t, "x", t0)}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session: %v, want ErrNotFound", err)
	}
}

func testAppendMirrorsStatus(t *testing.T, st session.Store) {
	s := create(t, st)
	t1 := t0.Add(time.Minute)
	last := appendAll(t, st, s.ID, 0, Status(t, session.StatusRunning, "", t0), Message(t, "go", t1))
	appendAll(t, st, s.ID, last, Status(t, session.StatusIdle, session.StopEndTurn, t1.Add(time.Second)))
	got, err := st.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.StatusIdle || got.StopReason != session.StopEndTurn {
		t.Fatalf("status %s/%s, want idle/end_turn", got.Status, got.StopReason)
	}
	if !got.UpdatedAt.Equal(t1.Add(time.Second)) {
		t.Fatalf("updated_at %s", got.UpdatedAt)
	}
}

// testAppendMirrorsSpend: a read of a session reports as spent the sum of
// its model.requests' costs, as the budget meter counts them, while it
// runs and with no session.status after them.
func testAppendMirrorsSpend(t *testing.T, st session.Store) {
	s := create(t, st)
	cost := func(c int64, at time.Time) session.Event {
		e, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: "m", CostUSDMicro: &c}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Turn = 1
		return e
	}
	last := appendAll(t, st, s.ID, 0, Status(t, session.StatusRunning, "", t0), cost(4581, t0.Add(time.Second)))
	appendAll(t, st, s.ID, last, cost(4779, t0.Add(2*time.Second)))
	got, err := st.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Budget.SpentCostUSDMicro != 4581+4779 {
		t.Fatalf("spent %d, want %d", got.Budget.SpentCostUSDMicro, 4581+4779)
	}
}

func testNeverRewritten(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	appendAll(t, st, s.ID, 0, Message(t, "one", t0))
	before, err := st.Events(ctx, s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		appendAll(t, st, s.ID, uint64(i+1), Message(t, "more", t0))
	}
	after, err := st.Events(ctx, s.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || !session.SameEvent(before[0], after[0]) {
		t.Fatalf("event 1 changed after later appends")
	}
	a, _ := session.Marshal(before[0])
	b, _ := session.Marshal(after[0])
	if !bytes.Equal(a, b) {
		t.Fatalf("event 1 bytes changed:\n%s\n%s", a, b)
	}
}

func testEventsWindow(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	for i := range 5 {
		appendAll(t, st, s.ID, uint64(i), Message(t, "m", t0))
	}
	for _, c := range []struct {
		from       uint64
		limit      int
		first, len int
	}{{1, 0, 1, 5}, {0, 0, 1, 5}, {3, 0, 3, 3}, {2, 2, 2, 2}, {6, 0, 0, 0}} {
		evs, err := st.Events(ctx, s.ID, c.from, c.limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != c.len || (c.len > 0 && evs[0].Seq != uint64(c.first)) {
			t.Fatalf("Events(%d, %d): %d events", c.from, c.limit, len(evs))
		}
	}
	if _, err := st.Events(ctx, session.NewID(session.PrefixSession), 1, 0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func testWatch(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	appendAll(t, st, s.ID, 0, Message(t, "one", t0), Message(t, "two", t0))
	ch, err := st.Watch(ctx, s.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if e := <-ch; e.Seq != 2 {
		t.Fatalf("replay began at %d, want 2", e.Seq)
	}
	appendAll(t, st, s.ID, 2, Message(t, "three", t0))
	appendAll(t, st, s.ID, 3, Message(t, "four", t0))
	for _, want := range []uint64{3, 4} {
		select {
		case e := <-ch:
			if e.Seq != want {
				t.Fatalf("live event %d, want %d", e.Seq, want)
			}
		case <-ctx.Done():
			t.Fatalf("no live event %d", want)
		}
	}
	cancel()
	for range ch {
	}
	if _, err := st.Watch(t.Context(), session.NewID(session.PrefixSession), 1); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Watch of a missing session: %v", err)
	}
}

// testDeltas holds a store that carries live deltas to its contract: a
// subscriber of a session receives the deltas published for it in order,
// a subscriber of another session receives none, and the channel closes
// when its context ends. A store that carries none has nothing to prove.
func testDeltas(t *testing.T, st session.Store) {
	pub, ok := st.(session.DeltaPublisher)
	sub, ok2 := st.(session.DeltaSubscriber)
	if !ok && !ok2 {
		return
	}
	if !ok || !ok2 {
		t.Fatalf("the store publishes deltas (%v) or subscribes to them (%v), not both", ok, ok2)
	}
	s, other := create(t, st), create(t, st)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ch := sub.SubscribeDeltas(ctx, s.ID)
	otherCtx, otherCancel := context.WithCancel(ctx)
	defer otherCancel()
	elsewhere := sub.SubscribeDeltas(otherCtx, other.ID)
	// A store whose deltas cross processes may take a moment to listen,
	// and a delta published before it does is lost by design, so the
	// first is published until one arrives.
	probe := session.Delta{Turn: 1, Step: 1, Kind: session.DeltaText, Text: "probe"}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	pub.PublishDelta(s.ID, probe)
wait:
	for {
		select {
		case d := <-ch:
			if d != probe {
				t.Fatalf("the probe arrived as %+v", d)
			}
			break wait
		case <-tick.C:
			pub.PublishDelta(s.ID, probe)
		case <-ctx.Done():
			t.Fatal("no delta reached the subscriber")
		}
	}
	want := []session.Delta{
		{Turn: 1, Step: 2, Block: 0, Kind: session.DeltaThinking, Text: "Let me see."},
		{Thread: "evt_thread", Turn: 1, Step: 2, Block: 1, Kind: session.DeltaToolInput, Text: `{"path":`},
		{Turn: 1, Step: 2, Reset: true},
		{Turn: 1, Step: 2, Block: 0, Kind: session.DeltaText, Text: "Hello <there> & \u2028 \"you\"."},
	}
	for _, d := range want {
		pub.PublishDelta(s.ID, d)
	}
	for i := 0; i < len(want); {
		select {
		case d := <-ch:
			if d == probe {
				continue
			}
			if d != want[i] {
				t.Fatalf("delta %d is %+v, want %+v", i, d, want[i])
			}
			i++
		case <-ctx.Done():
			t.Fatalf("delta %d did not arrive", i)
		}
	}
	otherCancel()
	for d := range elsewhere {
		t.Fatalf("a subscriber of another session received %+v", d)
	}
	cancel()
	for range ch {
	}
}

func testBlobs(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	body := []byte(`{"id":"resp_1"}`)
	d, err := st.PutBlob(ctx, s.ID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if d != session.DigestOf(body) || !d.Valid() {
		t.Fatalf("digest %s", d)
	}
	if again, err := st.PutBlob(ctx, s.ID, bytes.NewReader(body)); err != nil || again != d {
		t.Fatalf("second put: %s, %v", again, err)
	}
	rc, err := st.Blob(ctx, s.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("blob %q, %v", got, err)
	}
	if _, err := st.Blob(ctx, s.ID, session.DigestOf([]byte("absent"))); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing blob: %v", err)
	}
	if _, err := st.PutBlob(ctx, session.NewID(session.PrefixSession), strings.NewReader("x")); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("PutBlob to a missing session: %v", err)
	}
}

func testRedact(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	secret, err := st.PutBlob(ctx, s.ID, strings.NewReader("token=abc"))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := st.PutBlob(ctx, s.ID, strings.NewReader("shared"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{
		Model: "m", Outcome: "ok", ResponseBlob: secret, RequestBlob: shared,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", RequestBlob: shared}, t0)
	if err != nil {
		t.Fatal(err)
	}
	req.Turn, req.Step = 2, 3
	last := appendAll(t, st, s.ID, 0, Message(t, "hi", t0), req, other)
	by := session.Sender{Subject: "usr_1", Kind: session.SenderPerson}
	if err := st.Redact(ctx, s.ID, req.ID, by, "leaked token"); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != int(last)+1 {
		t.Fatalf("%d events after redaction, want %d", len(evs), last+1)
	}
	got := evs[1]
	if got.ID != req.ID || got.Seq != 2 || got.Type != session.TypeModelRequest || got.Turn != 2 || got.Step != 3 || !got.Redacted() {
		t.Fatalf("redacted event %+v", got)
	}
	red := evs[len(evs)-1]
	var p session.EventRedacted
	if red.Type != session.TypeEventRedacted || red.Decode(&p) != nil || p.EventID != req.ID || p.Reason != "leaked token" {
		t.Fatalf("event.redacted %+v", red)
	}
	if _, err := st.Blob(ctx, s.ID, secret); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the redacted event's own blob survives: %v", err)
	}
	rc, err := st.Blob(ctx, s.ID, shared)
	if err != nil {
		t.Fatalf("a blob another event names was deleted: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := st.Get(ctx, s.ID)
	if err != nil || h.LastSeq != last+1 {
		t.Fatalf("header last_seq %d, %v", h.LastSeq, err)
	}
	if err := st.Redact(ctx, s.ID, session.NewID(session.PrefixEvent), by, ""); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("redacting a missing event: %v", err)
	}
}

func testLease(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	l, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_a", PID: 1, Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, s.ID, 0, Message(t, "by the holder", t0))
	_, err = st.Acquire(ctx, s.ID, session.Holder{Runner: "run_b"})
	var le *session.LockedError
	if !errors.Is(err, session.ErrLocked) || !errors.As(err, &le) || le.Holder.Runner != "run_a" {
		t.Fatalf("second Acquire: %v, want ErrLocked naming run_a", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Lost():
	default:
		t.Fatal("Lost not closed after Release")
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if err := l.Renew(ctx); err == nil {
		t.Fatal("Renew after Release succeeded")
	}
	l2, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_b"})
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Acquire(ctx, session.NewID(session.PrefixSession), session.Holder{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Acquire of a missing session: %v", err)
	}
}

func testDelete(t *testing.T, st session.Store) {
	s := create(t, st)
	ctx := t.Context()
	appendAll(t, st, s.ID, 0, Message(t, "one", t0))
	l, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("Delete of a leased session: %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := st.Delete(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("second Delete: %v", err)
	}
}

func testList(t *testing.T, st session.Store) {
	ctx := t.Context()
	var ids []string
	for range 5 {
		ids = append(ids, create(t, st).ID)
	}
	appendAll(t, st, ids[1], 0, Status(t, session.StatusRunning, "", t0))
	var got []string
	cursor := ""
	for {
		page, next, err := st.List(ctx, session.ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page {
			got = append(got, s.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != 5 {
		t.Fatalf("listed %d sessions, want 5", len(got))
	}
	for i := range got {
		if got[i] != ids[len(ids)-1-i] {
			t.Fatalf("List order %v, want newest first %v", got, ids)
		}
	}
	running, _, err := st.List(ctx, session.ListOptions{Status: session.StatusRunning})
	if err != nil || len(running) != 1 || running[0].ID != ids[1] {
		t.Fatalf("status filter: %v, %v", running, err)
	}
	none, _, err := st.List(ctx, session.ListOptions{AgentID: "agent_none"})
	if err != nil || len(none) != 0 {
		t.Fatalf("agent filter: %v, %v", none, err)
	}
}

func testListFilters(t *testing.T, st session.Store) {
	// Newest first, the order List answers in, each with its initiator
	// and runner kind, and of one of three agents in turn.
	var all []session.Session
	archiver, archives := st.(session.Archiver)
	agents := []string{session.NewID(session.PrefixAgent), session.NewID(session.PrefixAgent), session.NewID(session.PrefixAgent)}
	for i, c := range []struct{ owner, runner string }{
		{"usr_a", session.RunnerHosted}, {"usr_b", session.RunnerExternal}, {"usr_a", session.RunnerExternal},
		{"usr_c", session.RunnerHosted}, {"usr_a", session.RunnerHosted}, {"usr_b", session.RunnerHosted},
		{"usr_a", session.RunnerHosted}, {"usr_c", session.RunnerExternal},
	} {
		s := NewSession()
		s.Initiator.Subject, s.Runner, s.Agent.ID = c.owner, c.runner, agents[i%len(agents)]
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
		}
		// Every third session is archived where the store archives.
		if archives && i%3 == 1 {
			var err error
			if s, err = archiver.SetArchived(t.Context(), s.ID, &t0); err != nil {
				t.Fatal(err)
			}
		}
		all = append([]session.Session{s}, all...)
	}
	for _, c := range []struct {
		name string
		o    session.ListOptions
	}{
		{"one owner", session.ListOptions{Owners: []string{"usr_a"}}},
		{"two owners", session.ListOptions{Owners: []string{"usr_b", "usr_c"}}},
		{"no such owner", session.ListOptions{Owners: []string{"usr_z"}}},
		{"runner", session.ListOptions{Runner: session.RunnerExternal}},
		{"owner and runner", session.ListOptions{Owners: []string{"usr_a"}, Runner: session.RunnerHosted}},
		{"owners and runner", session.ListOptions{Owners: []string{"usr_a", "usr_b"}, Runner: session.RunnerHosted}},
		{"not archived", session.ListOptions{Archived: session.ArchivedExclude}},
		{"archived", session.ListOptions{Archived: session.ArchivedOnly}},
		{"owner and archived", session.ListOptions{Owners: []string{"usr_a"}, Archived: session.ArchivedOnly}},
		{"one agent", session.ListOptions{Agents: agents[:1]}},
		{"two agents and an owner", session.ListOptions{Agents: agents[1:], Owners: []string{"usr_a"}}},
		{"no such agent", session.ListOptions{Agents: []string{session.NewID(session.PrefixAgent)}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var want []string
			for _, s := range all {
				if (len(c.o.Owners) == 0 || slices.Contains(c.o.Owners, s.Initiator.Subject)) && (c.o.Runner == "" || s.Runner == c.o.Runner) && c.o.Archived.Keeps(s) &&
					(len(c.o.Agents) == 0 || slices.Contains(c.o.Agents, s.Agent.ID)) {
					want = append(want, s.ID)
				}
			}
			for _, limit := range []int{1, 2, 3, 0} {
				o := c.o
				o.Limit = limit
				var got []string
				for pages := 0; ; pages++ {
					if pages > len(all) {
						t.Fatalf("limit %d: the cursor never ends", limit)
					}
					page, next, err := st.List(t.Context(), o)
					if err != nil {
						t.Fatal(err)
					}
					if limit > 0 && len(page) > limit {
						t.Fatalf("limit %d: a page of %d", limit, len(page))
					}
					if len(page) == 0 && o.Cursor != "" {
						t.Fatalf("limit %d: the page before the last named a next page", limit)
					}
					for _, s := range page {
						got = append(got, s.ID)
					}
					if next == "" {
						break
					}
					o.Cursor = next
				}
				if !slices.Equal(got, want) {
					t.Fatalf("limit %d: listed %v, want %v", limit, got, want)
				}
			}
		})
	}
}

// testArchive holds a store that archives to spec 015: archived_at is set
// and cleared without an event, a repeat keeps the first time, and it
// survives the header's rewrite by a later batch.
func testArchive(t *testing.T, st session.Store) {
	a, ok := st.(session.Archiver)
	if !ok {
		t.Skip("the store does not archive")
	}
	s := create(t, st)
	last := appendAll(t, st, s.ID, 0, Status(t, session.StatusEnded, session.StopCompleted, t0))
	got, err := a.SetArchived(t.Context(), s.ID, &t0)
	if err != nil || got.ArchivedAt == nil || !got.ArchivedAt.Equal(t0) {
		t.Fatalf("archive: %v, %v", got.ArchivedAt, err)
	}
	if got, err = a.SetArchived(t.Context(), s.ID, new(t0.Add(time.Hour))); err != nil || !got.ArchivedAt.Equal(t0) {
		t.Fatalf("a second archive moved archived_at to %v (%v); it keeps the first", got.ArchivedAt, err)
	}
	read, err := st.Get(t.Context(), s.ID)
	if err != nil || read.ArchivedAt == nil || !read.ArchivedAt.Equal(t0) || read.LastSeq != last {
		t.Fatalf("read after archive: %+v, %v", read, err)
	}
	evs, err := st.Events(t.Context(), s.ID, 1, 0)
	if err != nil || len(evs) != int(last) {
		t.Fatalf("archiving appended to the log: %d events, %v", len(evs), err)
	}
	if got, err = a.SetArchived(t.Context(), s.ID, nil); err != nil || got.ArchivedAt != nil {
		t.Fatalf("unarchive: %v, %v", got.ArchivedAt, err)
	}
	if read, err = st.Get(t.Context(), s.ID); err != nil || read.ArchivedAt != nil {
		t.Fatalf("read after unarchive: %v, %v", read.ArchivedAt, err)
	}
	// A batch that rewrites the header keeps what archiving set.
	other := create(t, st)
	if _, err := a.SetArchived(t.Context(), other.ID, &t0); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, other.ID, 0, Status(t, session.StatusEnded, session.StopCompleted, t0))
	if read, err = st.Get(t.Context(), other.ID); err != nil || read.ArchivedAt == nil {
		t.Fatalf("archived_at after an append: %v, %v", read.ArchivedAt, err)
	}
	if _, err := a.SetArchived(t.Context(), session.NewID(session.PrefixSession), &t0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("archive of a missing session: %v, want not found", err)
	}
}

// testSummary holds a store that counts to spec 015: each count is of
// the sessions its List filters to, before any paging, by status with
// the idle sessions waiting for a person apart, and Agents is the
// number of distinct agents among them. A status filter does not narrow
// a summary.
func testSummary(t *testing.T, st session.Store) {
	summarizer, ok := st.(session.Summarizer)
	if !ok {
		t.Skip("the store does not count")
	}
	archiver, archives := st.(session.Archiver)
	agents := []session.AgentRef{
		{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		{ID: session.NewID(session.PrefixAgent), Name: "reviewer", Version: 1},
		{ID: session.NewID(session.PrefixAgent), Name: "writer", Version: 2},
	}
	for _, c := range []struct {
		owner, runner string
		agent         int
		status        session.Status
		reason        session.StopReason
		archived      bool
	}{
		{"usr_a", session.RunnerHosted, 0, session.StatusRunning, "", false},
		{"usr_a", session.RunnerHosted, 0, session.StatusIdle, session.StopToolConfirmation, false},
		{"usr_a", session.RunnerExternal, 1, session.StatusIdle, session.StopEndTurn, false},
		{"usr_b", session.RunnerHosted, 1, session.StatusRunning, "", false},
		{"usr_b", session.RunnerHosted, 2, session.StatusEnded, session.StopCompleted, false},
		{"usr_a", session.RunnerHosted, 2, session.StatusEnded, session.StopFailed, true},
		{"usr_c", session.RunnerExternal, 0, session.StatusIdle, session.StopToolConfirmation, false},
		{"usr_a", session.RunnerHosted, 0, session.StatusIdle, "", false},
	} {
		s := NewSession()
		s.Agent, s.Initiator.Subject, s.Runner = agents[c.agent], c.owner, c.runner
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
		}
		if c.status != session.StatusIdle || c.reason != "" {
			appendAll(t, st, s.ID, 0, Status(t, c.status, c.reason, t0))
		}
		if c.archived && archives {
			if _, err := archiver.SetArchived(t.Context(), s.ID, &t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	every, _, err := st.List(t.Context(), session.ListOptions{Limit: 100})
	if err != nil || len(every) != 8 {
		t.Fatalf("list: %d sessions, %v", len(every), err)
	}
	for _, c := range []struct {
		name string
		o    session.ListOptions
	}{
		{"every session", session.ListOptions{}},
		{"one owner", session.ListOptions{Owners: []string{"usr_a"}}},
		{"two owners", session.ListOptions{Owners: []string{"usr_b", "usr_c"}}},
		{"no such owner", session.ListOptions{Owners: []string{"usr_z"}}},
		{"runner", session.ListOptions{Runner: session.RunnerExternal}},
		{"agent", session.ListOptions{AgentID: agents[0].ID}},
		{"no such agent", session.ListOptions{AgentID: "agent_none"}},
		{"not archived", session.ListOptions{Archived: session.ArchivedExclude}},
		{"archived", session.ListOptions{Archived: session.ArchivedOnly}},
		{"owner, runner and not archived", session.ListOptions{Owners: []string{"usr_a"}, Runner: session.RunnerHosted, Archived: session.ArchivedExclude}},
		{"two agents", session.ListOptions{Agents: []string{agents[1].ID, agents[2].ID}}},
		{"an agent and an owner", session.ListOptions{Agents: []string{agents[0].ID}, Owners: []string{"usr_a"}}},
		{"a status, which does not narrow", session.ListOptions{Status: session.StatusRunning, Limit: 1, Cursor: every[0].ID}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var want session.Summary
			seen := map[string]bool{}
			for _, s := range every {
				if (c.o.AgentID != "" && s.Agent.ID != c.o.AgentID) || (len(c.o.Owners) > 0 && !slices.Contains(c.o.Owners, s.Initiator.Subject)) ||
					(len(c.o.Agents) > 0 && !slices.Contains(c.o.Agents, s.Agent.ID)) ||
					(c.o.Runner != "" && s.Runner != c.o.Runner) || !c.o.Archived.Keeps(s) {
					continue
				}
				switch {
				case s.Status == session.StatusRunning:
					want.Sessions.Running++
				case s.Status == session.StatusIdle && s.StopReason == session.StopToolConfirmation:
					want.Sessions.WaitingForApproval++
				case s.Status == session.StatusIdle:
					want.Sessions.Idle++
				case s.Status == session.StatusEnded:
					want.Sessions.Ended++
				}
				seen[s.Agent.ID] = true
			}
			want.Agents = len(seen)
			got, err := summarizer.Summarize(t.Context(), c.o)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("summary %+v, want %+v", got, want)
			}
		})
	}
}

func testUnknownType(t *testing.T, st session.Store) {
	s := create(t, st)
	e, err := session.NewEvent("future.kind", map[string]int{"n": 1}, t0)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, s.ID, 0, Message(t, "hi", t0), e)
	evs, err := st.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Type != "future.kind" {
		t.Fatalf("the unknown event was not kept: %+v", evs)
	}
	tr, err := session.Fold(evs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Unknown) != 1 || tr.Unknown[0] != "future.kind" || !errors.Is(tr.Check(), session.ErrSchemaTooNew) {
		t.Fatalf("fold unknown %v, check %v", tr.Unknown, tr.Check())
	}
	if len(tr.Messages) != 1 {
		t.Fatalf("the unknown event rendered: %d messages", len(tr.Messages))
	}
}

// forest creates sessions in three fork trees and a fork whose tree's
// root was deleted before it was read (spec 056), newest last: a, the
// root of b and c, which b's fork d joins; x alone; y and its fork z;
// and w, a fork of a session no longer in the store. d is of another
// initiator than the rest.
func forest(t *testing.T, st session.Store) map[string]session.Session {
	t.Helper()
	out := map[string]session.Session{}
	gone := session.NewID(session.PrefixSession)
	for _, c := range []struct{ name, parent, root string }{
		{"a", "", ""}, {"x", "", ""}, {"b", "a", "a"}, {"y", "", ""}, {"c", "a", "a"}, {"z", "y", "y"}, {"d", "b", "a"}, {"w", "gone", "gone"},
	} {
		s := NewSession()
		id := func(name string) string {
			if name == "gone" {
				return gone
			}
			return out[name].ID
		}
		if c.parent != "" {
			s.Parent, s.Root = &session.Parent{SessionID: id(c.parent), Seq: 8}, id(c.root)
		}
		if c.name == "d" {
			s.Initiator.Subject = "usr_2"
		}
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
		}
		got, err := st.Get(t.Context(), s.ID)
		if err != nil || got.Root != s.Root || (got.Parent == nil) != (s.Parent == nil) {
			t.Fatalf("%s reads back with root %q, parent %+v, %v", c.name, got.Root, got.Parent, err)
		}
		out[c.name] = s
	}
	return out
}

// listed pages through what o lists at several page sizes and answers
// the names of the sessions, and, grouped by tree, each one's tree.
func listed(t *testing.T, st session.Store, o session.ListOptions, names map[string]session.Session) []string {
	t.Helper()
	byID := map[string]string{}
	for name, s := range names {
		byID[s.ID] = name
	}
	var first []string
	for _, limit := range []int{1, 2, 0} {
		var got []string
		cursor := ""
		for range 20 {
			o.Limit, o.Cursor = limit, cursor
			page, next, err := st.List(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range page {
				item := byID[s.ID]
				if (s.Tree != nil) != (o.Group == session.GroupTree) {
					t.Fatalf("%s carries tree %+v in a list grouped %q", item, s.Tree, o.Group)
				}
				if s.Tree != nil {
					root := byID[s.Tree.Root]
					if root == "" {
						root = "gone"
					}
					item += fmt.Sprintf("(%s %d)", root, s.Tree.Sessions)
				}
				got = append(got, item)
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if first != nil && !slices.Equal(got, first) {
			t.Fatalf("pages of %d list %v, of another size %v", limit, got, first)
		}
		first = got
	}
	if first == nil {
		first = []string{}
	}
	return first
}

func testListByRoot(t *testing.T, st session.Store) {
	f := forest(t, st)
	for _, c := range []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"a tree", session.ListOptions{Root: f["a"].ID}, []string{"d", "c", "b", "a"}},
		{"its owner's sessions of it", session.ListOptions{Root: f["a"].ID, Owners: []string{"usr_1"}}, []string{"c", "b", "a"}},
		{"a fork, which is no root", session.ListOptions{Root: f["b"].ID}, []string{"b"}},
		{"a session alone", session.ListOptions{Root: f["x"].ID}, []string{"x"}},
		{"a deleted root", session.ListOptions{Root: f["w"].Root}, []string{"w"}},
		{"no session", session.ListOptions{Root: session.NewID(session.PrefixSession)}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, st, c.o, f); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
}

func testListByParent(t *testing.T, st session.Store) {
	f := forest(t, st)
	for _, c := range []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"a root's forks", session.ListOptions{Parent: f["a"].ID}, []string{"c", "b"}},
		{"a fork's fork", session.ListOptions{Parent: f["b"].ID}, []string{"d"}},
		{"in a tree", session.ListOptions{Parent: f["b"].ID, Root: f["a"].ID}, []string{"d"}},
		{"within the owners", session.ListOptions{Parent: f["b"].ID, Owners: []string{"usr_1"}}, []string{}},
		{"a session no one forked", session.ListOptions{Parent: f["x"].ID}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, st, c.o, f); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
}

func testListGroupedByTree(t *testing.T, st session.Store) {
	f := forest(t, st)
	for _, c := range []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"every tree by its newest", session.ListOptions{Group: session.GroupTree}, []string{"w(gone 1)", "d(a 4)", "z(y 2)", "x(x 1)"}},
		{"within the owners", session.ListOptions{Group: session.GroupTree, Owners: []string{"usr_1"}}, []string{"w(gone 1)", "z(y 2)", "c(a 3)", "x(x 1)"}},
		{"one tree", session.ListOptions{Group: session.GroupTree, Root: f["a"].ID}, []string{"d(a 4)"}},
		{"a parent's forks", session.ListOptions{Group: session.GroupTree, Parent: f["a"].ID}, []string{"c(a 2)"}},
		{"nothing kept", session.ListOptions{Group: session.GroupTree, Owners: []string{"usr_z"}}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, st, c.o, f); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
}
