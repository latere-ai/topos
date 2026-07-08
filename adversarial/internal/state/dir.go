// Package state owns on-disk persistence: state-dir layout, atomic
// writes, run-level files, per-fork files. See specs/06-session-format.md.
package state

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session is one adversarial run's on-disk handle.
type Session struct {
	Root        string
	ID          string
	StateDirAbs string
	StartedAt   time.Time
}

// NewSession creates the session folder skeleton:
//
//	sessions/<id>/
//	sessions/<id>/forks/critic-N/         for N in 1..forkCount
//	sessions/<id>/forks/critic-N/rounds/
func NewSession(stateDirAbs string, forkCount int, now time.Time) (*Session, error) {
	if forkCount < 1 {
		return nil, errors.New("forkCount must be ≥ 1")
	}
	id := newSessionID(now)
	root := filepath.Join(stateDirAbs, "sessions", id)
	if _, err := os.Stat(root); err == nil {
		return nil, fmt.Errorf("session folder already exists: %s", root)
	}
	for i := 1; i <= forkCount; i++ {
		if err := os.MkdirAll(filepath.Join(root, "forks", fmt.Sprintf("critic-%d", i), "rounds"), 0o755); err != nil {
			return nil, err
		}
	}
	if err := fsync(filepath.Dir(root)); err != nil {
		return nil, err
	}
	return &Session{Root: root, ID: id, StateDirAbs: stateDirAbs, StartedAt: now}, nil
}

// Path returns the absolute path for rel (joined under Session root).
func (s *Session) Path(rel string) string {
	return filepath.Join(s.Root, rel)
}

// AtomicWrite writes data to <Root>/<rel> via temp file + rename.
// Fsyncs file then parent. Sets perm 0o644.
func (s *Session) AtomicWrite(rel string, data []byte) error {
	abs := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp := abs + ".tmp." + randSuffix()
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return fsync(filepath.Dir(abs))
}

// AppendLine appends data + "\n" to <Root>/<rel>.
// O_APPEND|O_CREATE|O_WRONLY, perm 0o644. No fsync per line.
func (s *Session) AppendLine(rel string, data []byte) error {
	abs := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// AppendCrossSessionLog appends one line to <stateDirAbs>/log.jsonl.
// The cross-session log lives outside any session folder.
func AppendCrossSessionLog(stateDirAbs string, data []byte) error {
	if err := os.MkdirAll(stateDirAbs, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDirAbs, "log.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// newSessionID returns "<YYYYMMDDTHHMMSSZ>-<rand6>" in UTC.
func newSessionID(now time.Time) string {
	return now.UTC().Format("20060102T150405Z") + "-" + randSuffix()
}

func randSuffix() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:])
	return strings.ToLower(enc)[:6]
}

func fsync(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
