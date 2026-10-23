// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"testing"

	"latere.ai/x/topos/session"
)

// A session another writer ended after the runner read it, as the end
// route or the reaper does, takes no running from the runner's claim:
// ended is the last status a session has.
func TestAClaimAfterAnEndAppendsNothing(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.message(ctx, "hello")
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	log := NewLog(f.store, s.ID, s.LastSeq)
	f.send(ctx, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCanceled})
	if err := f.r.running(ctx, log); !errors.Is(err, ErrEnded) {
		t.Fatalf("a claim after an end: %v, want ErrEnded", err)
	}
	if s, err = f.store.Get(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if s.Status != session.StatusEnded || s.StopReason != session.StopCanceled {
		t.Fatalf("the session is %s %s after a claim that followed its end", s.Status, s.StopReason)
	}
}

// A title asked after a turn whose read already holds another writer's
// end of the session appends nothing: the end landed after the runner's
// own last append, between its read of the session and its read of the
// log, and an ended session takes no more events.
func TestATitleThatReadAnEndAppendsNothing(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.message(ctx, "Plan.")
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	log := NewLog(f.store, s.ID, s.LastSeq)
	f.send(ctx, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCanceled})
	read, err := f.store.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := session.NewEvent(session.TypeTitleChanged, session.TitleChanged{By: session.Sender{Subject: session.HarnessSubject, Kind: session.SenderService}, New: "Plans"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.appendTitle(ctx, []session.Event{tc}, read.LastSeq); !errors.Is(err, errTitleEnded) {
		t.Fatalf("a title over a read that holds an end: %v, want errTitleEnded", err)
	}
	if n := f.count(ctx, session.TypeTitleChanged); n != 0 {
		t.Fatalf("%d title changes after the end", n)
	}
}
