// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package dir is the directory store of spec 004. Each session is a
// directory under the data directory's sessions/:
//
//	sessions/<ses_id>/
//	  session.json        the Session, replaced atomically
//	  events.jsonl        one Event per line, LF terminated, appended
//	  blobs/sha256/<hex>  blob bodies
//	  lock                the single-writer lock
//
// A batch is one write followed by an fsync, and its last line carries
// the batch's event count, so the lines of a batch that was never
// acknowledged are recognized and dropped. Appending, redacting and
// deleting take the lock: the store's own lease when it holds one,
// otherwise the lock for the length of the call. Readers take none.
package dir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/topos/session"
)

// PollInterval is how often a watcher in another process than the
// writer's looks for new events.
var PollInterval = 250 * time.Millisecond

// Store is the directory store.
type Store struct {
	root  string
	host  string
	now   func() time.Time
	blobs session.Blobs

	mu     sync.Mutex
	states map[string]*state
	// deltas carries live deltas to the subscribers in this process. One
	// toposd serves a data directory alone, so its runners and its
	// streams share this store.
	deltas session.DeltaHub
}

// state is this process's view of one session: the mutex that
// serializes its writes, the lease it holds, and the signal its watchers
// wait on.
type state struct {
	mu     sync.Mutex
	lease  *lease
	sigMu  sync.Mutex
	signal chan struct{}
}

func (st *state) wait() <-chan struct{} {
	st.sigMu.Lock()
	defer st.sigMu.Unlock()
	return st.signal
}

func (st *state) notify() {
	st.sigMu.Lock()
	defer st.sigMu.Unlock()
	close(st.signal)
	st.signal = make(chan struct{})
}

// Options configure a directory store.
type Options struct {
	// Blobs keeps the blob bodies outside the session directories (spec
	// 014); nil keeps them under each session's blobs/sha256/.
	Blobs session.Blobs
}

// Open returns the store over dataDir/sessions, creating it, with the
// blob bodies in each session's directory.
func Open(dataDir string) (*Store, error) { return OpenWith(dataDir, Options{}) }

// OpenWith is Open with options.
func OpenWith(dataDir string, o Options) (*Store, error) {
	root := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("dir: create %s: %w", root, err)
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("dir: hostname: %w", err)
	}
	return &Store{root: root, host: host, now: time.Now, blobs: o.Blobs, states: map[string]*state{}}, nil
}

func (s *Store) state(id string) *state {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[id]
	if !ok {
		st = &state{signal: make(chan struct{})}
		s.states[id] = st
	}
	return st
}

func (s *Store) dir(id string) string        { return filepath.Join(s.root, id) }
func (s *Store) headerPath(id string) string { return filepath.Join(s.dir(id), "session.json") }
func (s *Store) eventsPath(id string) string { return filepath.Join(s.dir(id), "events.jsonl") }
func (s *Store) lockPath(id string) string   { return filepath.Join(s.dir(id), "lock") }
func blobDir(sessionDir string) string       { return filepath.Join(sessionDir, "blobs", "sha256") }
func blobPath(sessionDir string, d session.Digest) string {
	return filepath.Join(blobDir(sessionDir), d.Hex())
}

// exists checks id and that the session's directory is there.
func (s *Store) exists(id string) error {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return err
	}
	if _, err := os.Stat(s.headerPath(id)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", session.ErrNotFound, id)
		}
		return fmt.Errorf("dir: stat %s: %w", id, err)
	}
	return nil
}

