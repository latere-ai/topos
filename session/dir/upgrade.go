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
	"strconv"
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
// still closes it and a torn tail stays for recover to truncate. The
// marker is written after every log, so a crash part way reads them all
// again on the next Open, and a log already renamed is left as it is.
func (s *Store) renameSubagents() error {
	marker := filepath.Join(s.root, subagentMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dir: stat %s: %w", subagentMarker, err)
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
		if err := s.renameSubagentsOf(name); err != nil {
			return err
		}
	}
	return writeFileAtomic(marker, nil)
}

// renameSubagentsOf rewrites one session's log under its lock.
func (s *Store) renameSubagentsOf(id string) (err error) {
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
		return fmt.Errorf("dir: rename the subagent events of %s: %w", id, &session.LockedError{Holder: readHolder(f)})
	}
	defer func() { err = errors.Join(err, unlock(f)) }()
	// The log is read again under the lock, so no append lands between
	// the read and the replace.
	if b, err = os.ReadFile(path); err != nil {
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
// when ln is a torn tail, does not decode, or names nothing renamed.
func renameLine(ln []byte) ([]byte, error) {
	body, whole := bytes.CutSuffix(ln, []byte("\n"))
	if !whole {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil, nil
	}
	changed := false
	if v, ok := fields["thread"]; ok {
		delete(fields, "thread")
		fields["subagent"] = v
		changed = true
	}
	var typ string
	if json.Unmarshal(fields["type"], &typ) == nil {
		if now, ok := renamedTypes[typ]; ok {
			fields["type"] = json.RawMessage(strconv.Quote(string(now)))
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	enc, err := session.Marshal(l)
	if err != nil {
		return nil, err
	}
	return append(enc, '\n'), nil
}
