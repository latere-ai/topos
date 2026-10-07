// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// answer is an agent.message of the session's own thread with text.
func answer(t *testing.T, text string, at time.Time) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{
		Message:    lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: text}}},
		StopReason: ir.StopEndTurn,
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	e.Turn = 1
	return e
}

// keptOf is the files.kept of msg, keeping one PNG as d.
func keptOf(t *testing.T, msg session.Event, d session.Digest, at time.Time) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeFilesKept, session.FilesKept{
		Message: msg.ID,
		Files:   []session.KeptFile{{Path: "chart.png", Resolved: "/work/chart.png", Blob: d, MediaType: "image/png", Size: 16, Width: 1200, Height: 800}},
		Skipped: []session.SkippedFile{{Path: "draft.svg", Reason: session.KeptNotAnImage}},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	e.Turn = 1
	return e
}

// TestFilesKeptIsKnownAndRedactable: a build that knows files.kept folds
// a log holding it into the same transcript as the log without it, with
// no unknown type and so no schema_too_new; the type is redactable, its
// tombstone names no blob, and an agent.message's companion is its
// files.kept (spec 055).
func TestFilesKeptIsKnownAndRedactable(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	d := session.DigestOf([]byte("\x89PNG\r\n\x1a\nchart"))
	msg := answer(t, "March dips.\n\n![Monthly sales](chart.png)", at)
	kept := keptOf(t, msg, d, at)
	without := []session.Event{storetest.Message(t, "Chart the sales.", at), msg}
	with := append(slices.Clone(without), kept)
	session.Stamp(session.NewID(session.PrefixSession), 0, with)
	if !session.Known[session.TypeFilesKept] || !session.Redactable(session.TypeFilesKept) {
		t.Fatal("files.kept is unknown or not redactable")
	}
	got, err := session.Fold(with, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Check(); err != nil || len(got.Unknown) != 0 {
		t.Fatalf("a log holding files.kept: %v, unknown %v", err, got.Unknown)
	}
	want, err := session.Fold(with[:2], "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("files.kept rendered into the messages: %d, want %d", len(got.Messages), len(want.Messages))
	}
	if b := kept.Blobs(); len(b) != 1 || b[0] != d {
		t.Fatalf("files.kept names %v", b)
	}
	if c := session.Companions(with[1], with); len(c) != 1 || c[0].ID != kept.ID {
		t.Fatalf("the answer's companions are %v", c)
	}
	if c := session.Companions(with[0], with); len(c) != 0 {
		t.Fatalf("a person's message has companions %v", c)
	}
	tomb, _, err := session.Tombstone(with[2], 3, session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, "", at)
	if err != nil {
		t.Fatal(err)
	}
	if !tomb.Redacted() || len(tomb.Blobs()) != 0 || !session.KeptFor(msg.ID, []session.Event{tomb}) || session.KeptFor(with[0].ID, []session.Event{tomb}) {
		t.Fatalf("the tombstone of files.kept is %s", tomb.Payload)
	}
	if c := session.Companions(with[1], []session.Event{with[0], with[1], tomb}); len(c) != 0 {
		t.Fatalf("a redacted files.kept is still a companion: %v", c)
	}
}

// TestAForkCopiesKeptImages: a fork whose copied log holds a files.kept
// copies the image it names, so the fork shows the picture its copied
// answer showed (spec 055).
func TestAForkCopiesKeptImages(t *testing.T) {
	ctx := t.Context()
	st := session.NewMemoryStore()
	parent := storetest.NewSession()
	parent.ID = session.NewID(session.PrefixSession)
	if err := st.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\nchart")
	d, err := st.PutBlob(ctx, parent.ID, bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	msg := answer(t, "![Monthly sales](chart.png)", at)
	evs := []session.Event{
		storetest.Message(t, "Chart the sales.", at),
		storetest.Status(t, session.StatusRunning, "", at),
		msg,
		keptOf(t, msg, d, at),
		storetest.Status(t, session.StatusIdle, session.StopEndTurn, at),
	}
	for i := range evs {
		evs[i].Turn = 1
	}
	session.Stamp(parent.ID, 0, evs)
	if _, err := st.Append(ctx, parent.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	logged, err := st.Events(ctx, parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	child := storetest.NewSession()
	child.ID = session.NewID(session.PrefixSession)
	if _, err := session.Fork(ctx, st, child, nil, parent, logged); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Blob(ctx, child.ID, d)
	if err != nil {
		t.Fatalf("the fork has no copy of the kept image: %v", err)
	}
	b, err := io.ReadAll(rc)
	if err = errors.Join(err, rc.Close()); err != nil || !bytes.Equal(b, png) {
		t.Fatalf("the fork's copy is %q, %v", b, err)
	}
	copied, err := st.Events(ctx, child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(copied, func(e session.Event) bool { return e.Type == session.TypeFilesKept }); i < 0 || !strings.Contains(string(copied[i].Payload), msg.ID) {
		t.Fatal("the fork's log holds no files.kept naming its copied answer")
	}
}