// Create writes the session into a hidden directory and renames it into
// place, so a crash leaves either the whole session or none of it.
func (s *Store) Create(ctx context.Context, sess session.Session, blobs map[session.Digest][]byte) error {
	if err := session.CheckCreate(sess, blobs); err != nil {
		return err
	}
	final := s.dir(sess.ID)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("%w: %s", session.ErrExists, sess.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dir: stat %s: %w", sess.ID, err)
	}
	tmp := filepath.Join(s.root, "."+sess.ID+".creating")
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("dir: clear %s: %w", tmp, err)
	}
	if err := os.MkdirAll(blobDir(tmp), 0o700); err != nil {
		return fmt.Errorf("dir: create %s: %w", tmp, err)
	}
	hdr, err := session.Marshal(sess)
	if err != nil {
		return fmt.Errorf("dir: encode session: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(tmp, "session.json"), hdr); err != nil {
		return err
	}
	for _, name := range []string{"events.jsonl", "lock"} {
		if err := writeFileAtomic(filepath.Join(tmp, name), nil); err != nil {
			return err
		}
	}
	digests := make([]session.Digest, 0, len(blobs))
	for d := range blobs {
		digests = append(digests, d)
	}
	slices.Sort(digests)
	for _, d := range digests {
		// A body kept outside is durable before the session appears.
		if s.blobs != nil {
			if err := s.blobs.PutBlob(ctx, sess.ID, d, blobs[d]); err != nil {
				return err
			}
			continue
		}
		if err := writeBlob(tmp, d, blobs[d]); err != nil {
			return err
		}
	}
	for _, d := range []string{filepath.Join(tmp, "blobs"), tmp} {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	hook("create.written")
	if err := os.Rename(tmp, final); err != nil {
		if errors.Is(err, fs.ErrExist) || errors.Is(err, errNotEmpty) {
			return fmt.Errorf("%w: %s", session.ErrExists, sess.ID)
		}
		return fmt.Errorf("dir: rename %s into place: %w", sess.ID, err)
	}
	return syncDir(s.root)
}

// Get reads session.json and applies the events appended after the last
// sequence it records, so the header is current between the batches
// that rewrite it.
func (s *Store) Get(ctx context.Context, id string) (session.Session, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return session.Session{}, err
	}
	b, err := os.ReadFile(s.headerPath(id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return session.Session{}, fmt.Errorf("%w: %s", session.ErrNotFound, id)
		}
		return session.Session{}, fmt.Errorf("dir: read session %s: %w", id, err)
	}
	var sess session.Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return session.Session{}, fmt.Errorf("%w: session.json of %s: %w", session.ErrCorrupt, id, err)
	}
	tail, err := tailAfter(s.eventsPath(id), sess.LastSeq)
	if err != nil {
		return session.Session{}, err
	}
	session.ApplyBatch(&sess, tail)
	return sess, nil
}

func (s *Store) List(ctx context.Context, o session.ListOptions) ([]session.Session, string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, "", fmt.Errorf("dir: list sessions: %w", err)
	}
	var all []session.Session
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || session.CheckID(session.PrefixSession, name) != nil {
			continue
		}
		sess, err := s.Get(ctx, name)
		if errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		all = append(all, sess)
	}
	page, next := session.ListPage(all, o)
	return page, next, nil
}

