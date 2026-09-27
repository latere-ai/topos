// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func open(t *testing.T, dataDir string) *Store {
	t.Helper()
	s, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDirStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store { return open(t, t.TempDir()) })
}

func newSession(ctx context.Context, t *testing.T, st session.Store) session.Session {
	t.Helper()
	s := storetest.NewSession()
	if err := st.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	return s
}

func appendBatchOf(ctx context.Context, t *testing.T, st session.Store, id string, after uint64, evs ...session.Event) uint64 {
	t.Helper()
	session.Stamp(id, after, evs)
	last, err := st.Append(ctx, id, after, evs)
	if err != nil {
		t.Fatalf("Append after %d: %v", after, err)
	}
	return last
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func appendRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDirStoreTruncatesTornTail(t *testing.T) {
	ctx := t.Context()
	for name, tail := range map[string]func(t *testing.T, id string) []byte{
		"torn last line": func(t *testing.T, id string) []byte {
			return []byte(`{"id":"evt_01J9Z3Q4W8KX6T0M2V5N7R1B3C","seq":3,"session_id":"` + id + `","ty`)
		},
		"lines of an unclosed batch": func(t *testing.T, id string) []byte {
			evs := []session.Event{storetest.Message(t, "x", t0), storetest.Message(t, "y", t0)}
			session.Stamp(id, 2, evs)
			b, err := encodeBatch(evs)
			if err != nil {
				t.Fatal(err)
			}
			return b[:bytes.IndexByte(b, '\n')+1]
		},
		"undecodable last line": func(t *testing.T, id string) []byte {
			return []byte("\x00\x00\x00\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := open(t, t.TempDir())
			s := newSession(ctx, t, st)
			appendBatchOf(ctx, t, st, s.ID, 0, storetest.Message(t, "one", t0), storetest.Message(t, "two", t0))
			path := st.eventsPath(s.ID)
			acked := fileSize(t, path)
			appendRaw(t, path, tail(t, s.ID))

			if h, err := st.Get(ctx, s.ID); err != nil || h.LastSeq != 2 {
				t.Fatalf("Get over a torn tail: last_seq %d, %v", h.LastSeq, err)
			}
			if evs, err := st.Events(ctx, s.ID, 1, 0); err != nil || len(evs) != 2 {
				t.Fatalf("Events over a torn tail: %d, %v", len(evs), err)
			}
			l, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_a"})
			if err != nil {
				t.Fatal(err)
			}
			if got := fileSize(t, path); got != acked {
				t.Fatalf("after recovery events.jsonl is %d bytes, want the acknowledged %d", got, acked)
			}
			if last := appendBatchOf(ctx, t, st, s.ID, 2, storetest.Message(t, "three", t0)); last != 3 {
				t.Fatalf("append after recovery: last %d", last)
			}
			if err := l.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirStoreRefusesCorruptSequence(t *testing.T) {
	ctx := t.Context()
	st := open(t, t.TempDir())
	s := newSession(ctx, t, st)
	appendBatchOf(ctx, t, st, s.ID, 0, storetest.Message(t, "one", t0))
	gap := []session.Event{storetest.Message(t, "gap", t0)}
	session.Stamp(s.ID, 4, gap)
	b, err := encodeBatch(gap)
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, st.eventsPath(s.ID), b)
	if _, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_a"}); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("Acquire over a gap: %v, want ErrCorrupt", err)
	}
	if _, err := st.Append(ctx, s.ID, 1, []session.Event{storetest.Message(t, "x", t0)}); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("Append over a gap: %v, want ErrCorrupt", err)
	}

	bad := newSession(ctx, t, st)
	appendRaw(t, st.eventsPath(bad.ID), []byte("not json\n"))
	later := []session.Event{storetest.Message(t, "after", t0)}
	session.Stamp(bad.ID, 0, later)
	if b, err = encodeBatch(later); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, st.eventsPath(bad.ID), b)
	if _, err := st.Events(ctx, bad.ID, 1, 0); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("an undecodable line before a closed batch: %v, want ErrCorrupt", err)
	}
}

