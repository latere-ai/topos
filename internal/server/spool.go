// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"latere.ai/x/topos/session"
)

// spoolPrefix begins the name of every spool file, by which a server's
// start tells the files a stopped process left in its spool directory.
const spoolPrefix = "source-"

// errSpoolFull is a source the spools of the messages a server reads at
// once leave no room for.
var errSpoolFull = errors.New("the server holds as many bytes of messages' sources on disk as it takes at once")

// sources is what a server reads of the files messages name by source
// (spec 081): each source held to file bytes, a message's sources to
// message bytes together, and the spools of every message it reads at
// once to spool bytes on disk. The bounds are MaxSourceBytes,
// MaxMessageSourceBytes and MaxSpoolBytes, kept here so a test can lower
// them.
type sources struct {
	dir                  string
	file, message, spool int64
	used                 atomic.Int64
}

// newSources prepares dir to spool to: made when missing, and emptied of
// the spool files a process that stopped before removing them left. An
// empty dir spools to the system's temporary directory and removes
// nothing there.
func newSources(dir string) (*sources, error) {
	s := &sources{dir: dir, file: MaxSourceBytes, message: MaxMessageSourceBytes, spool: MaxSpoolBytes}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("server: the spool directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("server: the spool directory: %w", err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), spoolPrefix) || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("server: remove a spool file a stopped server left: %w", err)
		}
	}
	return s, nil
}

// spooled is a source past session.InlineTextBytes as the server holds
// it while the send that named it is checked: a file on disk, its length
// and digest, read as the source arrived, and its first bytes, which name
// its media type.
type spooled struct {
	s      *sources
	file   *os.File
	size   int64
	digest session.Digest
	head   []byte
	// reserved is what the spool counts toward the server's bound, freed
	// when the spool is closed.
	reserved int64
}

// sniffLen is how many bytes http.DetectContentType reads.
const sniffLen = 512

// read reads a source whole, hashing it as it arrives: into memory while
// it is at most inline bytes, so the inline rule reads its bytes as a
// data file's (spec 079), and onto a spool file past them, held with
// every other spool to the server's bound. It answers the bytes of a
// source kept in memory, or the spool of one that is not. r holds itself
// to the source's bound, as filestore.ReadSource's stream does.
func (s *sources) read(r io.Reader, inline int64) ([]byte, *spooled, error) {
	first := make([]byte, inline+1)
	n, err := io.ReadFull(r, first)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return first[:n], nil, nil
	case err != nil:
		return nil, nil, err
	}
	f, err := os.CreateTemp(s.dir, spoolPrefix+"*")
	if err != nil {
		return nil, nil, fmt.Errorf("spool the source: %w", err)
	}
	h := sha256.New()
	sp := &spooled{s: s, file: f, head: first[:min(len(first), sniffLen)]}
	w := &budgeted{w: io.MultiWriter(f, h), sp: sp}
	size, err := io.Copy(w, io.MultiReader(bytes.NewReader(first), r))
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("spool the source: %w", err), sp.close())
	}
	// The spool keeps its own copy of the head, the rest of first freed.
	sp.head = append([]byte(nil), sp.head...)
	sp.size, sp.digest = size, session.Digest("sha256:"+hex.EncodeToString(h.Sum(nil)))
	return nil, sp, nil
}

// budgeted writes to w within the server's spool bound, counting each
// write toward it before it is made.
type budgeted struct {
	w  io.Writer
	sp *spooled
}

func (b *budgeted) Write(p []byte) (int, error) {
	n := int64(len(p))
	if b.sp.s.used.Add(n) > b.sp.s.spool {
		b.sp.s.used.Add(-n)
		return 0, errSpoolFull
	}
	b.sp.reserved += n
	return b.w.Write(p)
}

// reader reads the spool from its start, by offset, so it can be read
// again whatever read it before.
func (sp *spooled) reader() io.Reader { return io.NewSectionReader(sp.file, 0, sp.size) }

// close removes the spool and frees what it counted toward the server's
// bound.
func (sp *spooled) close() error {
	sp.s.used.Add(-sp.reserved)
	sp.reserved = 0
	err := sp.file.Close()
	if rerr := os.Remove(sp.file.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		err = errors.Join(err, rerr)
	}
	return err
}
