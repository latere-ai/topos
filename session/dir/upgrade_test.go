// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// TestADirectoryOfThreadEventsOpensAsSubagentEvents: a data directory
// whose log a release before spec 013's names wrote, a subagent's events
// as thread.started, thread.message and thread.ended in a thread field,
// opens with them as subagent.started, subagent.message and
// subagent.ended in a subagent field. The session's own events keep their
// bytes, the batch the old lines closed still closes them, the fold
// renders each conversation apart, an append after it lands, and a second
// Open reads the log no more.
func TestADirectoryOfThreadEventsOpensAsSubagentEvents(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	st := open(t.Context(), t, data)
	s := newSession(ctx, t, st)
	sub := "evt_01J9Z3Q4W8KX6T0M2V5N7R0002"
	own := `{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R0001","seq":1,"session_id":"` + s.ID + `","type":"user.message","time":"2026-09-27T10:00:01Z","turn":1,"payload":{"sender":{"subject":"usr_ada","name":"Ada","kind":"person"},"content":[{"type":"text","text":"Review then fix."}]}}` + "\n"
	old := own +
		`{"id":"` + sub + `","seq":2,"session_id":"` + s.ID + `","type":"thread.started","time":"2026-09-27T10:00:02Z","turn":1,"thread":"` + sub + `","payload":{"agent":{"name":"reviewer","version":0},"task":"Review the diff.","depth":1}}` + "\n" +
		`{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R0003","seq":3,"session_id":"` + s.ID + `","type":"thread.message","time":"2026-09-27T10:00:03Z","turn":1,"thread":"` + sub + `","payload":{"to":"` + sub + `","from_name":"main","content":[{"type":"text","text":"Check line 9 too."}]}}` + "\n" +
		`{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R0004","seq":4,"session_id":"` + s.ID + `","type":"agent.message","time":"2026-09-27T10:00:04Z","turn":1,"thread":"` + sub + `","payload":{"message":{"role":"assistant","blocks":[{"type":"text","text":"One issue."}]},"stop_reason":"end_turn"}}` + "\n" +
		`{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R0005","seq":5,"session_id":"` + s.ID + `","type":"thread.ended","time":"2026-09-27T10:00:05Z","turn":1,"thread":"` + sub + `","payload":{"reason":"completed","final_text":"One issue."},"batch":5}` + "\n"
	path := st.eventsPath(s.ID)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// A log that says thread only in what a person wrote is left as it is.
	quoted := newSession(ctx, t, st)
	said := `{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R0009","seq":1,"session_id":"` + quoted.ID + `","type":"user.message","time":"2026-09-27T10:00:01Z","turn":1,"payload":{"sender":{"subject":"usr_ada","kind":"person"},"content":[{"type":"text","text":"Name it \"thread\"."}]},"batch":1}` + "\n"
	if err := os.WriteFile(st.eventsPath(quoted.ID), []byte(said), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, "sessions", subagentMarker)); err != nil {
		t.Fatal(err)
	}

	st = open(t.Context(), t, data)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte(own)) {
		t.Fatalf("the session's own event changed:\n%s", b)
	}
	if bytes.Contains(b, []byte(`"thread`)) {
		t.Fatalf("the log still says thread:\n%s", b)
	}
	if kept, err := os.ReadFile(st.eventsPath(quoted.ID)); err != nil || string(kept) != said {
		t.Fatalf("a log that renames nothing changed: %q, %v", kept, err)
	}
	evs, err := st.Events(ctx, s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ      session.Type
		subagent string
	}{
		{session.TypeUserMessage, ""},
		{session.TypeSubagentStarted, sub},
		{session.TypeSubagentMessage, sub},
		{session.TypeAgentMessage, sub},
		{session.TypeSubagentEnded, sub},
	}
	if len(evs) != len(want) {
		t.Fatalf("the renamed log holds %d events, want %d", len(evs), len(want))
	}
	for i, w := range want {
		if evs[i].Type != w.typ || evs[i].Subagent != w.subagent || evs[i].Seq != uint64(i+1) {
			t.Fatalf("event %d is %s %d in %q, want %s in %q", i+1, evs[i].Type, evs[i].Seq, evs[i].Subagent, w.typ, w.subagent)
		}
	}
	mine, err := session.Fold(evs, "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := session.Fold(evs, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine.Unknown)+len(theirs.Unknown) > 0 || len(mine.Messages) != 1 || len(theirs.Messages) < 2 {
		t.Fatalf("the folds after the rename: %d messages and %v unknown, %d messages and %v unknown", len(mine.Messages), mine.Unknown, len(theirs.Messages), theirs.Unknown)
	}
	if last := appendBatchOf(ctx, t, st, s.ID, 5, storetest.Message(t, "next", t0)); last != 6 {
		t.Fatalf("an append after the rename ends at %d", last)
	}

	// A log that says thread again is not read: the marker holds.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(after), `"type":"subagent.ended"`, `"type":"thread.ended"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	open(t.Context(), t, data)
	if again, err := os.ReadFile(path); err != nil || !bytes.Contains(again, []byte(`"thread.ended"`)) {
		t.Fatalf("a second Open read the log again: %v", err)
	}
}

