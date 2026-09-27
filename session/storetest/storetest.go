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
		{"AppendedEventIsNeverRewritten", testNeverRewritten},
		{"EventsWindow", testEventsWindow},
		{"Watch", testWatch},
		{"Blobs", testBlobs},
		{"Redact", testRedact},
		{"Lease", testLease},
		{"Delete", testDelete},
		{"List", testList},
		{"ListFilters", testListFilters},
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
	// and runner kind.
	var all []session.Session
	for _, c := range []struct{ owner, runner string }{
		{"usr_a", session.RunnerHosted}, {"usr_b", session.RunnerExternal}, {"usr_a", session.RunnerExternal},
		{"usr_c", session.RunnerHosted}, {"usr_a", session.RunnerHosted}, {"usr_b", session.RunnerHosted},
		{"usr_a", session.RunnerHosted}, {"usr_c", session.RunnerExternal},
	} {
		s := NewSession()
		s.Initiator.Subject, s.Runner = c.owner, c.runner
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
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
	} {
		t.Run(c.name, func(t *testing.T) {
			var want []string
			for _, s := range all {
				if (len(c.o.Owners) == 0 || slices.Contains(c.o.Owners, s.Initiator.Subject)) && (c.o.Runner == "" || s.Runner == c.o.Runner) {
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
