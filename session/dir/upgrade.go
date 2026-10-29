// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"latere.ai/x/topos/session"
)

// subagentMarker is the file under sessions/ that records that every log
// of the directory names a subagent's events as spec 013 does, so Open
// reads each log for the rename once.
const subagentMarker = ".subagent-events"

// renamedTypes are the types a subagent's events had in a log written
// before spec 013 named them subagent.*, by the type each has now.
var renamedTypes = map[string]session.Type{
	"thread.started": session.TypeSubagentStarted,
	"thread.ended":   session.TypeSubagentEnded,
	"thread.message": session.TypeSubagentMessage,
}

// renameSubagents rewrites, once for the data directory, each log line
// written before a subagent's events were named subagent.*: a type of
// renamedTypes becomes the type it has now, and the thread field, the id
// of the subagent the event belongs to, becomes subagent with the same
// value. Every other line is kept byte for byte, so a batch's last line
// still closes it and a torn tail stays for recover to truncate.
func (s *Store) renameSubagents() error { return s.rewriteOnce(subagentMarker, s.renameSubagentsOf) }

// rewriteOnce runs rewrite on every session of the directory unless the
// marker is there, and writes the marker after the last, so a crash part
// way runs it on every session again at the next Open; a rewrite leaves a
// session it already rewrote as it is.
func (s *Store) rewriteOnce(marker string, rewrite func(id string) error) error {
	path := filepath.Join(s.root, marker)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dir: stat %s: %w", marker, err)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("dir: list sessions: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || session.CheckID(session.PrefixSession, name) != nil {
			continue
		}
		if err := rewrite(name); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, nil)
}

// lockedRewrite runs fn with the session's lock held, and fails for a
// session another open file holds, naming what it was to do.
func (s *Store) lockedRewrite(id, what string, fn func() error) (err error) {
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
		return fmt.Errorf("dir: %s %s: %w", what, id, &session.LockedError{Holder: readHolder(f)})
	}
	defer func() { err = errors.Join(err, unlock(f)) }()
	return fn()
}

// renameSubagentsOf rewrites one session's log under its lock.
func (s *Store) renameSubagentsOf(id string) error {
	path := s.eventsPath(id)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dir: read the events of %s: %w", id, err)
	}
	if !bytes.Contains(b, []byte(`"thread`)) {
		return nil
	}
	return s.lockedRewrite(id, "rename the subagent events of", func() error {
		// The log is read again under the lock, so no append lands
		// between the read and the replace.
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("dir: read the events of %s: %w", id, err)
		}
		out, changed, err := renameLines(b)
		if err != nil {
			return fmt.Errorf("dir: rename the subagent events of %s: %w", id, err)
		}
		if !changed {
			return nil
		}
		return writeFileAtomic(path, out)
	})
}

// renameLines returns b with each whole line renamed by renameLine, and
// whether any line changed.
func renameLines(b []byte) ([]byte, bool, error) {
	var out bytes.Buffer
	changed := false
	for len(b) > 0 {
		ln := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			ln = b[:i+1]
		}
		b = b[len(ln):]
		up, err := renameLine(ln)
		if err != nil {
			return nil, false, err
		}
		if up == nil {
			out.Write(ln)
			continue
		}
		out.Write(up)
		changed = true
	}
	return out.Bytes(), changed, nil
}

// renameLine is ln renamed and encoded as the store writes a line, or nil
// when ln is a torn tail, is not a line of the log, or names nothing
// renamed.
func renameLine(ln []byte) ([]byte, error) {
	body, whole := bytes.CutSuffix(ln, []byte("\n"))
	if !whole {
		return nil, nil
	}
	l, thread, ok := oldLine(body)
	if !ok {
		return nil, nil
	}
	now, renamed := renamedTypes[string(l.Type)]
	if thread == nil && !renamed {
		return nil, nil
	}
	if thread != nil {
		l.Subagent = *thread
	}
	if renamed {
		l.Type = now
	}
	enc, err := session.Marshal(l)
	if err != nil {
		return nil, err
	}
	return append(enc, '\n'), nil
}

// oldLine decodes body as a line of the log, with the thread field a line
// written before a subagent's events were named subagent.* holds, nil when
// it holds none. It reports false when body is not a line of the log,
// which is kept as it is for the scan that reads the log to judge.
func oldLine(body []byte) (line, *string, bool) {
	var l line
	var old struct {
		Thread *string `json:"thread"`
	}
	if json.Unmarshal(body, &l) != nil || json.Unmarshal(body, &old) != nil {
		return line{}, nil, false
	}
	return l, old.Thread, true
}