// TestRenameLinesKeepsWhatItCannotRead: a torn tail and a line that does
// not decode are kept as they are, for recover to judge, and a log that
// names nothing renamed is reported unchanged.
func TestRenameLinesKeepsWhatItCannotRead(t *testing.T) {
	torn := `{"id":"evt_1","type":"thread.started","thread":"evt_1"`
	junk := "\x00\x00\n"
	plain := `{"id":"evt_2","seq":1,"type":"user.message","payload":{"text":"\"thread\" in a message"}}` + "\n"
	mistyped := `{"id":"evt_3","seq":"two","type":"thread.ended","thread":"evt_1","payload":{}}` + "\n"
	for name, in := range map[string]string{"torn tail": plain + torn, "undecodable line": junk + plain, "nothing renamed": plain, "not a line of the log": mistyped} {
		out, changed, err := renameLines([]byte(in))
		if err != nil || changed || string(out) != in {
			t.Fatalf("%s: %q, changed %v, %v", name, out, changed, err)
		}
	}
}

// TestRenamingASessionAnotherProcessHoldsFailsTheOpen: a log that needs
// renaming while another open file holds the session's lock is not
// rewritten under it, Open says which session is held, and the next Open
// once it is free renames it. A file and a hidden directory under
// sessions/, and a session without a log, are passed over.
func TestRenamingASessionAnotherProcessHoldsFailsTheOpen(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	st := open(t.Context(), t, data)
	s := newSession(ctx, t, st)
	bare := newSession(ctx, t, st)
	path := st.eventsPath(s.ID)
	line := `{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R0002","seq":1,"session_id":"` + s.ID + `","type":"thread.started","time":"2026-09-27T10:00:02Z","turn":1,"thread":"evt_01J9Z3Q4W8KX6T0M2V5N7R0002","payload":{"agent":{"name":"reviewer","version":0},"task":"t","depth":1},"batch":1}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(st.eventsPath(bare.ID)); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(data, "sessions")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".ses_hidden"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, subagentMarker)); err != nil {
		t.Fatal(err)
	}
	held, err := st.lockFile(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := tryLock(held); err != nil || !ok {
		t.Fatalf("take the lock: %v, %v", ok, err)
	}
	var locked *session.LockedError
	if _, err := Open(t.Context(), data); !errors.As(err, &locked) || !strings.Contains(err.Error(), s.ID) {
		t.Fatalf("Open while the session is held: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != line {
		t.Fatalf("the held log was rewritten: %v", err)
	}
	if err := errors.Join(unlock(held), held.Close()); err != nil {
		t.Fatal(err)
	}
	st = open(t.Context(), t, data)
	evs, err := st.Events(ctx, s.ID, 1, 0)
	if err != nil || len(evs) != 1 || evs[0].Type != session.TypeSubagentStarted || evs[0].Subagent == "" {
		t.Fatalf("the log once free: %+v, %v", evs, err)
	}
}

// TestARenameThatCannotReadASessionFailsTheOpen: a log or a lock that is
// not a file fails the Open that would rename the log, naming what it
// could not read, rather than leaving the log unrenamed.
func TestARenameThatCannotReadASessionFailsTheOpen(t *testing.T) {
	for name, path := range map[string]func(st *Store, id string) string{
		"the log": (*Store).eventsPath,
		"the lock": func(st *Store, id string) string {
			if err := os.WriteFile(st.eventsPath(id), []byte(`{"thread":"evt_1"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return st.lockPath(id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			st := open(t.Context(), t, data)
			s := newSession(t.Context(), t, st)
			p := path(st, s.ID)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(data, "sessions", subagentMarker)); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(t.Context(), data); err == nil {
				t.Fatalf("Open renamed over %s that is a directory", name)
			}
			if _, err := os.Stat(filepath.Join(data, "sessions", subagentMarker)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the marker after a failed rename: %v", err)
			}
		})
	}
}
