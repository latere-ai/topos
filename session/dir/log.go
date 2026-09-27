// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"latere.ai/x/topos/session"
)

// line is one line of events.jsonl: the Event, and on the last line of
// every batch the batch's event count. A batch is one write; lines after
// the last one that closes a batch belong to a batch that was never
// acknowledged.
type line struct {
	session.Event
	Batch int `json:"batch,omitempty"`
}

// encodeBatch renders a batch as the bytes of one write.
func encodeBatch(events []session.Event) ([]byte, error) {
	var buf bytes.Buffer
	for i, e := range events {
		l := line{Event: e}
		if i == len(events)-1 {
			l.Batch = len(events)
		}
		b, err := session.Marshal(l)
		if err != nil {
			return nil, fmt.Errorf("dir: encode event %s: %w", e.ID, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// logScan is the committed content of an events file.
type logScan struct {
	lines []line
	// size is the byte length of the committed prefix; bytes after it
	// belong to an unacknowledged batch or a torn line.
	size int64
	// total is the file's length when it was read.
	total int64
}

func (s logScan) events() []session.Event {
	out := make([]session.Event, len(s.lines))
	for i, l := range s.lines {
		out[i] = l.Event
	}
	return out
}

// scanLog reads the committed lines of r. A line that does not decode
// is ErrCorrupt unless every line after it is uncommitted too, in which
// case it is a torn tail.
func scanLog(r io.Reader) (logScan, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var out logScan
	var pending []line
	var pendingBytes int64
	var bad error
	for {
		b, err := br.ReadBytes('\n')
		out.total += int64(len(b))
		if len(b) > 0 && b[len(b)-1] == '\n' {
			var l line
			if derr := json.Unmarshal(b, &l); derr != nil {
				if bad == nil {
					bad = fmt.Errorf("%w: undecodable line at byte %d: %w", session.ErrCorrupt, out.size+pendingBytes, derr)
				}
			} else if bad == nil {
				pending = append(pending, l)
			}
			pendingBytes += int64(len(b))
			if bad == nil && l.Batch > 0 {
				if l.Batch != len(pending) {
					return logScan{}, fmt.Errorf("%w: a batch of %d closes %d lines at seq %d", session.ErrCorrupt, l.Batch, len(pending), l.Seq)
				}
				out.lines = append(out.lines, pending...)
				out.size += pendingBytes
				pending, pendingBytes = nil, 0
			} else if bad != nil && l.Batch > 0 {
				return logScan{}, bad
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return logScan{}, fmt.Errorf("dir: read events: %w", err)
		}
	}
	return out, nil
}

// readLog scans the events file at path.
func readLog(path string) (s logScan, err error) {
	f, err := os.Open(path)
	if err != nil {
		return logScan{}, fmt.Errorf("dir: open events: %w", err)
	}
	defer closeInto(&err, f)
	return scanLog(f)
}

// closeInto closes c and joins its error into *err, for a deferred close
// of a file a function only reads.
func closeInto(err *error, c io.Closer) {
	if cerr := c.Close(); cerr != nil {
		*err = errors.Join(*err, cerr)
	}
}

// tailAfter returns the committed events of the file at path whose
// sequence is above after, reading backwards from the end so a header
// read costs the tail, not the log. The scan starts after a line that
// closes a batch at or below after, so it never begins inside a batch.
func tailAfter(path string, after uint64) (evs []session.Event, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dir: open events: %w", err)
	}
	defer closeInto(&err, f)
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("dir: stat events: %w", err)
	}
	const chunk = 64 << 10
	pos := fi.Size()
	var buf []byte
	for {
		n := min(chunk, pos)
		pos -= n
		b := make([]byte, n, int64(len(buf))+n)
		if _, err := f.ReadAt(b, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("dir: read events: %w", err)
		}
		buf = append(b, buf...)
		start := 0
		if pos > 0 {
			off, seq, ok := boundary(buf)
			if !ok || seq > after {
				continue
			}
			start = off
		}
		s, err := scanLog(bytes.NewReader(buf[start:]))
		if err != nil {
			return nil, err
		}
		var out []session.Event
		for _, e := range s.events() {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		return out, nil
	}
}

// boundary finds, in b that may begin inside a line, the first complete
// line that closes a batch, and returns the offset after it and its
// sequence.
func boundary(b []byte) (int, uint64, bool) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return 0, 0, false
	}
	off := i + 1
	for off < len(b) {
		j := bytes.IndexByte(b[off:], '\n')
		if j < 0 {
			return 0, 0, false
		}
		var l struct {
			Seq   uint64 `json:"seq"`
			Batch int    `json:"batch"`
		}
		end := off + j + 1
		if json.Unmarshal(b[off:end], &l) == nil && l.Batch > 0 {
			return end, l.Seq, true
		}
		off = end
	}
	return 0, 0, false
}

// appendBatch writes a batch in one write and syncs it.
func appendBatch(path string, events []session.Event) error {
	b, err := encodeBatch(events)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("dir: open events: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("dir: write events: %w", err), f.Close())
	}
	hook("events.written")
	if err := fsync(f); err != nil {
		return errors.Join(fmt.Errorf("dir: sync events: %w", err), f.Close())
	}
	hook("events.synced")
	if err := f.Close(); err != nil {
		return fmt.Errorf("dir: close events: %w", err)
	}
	return nil
}

// rewriteLog replaces the events file with lines, atomically.
func rewriteLog(path string, lines []line) error {
	var buf bytes.Buffer
	for _, l := range lines {
		b, err := session.Marshal(l)
		if err != nil {
			return fmt.Errorf("dir: encode event %s: %w", l.ID, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(path, buf.Bytes())
}

// writeFileAtomic writes b to a temporary file beside path, syncs it,
// renames it over path and syncs the directory.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("dir: create temporary file: %w", err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			err = errors.Join(err, cerr)
		}
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return errors.Join(err, rerr)
		}
		return err
	}
	if _, err := f.Write(b); err != nil {
		return fail(fmt.Errorf("dir: write %s: %w", filepath.Base(path), err))
	}
	if err := fsync(f); err != nil {
		return fail(fmt.Errorf("dir: sync %s: %w", filepath.Base(path), err))
	}
	if err := f.Close(); err != nil {
		return fail(fmt.Errorf("dir: close %s: %w", filepath.Base(path), err))
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail(fmt.Errorf("dir: rename %s: %w", filepath.Base(path), err))
	}
	return syncDir(dir)
}

// syncDir makes the directory's entries durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("dir: open directory: %w", err)
	}
	serr := fsync(d)
	cerr := d.Close()
	if serr != nil {
		return fmt.Errorf("dir: sync directory %s: %w", dir, serr)
	}
	if cerr != nil {
		return fmt.Errorf("dir: close directory: %w", cerr)
	}
	return nil
}

// fsync makes a file's or a directory's content durable. Tests replace
// it to prove every caller returns a failed sync rather than
// acknowledging a write the disk may not hold.
var fsync = func(f *os.File) error { return f.Sync() }

// hook marks a durability point. Tests replace it to stop a process
// there.
var hook = func(point string) {}