func (s *Store) Append(ctx context.Context, id string, afterSeq uint64, events []session.Event) (uint64, error) {
	if err := s.exists(id); err != nil {
		return 0, err
	}
	last := afterSeq + uint64(len(events))
	st := s.state(id)
	err := s.locked(id, st, func(hdr *session.Session) error {
		retried, err := session.CheckBatch(id, hdr.LastSeq, afterSeq, events, func(from uint64) ([]session.Event, error) {
			log, err := readLog(s.eventsPath(id))
			if err != nil {
				return nil, err
			}
			return window(log.events(), from, 0), nil
		})
		if err != nil || retried {
			return err
		}
		if err := appendBatch(s.eventsPath(id), events); err != nil {
			return err
		}
		session.ApplyBatch(hdr, events)
		// The header is written when the batch changes what a read of the
		// session reports beside its sequence: its status or its spend.
		if slices.ContainsFunc(events, func(e session.Event) bool {
			return e.Type == session.TypeSessionStatus || e.Type == session.TypeModelRequest
		}) {
			if err := s.writeHeader(id, *hdr); err != nil {
				return err
			}
		}
		st.notify()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return last, nil
}

// SetArchived sets or clears the session's archived_at under its lock
// and rewrites session.json; the log is untouched.
func (s *Store) SetArchived(ctx context.Context, id string, at *time.Time) (session.Session, error) {
	if err := s.exists(id); err != nil {
		return session.Session{}, err
	}
	st := s.state(id)
	var out session.Session
	err := s.locked(id, st, func(hdr *session.Session) error {
		if session.Archive(hdr, at) {
			if err := s.writeHeader(id, *hdr); err != nil {
				return err
			}
			st.notify()
		}
		out = *hdr
		return nil
	})
	if err != nil {
		return session.Session{}, err
	}
	return out, nil
}

func (s *Store) writeHeader(id string, hdr session.Session) error {
	b, err := session.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("dir: encode session: %w", err)
	}
	if err := writeFileAtomic(s.headerPath(id), b); err != nil {
		return err
	}
	hook("session.written")
	return nil
}

func (s *Store) Events(ctx context.Context, id string, fromSeq uint64, limit int) ([]session.Event, error) {
	if err := s.exists(id); err != nil {
		return nil, err
	}
	log, err := readLog(s.eventsPath(id))
	if err != nil {
		return nil, err
	}
	return window(log.events(), fromSeq, limit), nil
}

// window returns the events from fromSeq, at most limit of them (no
// bound when limit is 0 or less).
func window(events []session.Event, fromSeq uint64, limit int) []session.Event {
	i, _ := slices.BinarySearchFunc(events, max(fromSeq, 1), func(e session.Event, seq uint64) int {
		switch {
		case e.Seq < seq:
			return -1
		case e.Seq > seq:
			return 1
		}
		return 0
	})
	out := events[i:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return slices.Clone(out)
}

// PublishDelta hands a live delta to this process's subscribers of its
// session; another process sharing the directory sees none, since
// deltas are never written.
func (s *Store) PublishDelta(id string, d session.Delta) { s.deltas.PublishDelta(id, d) }

// SubscribeDeltas follows a session's live deltas published in this
// process until ctx ends.
func (s *Store) SubscribeDeltas(ctx context.Context, id string) <-chan session.Delta {
	return s.deltas.SubscribeDeltas(ctx, id)
}

// DroppedDeltas is how many deltas a subscriber did not take.
func (s *Store) DroppedDeltas() uint64 { return s.deltas.DroppedDeltas() }

// Watch replays the events from fromSeq, then sends each new one. A
// write in this process wakes the watcher at once; another process's is
// seen within PollInterval. The channel closes when ctx ends, the
// session is deleted, or its log cannot be read.
func (s *Store) Watch(ctx context.Context, id string, fromSeq uint64) (<-chan session.Event, error) {
	if err := s.exists(id); err != nil {
		return nil, err
	}
	st := s.state(id)
	out := make(chan session.Event)
	go func() {
		defer close(out)
		t := time.NewTicker(PollInterval)
		defer t.Stop()
		r := &tailReader{path: s.eventsPath(id), next: max(fromSeq, 1)}
		for {
			sig := st.wait()
			evs, err := r.read()
			if err != nil {
				return
			}
			for _, e := range evs {
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-sig:
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// tailReader follows an events file from a committed offset. A rename
// over the file (a redaction) or a shorter file (a recovery) sends it
// back to the start, where it skips what it already returned.
type tailReader struct {
	path string
	next uint64
	off  int64
	fi   os.FileInfo
}

func (r *tailReader) read() (evs []session.Event, err error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer closeInto(&err, f)
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if r.fi != nil && (!os.SameFile(r.fi, fi) || fi.Size() < r.off) {
		r.off = 0
	}
	r.fi = fi
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return nil, err
	}
	log, err := scanLog(f)
	if err != nil {
		return nil, err
	}
	r.off += log.size
	var out []session.Event
	for _, e := range log.events() {
		if e.Seq >= r.next {
			out = append(out, e)
			r.next = e.Seq + 1
		}
	}
	return out, nil
}

func (s *Store) PutBlob(ctx context.Context, id string, r io.Reader) (session.Digest, error) {
	if err := s.exists(id); err != nil {
		return "", err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("dir: read blob: %w", err)
	}
	d := session.DigestOf(b)
	if s.blobs != nil {
		return d, s.blobs.PutBlob(ctx, id, d, b)
	}
	if err := writeBlob(s.dir(id), d, b); err != nil {
		return "", err
	}
	return d, nil
}

// writeBlob makes the blob durable under the session directory: a
// temporary file, fsync, rename, fsync of the blob directory.
func writeBlob(sessionDir string, d session.Digest, b []byte) error {
	path := blobPath(sessionDir, d)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dir: stat blob: %w", err)
	}
	if err := writeFileAtomic(path, b); err != nil {
		return err
	}
	hook("blob.synced")
	return nil
}

func (s *Store) Blob(ctx context.Context, id string, d session.Digest) (io.ReadCloser, error) {
	if err := s.exists(id); err != nil {
		return nil, err
	}
	if !d.Valid() {
		return nil, fmt.Errorf("%w: digest %q", session.ErrInvalid, d)
	}
	if s.blobs != nil {
		b, err := s.blobs.GetBlob(ctx, id, d)
		if err != nil {
			return nil, err
		}
		if session.DigestOf(b) != d {
			return nil, fmt.Errorf("%w: blob %s does not match its digest", session.ErrCorrupt, d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	b, err := os.ReadFile(blobPath(s.dir(id), d))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: blob %s", session.ErrNotFound, d)
		}
		return nil, fmt.Errorf("dir: open blob: %w", err)
	}
	if session.DigestOf(b) != d {
		return nil, fmt.Errorf("%w: blob %s does not match its digest", session.ErrCorrupt, d)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Redact tombstones one event, appends event.redacted, rewrites the log
// atomically and deletes the blobs only that event named.
func (s *Store) Redact(ctx context.Context, id, eventID string, by session.Sender, reason string) error {
	if err := s.exists(id); err != nil {
		return err
	}
	st := s.state(id)
	return s.locked(id, st, func(hdr *session.Session) error {
		log, err := readLog(s.eventsPath(id))
		if err != nil {
			return err
		}
		i := slices.IndexFunc(log.lines, func(l line) bool { return l.ID == eventID })
		if i < 0 {
			return fmt.Errorf("%w: event %s", session.ErrNotFound, eventID)
		}
		if log.lines[i].Redacted() {
			return nil
		}
		orphans := session.OrphanBlobs(log.lines[i].Event, log.events())
		tomb, red, err := session.Tombstone(log.lines[i].Event, hdr.LastSeq, by, reason, s.now())
		if err != nil {
			return err
		}
		lines := slices.Clone(log.lines)
		lines[i].Event = tomb
		lines = append(lines, line{Event: red, Batch: 1})
		if err := rewriteLog(s.eventsPath(id), lines); err != nil {
			return err
		}
		session.ApplyBatch(hdr, []session.Event{red})
		for _, d := range orphans {
			if s.blobs != nil {
				if err := s.blobs.DeleteBlob(ctx, id, d); err != nil {
					return err
				}
				continue
			}
			if err := os.Remove(blobPath(s.dir(id), d)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("dir: delete blob %s: %w", d, err)
			}
		}
		if len(orphans) > 0 && s.blobs == nil {
			if err := syncDir(blobDir(s.dir(id))); err != nil {
				return err
			}
		}
		st.notify()
		return nil
	})
}

func (s *Store) Delete(ctx context.Context, id string) (err error) {
	if err := s.exists(id); err != nil {
		return err
	}
	st := s.state(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lease != nil {
		return &session.LockedError{Holder: st.lease.holder}
	}
	f, err := s.lockFile(id)
	if err != nil {
		return err
	}
	defer closeInto(&err, f)
	ok, err := tryLock(f)
	if err != nil {
		return err
	}
	if !ok {
		return &session.LockedError{Holder: readHolder(f)}
	}
	gone := filepath.Join(s.root, "."+id+"."+session.NewID("")+".deleting")
	if err := os.Rename(s.dir(id), gone); err != nil {
		return errors.Join(fmt.Errorf("dir: delete %s: %w", id, err), unlock(f))
	}
	if err := syncDir(s.root); err != nil {
		return errors.Join(err, unlock(f))
	}
	if err := unlock(f); err != nil {
		return err
	}
	if err := os.RemoveAll(gone); err != nil {
		return fmt.Errorf("dir: remove %s: %w", id, err)
	}
	st.notify()
	// The bodies kept outside go last, so a crash before leaves bodies
	// with no session, which SweepBlobs removes, and never a session
	// without its bodies.
	if s.blobs != nil {
		return s.blobs.DeleteSession(ctx, id)
	}
	return nil
}

// SweepBlobs removes the bodies kept outside whose session is gone, which
// a crash between a session's delete and its bodies' leaves (spec 014),
// judging the grace of session.SweepBlobs on now.
func (s *Store) SweepBlobs(ctx context.Context, now time.Time) error {
	if s.blobs == nil {
		return nil
	}
	return session.SweepBlobs(ctx, s.blobs, func(_ context.Context, id string) (bool, error) {
		err := s.exists(id)
		if errors.Is(err, session.ErrNotFound) {
			return true, nil
		}
		return false, err
	}, now)
}

func (s *Store) lockFile(id string) (*os.File, error) {
	f, err := os.OpenFile(s.lockPath(id), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dir: open lock: %w", err)
	}
	return f, nil
}

// readHolder reads the diagnostic holder record of a lock file. The
// operating-system lock is the truth; a record that does not decode
// names no holder.
func readHolder(f *os.File) session.Holder {
	var h session.Holder
	b, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<16))
	if err != nil || json.Unmarshal(bytes.TrimSpace(b), &h) != nil {
		return session.Holder{}
	}
	return h
}

// locked runs fn with the session's header under the lock: this
// process's lease when it holds one, otherwise the lock taken for the
// call, with the log recovered first.
func (s *Store) locked(id string, st *state, fn func(hdr *session.Session) error) (err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lease != nil {
		return fn(&st.lease.hdr)
	}
	f, err := s.lockFile(id)
	if err != nil {
		return err
	}
	defer closeInto(&err, f)
	ok, err := tryLock(f)
	if err != nil {
		return err
	}
	if !ok {
		return &session.LockedError{Holder: readHolder(f)}
	}
	hdr, err := s.recover(id)
	if err == nil {
		err = fn(&hdr)
	}
	return errors.Join(err, unlock(f))
}

// recover brings a session's files to a consistent state with the lock
// held: it truncates lines of an unacknowledged batch and a torn last
// line, refuses a log with a gap or a repeat, and rewrites session.json
// when it disagrees with the log's last session.status.
func (s *Store) recover(id string) (session.Session, error) {
	path := s.eventsPath(id)
	log, err := readLog(path)
	if err != nil {
		return session.Session{}, err
	}
	if log.size < log.total {
		if err := truncate(path, log.size); err != nil {
			return session.Session{}, err
		}
	}
	events := log.events()
	if err := session.CheckSequence(events, 1); err != nil {
		return session.Session{}, err
	}
	b, err := os.ReadFile(s.headerPath(id))
	if err != nil {
		return session.Session{}, fmt.Errorf("dir: read session %s: %w", id, err)
	}
	var hdr session.Session
	if err := json.Unmarshal(b, &hdr); err != nil {
		return session.Session{}, fmt.Errorf("%w: session.json of %s: %w", session.ErrCorrupt, id, err)
	}
	if hdr.LastSeq > uint64(len(events)) {
		return session.Session{}, fmt.Errorf("%w: session.json of %s records seq %d, the log ends at %d", session.ErrCorrupt, id, hdr.LastSeq, len(events))
	}
	status, reason := hdr.Status, hdr.StopReason
	session.ApplyBatch(&hdr, events[hdr.LastSeq:])
	if hdr.Status != status || hdr.StopReason != reason {
		if err := s.writeHeader(id, hdr); err != nil {
			return session.Session{}, err
		}
	}
	return hdr, nil
}

func truncate(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("dir: open events: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		return errors.Join(fmt.Errorf("dir: truncate events: %w", err), f.Close())
	}
	if err := fsync(f); err != nil {
		return errors.Join(fmt.Errorf("dir: sync events: %w", err), f.Close())
	}
	return f.Close()
}

// Acquire takes the session's lock for this process until Release, and
// recovers the log under it.
func (s *Store) Acquire(ctx context.Context, id string, holder session.Holder) (session.Lease, error) {
	if err := s.exists(id); err != nil {
		return nil, err
	}
	st := s.state(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lease != nil {
		return nil, &session.LockedError{Holder: st.lease.holder}
	}
	f, err := s.lockFile(id)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !ok {
		h := readHolder(f)
		return nil, errors.Join(&session.LockedError{Holder: h}, f.Close())
	}
	if holder.PID == 0 {
		holder.PID = os.Getpid()
	}
	if holder.Host == "" {
		holder.Host = s.host
	}
	if holder.AcquiredAt.IsZero() {
		holder.AcquiredAt = s.now().UTC()
	}
	fail := func(err error) (session.Lease, error) {
		return nil, errors.Join(err, unlock(f), f.Close())
	}
	b, err := session.Marshal(holder)
	if err != nil {
		return fail(fmt.Errorf("dir: encode holder: %w", err))
	}
	if err := f.Truncate(0); err != nil {
		return fail(fmt.Errorf("dir: write holder: %w", err))
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		return fail(fmt.Errorf("dir: write holder: %w", err))
	}
	hdr, err := s.recover(id)
	if err != nil {
		return fail(err)
	}
	l := &lease{st: st, f: f, holder: holder, hdr: hdr, lost: make(chan struct{})}
	st.lease = l
	return l, nil
}

// lease is a held lock. The operating system releases it when the
// process exits for any reason.
type lease struct {
	st     *state
	f      *os.File
	holder session.Holder
	hdr    session.Session // the header, current while the lease is held
	lost   chan struct{}
	done   bool
}

func (l *lease) Renew(ctx context.Context) error {
	l.st.mu.Lock()
	defer l.st.mu.Unlock()
	if l.done {
		return fmt.Errorf("%w: lease released", session.ErrLocked)
	}
	return nil
}

func (l *lease) Release() error {
	l.st.mu.Lock()
	defer l.st.mu.Unlock()
	if l.done {
		return nil
	}
	l.done = true
	if l.st.lease == l {
		l.st.lease = nil
	}
	close(l.lost)
	return errors.Join(unlock(l.f), l.f.Close())
}

func (l *lease) Lost() <-chan struct{} { return l.lost }

var _ session.Store = (*Store)(nil)