func TestDirStoreGetReadsTheTailAcrossChunks(t *testing.T) {
	ctx := t.Context()
	st := open(t, t.TempDir())
	s := newSession(ctx, t, st)
	big := strings.Repeat("x", 40<<10)
	var last uint64
	for range 6 {
		last = appendBatchOf(ctx, t, st, s.ID, last, storetest.Message(t, big, t0), storetest.Message(t, big, t0))
	}
	h, err := st.Get(ctx, s.ID)
	if err != nil || h.LastSeq != last {
		t.Fatalf("Get: last_seq %d, want %d, %v", h.LastSeq, last, err)
	}
	tail, err := tailAfter(st.eventsPath(s.ID), 3)
	if err != nil || len(tail) != int(last)-3 || tail[0].Seq != 4 {
		t.Fatalf("tailAfter(3): %d events, %v", len(tail), err)
	}
}

func TestDirStoreWatchSeesAnotherWriter(t *testing.T) {
	PollInterval = 20 * time.Millisecond
	data := t.TempDir()
	reader, writer := open(t, data), open(t, data)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s := newSession(ctx, t, writer)
	ch, err := reader.Watch(ctx, s.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	last := appendBatchOf(ctx, t, writer, s.ID, 0, storetest.Message(t, "one", t0))
	if e := <-ch; e.Seq != 1 {
		t.Fatalf("first event %d", e.Seq)
	}
	appendBatchOf(ctx, t, writer, s.ID, last, storetest.Message(t, "two", t0))
	if e := <-ch; e.Seq != 2 {
		t.Fatalf("second event %d", e.Seq)
	}
	if err := writer.Redact(ctx, s.ID, mustEvents(ctx, t, writer, s.ID)[0].ID, session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, ""); err != nil {
		t.Fatal(err)
	}
	if e := <-ch; e.Seq != 3 || e.Type != session.TypeEventRedacted {
		t.Fatalf("after a redaction rewrote the log the watcher got %d %s", e.Seq, e.Type)
	}
	if err := writer.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
}

func mustEvents(ctx context.Context, t *testing.T, st session.Store, id string) []session.Event {
	t.Helper()
	evs, err := st.Events(ctx, id, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestDirStoreSecondInstanceIsLockedOut(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	a, b := open(t, data), open(t, data)
	s := newSession(ctx, t, a)
	l, err := a.Acquire(ctx, s.ID, session.Holder{Runner: "run_a"})
	if err != nil {
		t.Fatal(err)
	}
	var le *session.LockedError
	if _, err := b.Acquire(ctx, s.ID, session.Holder{Runner: "run_b"}); !errors.As(err, &le) || le.Holder.Runner != "run_a" || le.Holder.PID != os.Getpid() {
		t.Fatalf("second instance Acquire: %v", err)
	}
	if _, err := b.Append(ctx, s.ID, 0, []session.Event{storetest.Message(t, "x", t0)}); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("append by a second instance while leased: %v", err)
	}
	if err := b.Delete(ctx, s.ID); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("delete by a second instance while leased: %v", err)
	}
	appendBatchOf(ctx, t, a, s.ID, 0, storetest.Message(t, "by the holder", t0))
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	appendBatchOf(ctx, t, b, s.ID, 1, storetest.Message(t, "after release", t0))
}

// The helper process: TOPOS_DIR_HELPER names what it does, over the data
// directory and session in TOPOS_DIR_DATA and TOPOS_DIR_SESSION.
func TestMain(m *testing.M) {
	if mode := os.Getenv("TOPOS_DIR_HELPER"); mode != "" {
		os.Exit(helper(mode, os.Getenv("TOPOS_DIR_DATA"), os.Getenv("TOPOS_DIR_SESSION"), os.Getenv("TOPOS_DIR_CRASH")))
	}
	os.Exit(m.Run())
}

func helper(mode, data, id, crash string) int {
	st, err := Open(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := context.Background()
	switch mode {
	case "hold":
		if _, err := st.Acquire(ctx, id, session.Holder{Runner: "run_child"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("locked")
		bufio.NewReader(os.Stdin).ReadString('\n')
		return 0
	case "crash":
		hook = func(point string) {
			if point == crash {
				os.Exit(3)
			}
		}
		h, err := st.Get(ctx, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		d, err := st.PutBlob(ctx, id, strings.NewReader("response body"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		req, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", ResponseBlob: d}, t0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		status, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}, t0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		evs := []session.Event{req, status}
		session.Stamp(id, h.LastSeq, evs)
		if _, err := st.Append(ctx, id, h.LastSeq, evs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	return 2
}

func child(t *testing.T, mode, data, id, crash string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TOPOS_DIR_HELPER="+mode, "TOPOS_DIR_DATA="+data, "TOPOS_DIR_SESSION="+id, "TOPOS_DIR_CRASH="+crash)
	return cmd
}

func TestDirStoreSingleWriterLock(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	st := open(t, data)
	s := newSession(ctx, t, st)
	cmd := child(t, "hold", data, s.ID, "")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close() })
	if got, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || got != "locked\n" {
		t.Fatalf("child: %q, %v", got, err)
	}
	var le *session.LockedError
	_, err = st.Acquire(ctx, s.ID, session.Holder{Runner: "run_parent"})
	if !errors.As(err, &le) || le.Holder.Runner != "run_child" || le.Holder.PID != cmd.Process.Pid {
		t.Fatalf("Acquire while the child holds the lock: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("the killed child exited cleanly")
	}
	l, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_parent"})
	if err != nil {
		t.Fatalf("Acquire after the holder was killed: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestDirStoreCrashAtEachDurabilityPoint stops a child process at each
// point of an append that carries a blob and a session.status, then
// checks that the log holds the whole batch or none of it, that no
// acknowledged event is lost, that a blob precedes the event naming it,
// and that the header mirrors the log.
func TestDirStoreCrashAtEachDurabilityPoint(t *testing.T) {
	ctx := t.Context()
	for _, c := range []struct {
		point string
		batch bool // whether the crashed batch is in the log
	}{
		{"blob.synced", false},
		{"events.written", true},
		{"events.synced", true},
		{"session.written", true},
	} {
		t.Run(c.point, func(t *testing.T) {
			data := t.TempDir()
			st := open(t, data)
			s := newSession(ctx, t, st)
			last := appendBatchOf(ctx, t, st, s.ID, 0, storetest.Status(t, session.StatusRunning, "", t0), storetest.Message(t, "go", t0))
			cmd := child(t, "crash", data, s.ID, c.point)
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 3 {
				t.Fatalf("child did not stop at %s: %v", c.point, err)
			}
			l, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_a"})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Release()
			evs := mustEvents(ctx, t, st, s.ID)
			want := int(last)
			if c.batch {
				want += 2
			}
			if len(evs) != want {
				t.Fatalf("%d events after a crash at %s, want %d", len(evs), c.point, want)
			}
			h, err := st.Get(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := session.StatusRunning
			if c.batch {
				wantStatus = session.StatusIdle
				var p session.ModelRequest
				if err := evs[last].Decode(&p); err != nil {
					t.Fatal(err)
				}
				rc, err := st.Blob(ctx, s.ID, p.ResponseBlob)
				if err != nil {
					t.Fatalf("the blob a durable event names is missing: %v", err)
				}
				rc.Close()
			}
			if h.Status != wantStatus {
				t.Fatalf("header status %s, want %s", h.Status, wantStatus)
			}
		})
	}
}

func TestSessionStatusMirrorsTheLog(t *testing.T) {
	ctx := t.Context()
	st := open(t, t.TempDir())
	s := newSession(ctx, t, st)
	appendBatchOf(ctx, t, st, s.ID, 0, storetest.Status(t, session.StatusRunning, "", t0))
	stale, err := os.ReadFile(st.headerPath(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	appendBatchOf(ctx, t, st, s.ID, 1, storetest.Status(t, session.StatusIdle, session.StopToolConfirmation, t0))
	// a crash between the events fsync and the header rename
	if err := os.WriteFile(st.headerPath(s.ID), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if h, err := st.Get(ctx, s.ID); err != nil || h.Status != session.StatusIdle || h.StopReason != session.StopToolConfirmation {
		t.Fatalf("Get: %s/%s, %v", h.Status, h.StopReason, err)
	}
	l, err := st.Acquire(ctx, s.ID, session.Holder{Runner: "run_a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(st.headerPath(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"stop_reason":"tool_confirmation"`)) {
		t.Fatalf("recovery did not rewrite session.json: %s", b)
	}
}

func TestDirStoreCreateIsAllOrNothing(t *testing.T) {
	data := t.TempDir()
	st := open(t, data)
	leftover := filepath.Join(st.root, ".ses_leftover.creating")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	list, _, err := st.List(t.Context(), session.ListOptions{})
	if err != nil || len(list) != 0 {
		t.Fatalf("a hidden partial directory listed: %v, %v", list, err)
	}
	if _, err := st.Get(t.Context(), "ses_nope"); !errors.Is(err, session.ErrBadID) {
		t.Fatalf("Get of a bad id: %v", err)
	}
	if _, err := st.Events(t.Context(), "../escape", 1, 0); !errors.Is(err, session.ErrBadID) {
		t.Fatalf("a path outside the store: %v", err)
	}
}
