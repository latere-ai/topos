// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// readOnly makes path unwritable for the rest of the test. The owner of
// a file can still change its mode, so the cleanup restores it before
// t.TempDir removes the tree.
func readOnly(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, fi.Mode().Perm()); err != nil {
			t.Error(err)
		}
	})
}

func TestOpenFailsWhenSessionsIsAFile(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(data); err == nil {
		t.Fatal("opened a store whose sessions directory is a file")
	}
}

func TestWriteFailuresAreReturned(t *testing.T) {
	ctx := context.Background()
	msg := func(t *testing.T) []session.Event { return []session.Event{storetest.Message(t, "x", t0)} }
	t.Run("create in an unwritable store", func(t *testing.T) {
		st := open(t, t.TempDir())
		readOnly(t, st.root, 0o500)
		if err := st.Create(ctx, storetest.NewSession(), nil); err == nil {
			t.Fatal("created a session in an unwritable directory")
		}
	})
	t.Run("append to an unwritable log", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		readOnly(t, st.eventsPath(s.ID), 0o400)
		evs := msg(t)
		session.Stamp(s.ID, 0, evs)
		if _, err := st.Append(ctx, s.ID, 0, evs); err == nil {
			t.Fatal("appended to a read-only log")
		}
	})
	t.Run("status batch in an unwritable session directory", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		readOnly(t, st.dir(s.ID), 0o500)
		evs := []session.Event{storetest.Status(t, session.StatusRunning, "", t0)}
		session.Stamp(s.ID, 0, evs)
		if _, err := st.Append(ctx, s.ID, 0, evs); err == nil {
			t.Fatal("a status batch whose header cannot be written succeeded")
		}
	})
	t.Run("blob into an unwritable blob directory", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		readOnly(t, blobDir(st.dir(s.ID)), 0o500)
		if _, err := st.PutBlob(ctx, s.ID, strings.NewReader("body")); err == nil {
			t.Fatal("wrote a blob into a read-only directory")
		}
	})
	t.Run("unreadable blob", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		d, err := st.PutBlob(ctx, s.ID, strings.NewReader("body"))
		if err != nil {
			t.Fatal(err)
		}
		readOnly(t, blobPath(st.dir(s.ID), d), 0o000)
		if _, err := st.Blob(ctx, s.ID, d); err == nil || errors.Is(err, session.ErrNotFound) {
			t.Fatalf("an unreadable blob: %v", err)
		}
		if _, err := st.Blob(ctx, s.ID, "sha256:nope"); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("a malformed digest: %v", err)
		}
	})
	t.Run("blob failing to read", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		if _, err := st.PutBlob(ctx, s.ID, io.MultiReader(strings.NewReader("a"), errReader{})); err == nil {
			t.Fatal("stored a blob whose reader failed")
		}
	})
	t.Run("redact in an unwritable session directory", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		appendBatchOf(t, st, s.ID, 0, storetest.Message(t, "secret", t0))
		id := mustEvents(t, st, s.ID)[0].ID
		readOnly(t, st.dir(s.ID), 0o500)
		if err := st.Redact(ctx, s.ID, id, session.Sender{Subject: "u"}, ""); err == nil {
			t.Fatal("redacted into a read-only directory")
		}
	})
	t.Run("delete from an unwritable store", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		readOnly(t, st.root, 0o500)
		if err := st.Delete(ctx, s.ID); err == nil {
			t.Fatal("deleted from a read-only directory")
		}
	})
	t.Run("unopenable lock", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		readOnly(t, st.lockPath(s.ID), 0o000)
		evs := msg(t)
		session.Stamp(s.ID, 0, evs)
		if _, err := st.Append(ctx, s.ID, 0, evs); err == nil {
			t.Fatal("appended without the lock")
		}
		if _, err := st.Acquire(ctx, s.ID, session.Holder{}); err == nil {
			t.Fatal("acquired without the lock")
		}
		if err := st.Delete(ctx, s.ID); err == nil {
			t.Fatal("deleted without the lock")
		}
	})
	t.Run("unreadable log", func(t *testing.T) {
		st := open(t, t.TempDir())
		s := newSession(t, st)
		readOnly(t, st.eventsPath(s.ID), 0o000)
		if _, err := st.Events(ctx, s.ID, 1, 0); err == nil {
			t.Fatal("read an unreadable log")
		}
		if _, err := st.Get(ctx, s.ID); err == nil {
			t.Fatal("read the header over an unreadable log")
		}
		if _, _, err := st.List(ctx, session.ListOptions{}); err == nil {
			t.Fatal("listed over an unreadable log")
		}
		if _, err := st.Acquire(ctx, s.ID, session.Holder{}); err == nil {
			t.Fatal("recovered an unreadable log")
		}
	})
	t.Run("unreadable store", func(t *testing.T) {
		st := open(t, t.TempDir())
		readOnly(t, st.root, 0o000)
		if _, _, err := st.List(ctx, session.ListOptions{}); err == nil {
			t.Fatal("listed an unreadable directory")
		}
		if err := st.exists(storetest.NewSession().ID); err == nil || errors.Is(err, session.ErrNotFound) {
			t.Fatalf("stat through an unreadable directory: %v", err)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestCorruptHeader(t *testing.T) {
	ctx := context.Background()
	st := open(t, t.TempDir())
	s := newSession(t, st)
	if err := os.WriteFile(st.headerPath(s.ID), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, s.ID); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("Get of a corrupt header: %v", err)
	}
	if _, err := st.Acquire(ctx, s.ID, session.Holder{}); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("recovery of a corrupt header: %v", err)
	}

	ahead := newSession(t, st)
	ahead.LastSeq = 9
	b, err := session.Marshal(ahead)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.headerPath(ahead.ID), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Acquire(ctx, ahead.ID, session.Holder{}); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a header ahead of its log: %v", err)
	}
}

func TestListSkipsWhatIsNotASession(t *testing.T) {
	ctx := context.Background()
	st := open(t, t.TempDir())
	s := newSession(t, st)
	for _, name := range []string{"notes.txt", "ses_short"} {
		if err := os.WriteFile(filepath.Join(st.root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(st.root, "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}
	empty := storetest.NewSession()
	if err := os.Mkdir(st.dir(empty.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	list, _, err := st.List(ctx, session.ListOptions{})
	if err != nil || len(list) != 1 || list[0].ID != s.ID {
		t.Fatalf("List: %v, %v", list, err)
	}
}

func TestReadHolderOfAGarbledLock(t *testing.T) {
	st := open(t, t.TempDir())
	s := newSession(t, st)
	if err := os.WriteFile(st.lockPath(s.ID), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(st.lockPath(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if h := readHolder(f); h != (session.Holder{}) {
		t.Fatalf("holder %+v from a garbled record", h)
	}
}

func TestCreateLeavesNoPartialSessionWhenABlobFails(t *testing.T) {
	ctx := context.Background()
	st := open(t, t.TempDir())
	s := storetest.NewSession()
	body := bytes.Repeat([]byte("x"), 10)
	stop := errors.New("stop")
	var failed error
	hook = func(point string) {
		if point == "blob.synced" {
			failed = stop
		}
	}
	t.Cleanup(func() { hook = func(string) {} })
	if err := st.Create(ctx, s, map[session.Digest][]byte{session.DigestOf(body): body}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(failed, stop) {
		t.Fatal("the blob of a new session was not written through writeBlob")
	}
	if _, err := os.Stat(filepath.Join(st.root, "."+s.ID+".creating")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the creation directory is left behind: %v", err)
	}
}

// failSyncAt makes the nth fsync from now fail.
func failSyncAt(t *testing.T, n int) {
	t.Helper()
	calls := 0
	fsync = func(f *os.File) error {
		calls++
		if calls == n {
			return errors.New("sync: input/output error")
		}
		return f.Sync()
	}
	t.Cleanup(func() { fsync = func(f *os.File) error { return f.Sync() } })
}

// TestEveryFailedSyncIsReturned fails each fsync of each write path in
// turn and checks the operation reports it.
func TestEveryFailedSyncIsReturned(t *testing.T) {
	ctx := context.Background()
	body := []byte("manifest")
	ops := map[string]struct {
		syncs int
		run   func(t *testing.T, st *Store, s session.Session) error
	}{
		"create": {11, func(t *testing.T, st *Store, s session.Session) error {
			return st.Create(ctx, storetest.NewSession(), map[session.Digest][]byte{session.DigestOf(body): body})
		}},
		"append with status": {3, func(t *testing.T, st *Store, s session.Session) error {
			evs := []session.Event{storetest.Status(t, session.StatusRunning, "", t0)}
			session.Stamp(s.ID, 0, evs)
			_, err := st.Append(ctx, s.ID, 0, evs)
			return err
		}},
		"put blob": {2, func(t *testing.T, st *Store, s session.Session) error {
			_, err := st.PutBlob(ctx, s.ID, bytes.NewReader(body))
			return err
		}},
		"redact": {3, func(t *testing.T, st *Store, s session.Session) error {
			d, err := st.PutBlob(ctx, s.ID, strings.NewReader("secret"))
			if err != nil {
				return err
			}
			e, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Outcome: "ok", ResponseBlob: d}, t0)
			if err != nil {
				return err
			}
			evs := []session.Event{e}
			session.Stamp(s.ID, 0, evs)
			if _, err := st.Append(ctx, s.ID, 0, evs); err != nil {
				return err
			}
			return errSetup
		}},
		"recover a torn tail": {1, func(t *testing.T, st *Store, s session.Session) error {
			appendRaw(t, st.eventsPath(s.ID), []byte(`{"seq":`))
			_, err := st.Acquire(ctx, s.ID, session.Holder{})
			return err
		}},
		"delete": {1, func(t *testing.T, st *Store, s session.Session) error {
			return st.Delete(ctx, s.ID)
		}},
	}
	for name, op := range ops {
		for n := 1; n <= op.syncs; n++ {
			t.Run(fmt.Sprintf("%s/sync %d", name, n), func(t *testing.T) {
				st := open(t, t.TempDir())
				s := newSession(t, st)
				if name == "redact" {
					if err := op.run(t, st, s); !errors.Is(err, errSetup) {
						t.Fatal(err)
					}
					id := mustEvents(t, st, s.ID)[0].ID
					failSyncAt(t, n)
					if err := st.Redact(ctx, s.ID, id, session.Sender{Subject: "u"}, ""); err == nil {
						t.Fatalf("redact acknowledged over failed sync %d", n)
					}
					return
				}
				failSyncAt(t, n)
				if err := op.run(t, st, s); err == nil {
					t.Fatalf("%s acknowledged over failed sync %d", name, n)
				}
			})
		}
	}
}

var errSetup = errors.New("setup done")

func TestRecoveryFailsWhenTheLogCannotBeTruncated(t *testing.T) {
	st := open(t, t.TempDir())
	s := newSession(t, st)
	appendRaw(t, st.eventsPath(s.ID), []byte(`{"seq":`))
	readOnly(t, st.eventsPath(s.ID), 0o400)
	if _, err := st.Acquire(context.Background(), s.ID, session.Holder{}); err == nil {
		t.Fatal("recovered a torn log it could not truncate")
	}
}

func TestTailAfterOverATornTailAcrossChunks(t *testing.T) {
	st := open(t, t.TempDir())
	s := newSession(t, st)
	big := strings.Repeat("y", 50<<10)
	var last uint64
	for range 4 {
		last = appendBatchOf(t, st, s.ID, last, storetest.Message(t, big, t0))
	}
	appendRaw(t, st.eventsPath(s.ID), append([]byte(`{"seq":99,"payload":"`), bytes.Repeat([]byte("z"), 70<<10)...))
	tail, err := tailAfter(st.eventsPath(s.ID), 2)
	if err != nil || len(tail) != 2 || tail[0].Seq != 3 {
		t.Fatalf("tailAfter over a torn tail: %d events, %v", len(tail), err)
	}
	h, err := st.Get(context.Background(), s.ID)
	if err != nil || h.LastSeq != last {
		t.Fatalf("Get: %d, %v", h.LastSeq, err)
	}
}

func TestABatchMarkerThatMiscountsIsCorrupt(t *testing.T) {
	st := open(t, t.TempDir())
	s := newSession(t, st)
	evs := []session.Event{storetest.Message(t, "a", t0)}
	session.Stamp(s.ID, 0, evs)
	b, err := session.Marshal(line{Event: evs[0], Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, st.eventsPath(s.ID), append(b, '\n'))
	if _, err := st.Events(context.Background(), s.ID, 1, 0); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a batch of 2 closing 1 line: %v", err)
	}
}

func TestWatchClosesWhenTheLogBecomesUnreadable(t *testing.T) {
	st := open(t, t.TempDir())
	s := newSession(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readOnly(t, st.eventsPath(s.ID), 0o000)
	ch, err := st.Watch(ctx, s.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an event from an unreadable log")
		}
	case <-ctx.Done():
		t.Fatal("the watcher of an unreadable log stayed open")
	}
	if _, err := st.Watch(ctx, "ses_bad", 1); !errors.Is(err, session.ErrBadID) {
		t.Fatalf("Watch of a bad id: %v", err)
	}
}
