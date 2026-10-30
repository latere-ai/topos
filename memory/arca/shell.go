// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"latere.ai/x/cella/shell"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// changedWords is how a command line says a write refused because the
// file changed since this session read it: the file tools' words for the
// same refusal, after the path the shell names.
const changedWords = "changed since it was last read; read it again before writing"

// Mounts are the directories a command line reaches the memory folders
// at (spec 097): /memory, which no line changes, and each folder, read
// only unless the session may change it.
func Mounts(folders []session.MemoryFolder) []shell.Mount {
	out := make([]shell.Mount, 0, len(folders)+1)
	out = append(out, shell.Mount{Dir: memory.Root, ReadOnly: true})
	for _, f := range folders {
		out = append(out, shell.Mount{Dir: memory.Root + "/" + f.Name, ReadOnly: !f.Writable()})
	}
	return out
}

// Runs reports whether the machine runs command itself, started in dir,
// as Exec would: an empty reason when every program the line names is
// one the shell provides and every path it names is in a memory folder,
// and otherwise the shell's reason and the program it names. It decides
// before anything runs, so a line it refuses has done nothing.
func (m *Machine) Runs(command, dir string) (reason, program string) {
	p := shell.Check(command, dir, Mounts(m.folders))
	return string(p.Reason), p.Program
}

// Exec runs a command line in the process over the memory folders (spec
// 097), when Runs allowed it from the same directory: no machine opens,
// and every read and write is the machine's own, with the version each
// path was read at. A background command, one with input or variables of
// its own, and a line Runs refuses are not run here.
func (m *Machine) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	if r.Background || r.Stdin != nil || len(r.Env) > 0 {
		return machine.ExecResult{}, errCommands
	}
	run := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		run, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	now := time.Now()
	req := shell.Request{Command: r.Command, Dir: r.Dir, Name: "/bin/sh", Now: now}
	if r.ReportDir {
		// A machine's shell runs the line after the line that reports
		// its directory, so its messages count from 2.
		req.Line = 2
	}
	res, err := shell.Run(run, &served{m: m, now: now, listings: map[string]*folderListing{}}, Mounts(m.folders), req)
	out := machine.ExecResult{Output: res.Output, ExitCode: res.ExitCode}
	if r.ReportDir {
		out.Dir = res.Dir
		if out.Dir == "" {
			out.Dir = r.Dir
		}
	}
	var declined *shell.Declined
	switch {
	case err == nil:
	case ctx.Err() != nil:
		out.Canceled, out.ExitCode, out.Dir = true, -1, ""
	case run.Err() != nil:
		out.TimedOut, out.ExitCode, out.Dir = true, -1, ""
	case errors.As(err, &declined):
		return machine.ExecResult{}, errCommands
	default:
		return machine.ExecResult{}, fmt.Errorf("memory: run the command: %w", err)
	}
	return out, nil
}

// served is the memory folders as a command line reaches them: a
// shell.FS over the machine's reads and writes, so a write is
// conditional on the version the session last read, as a file tool's
// is. A line's stats and listings read each folder's listing once and
// keep it for the line, changed by the line's own writes.
type served struct {
	m *Machine
	// now is when the line started: the time /memory and each folder
	// show, which keep none of their own.
	now time.Time

	mu       sync.Mutex
	listings map[string]*folderListing
}

// folderListing is a folder's files as the line sees them.
type folderListing struct {
	files map[string]listed
	// more is set when the folder holds more files than a listing reads:
	// a path not listed may be there.
	more bool
}

var _ shell.FS = (*served)(nil)

// listing is the folder's listing, read once for the line.
func (s *served) listing(ctx context.Context, f session.MemoryFolder) (*folderListing, error) {
	s.mu.Lock()
	l, ok := s.listings[f.Name]
	s.mu.Unlock()
	if ok {
		return l, nil
	}
	files, more, err := s.m.list(ctx, f)
	if err != nil {
		return nil, err
	}
	l = &folderListing{files: make(map[string]listed, len(files)), more: more}
	for _, e := range files {
		l.files[e.rel] = e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept, ok := s.listings[f.Name]; ok {
		return kept, nil
	}
	s.listings[f.Name] = l
	return l, nil
}

// cached is the folder's listing when the line has read it, nil before.
func (s *served) cached(f session.MemoryFolder) *folderListing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listings[f.Name]
}

// wrote records a write of rel in the folder's listing, when the line
// read it: its new size and version, or its removal.
func (s *served) wrote(f session.MemoryFolder, rel string, size int64, version string, removed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.listings[f.Name]
	if l == nil {
		return
	}
	if removed {
		delete(l.files, rel)
		return
	}
	l.files[rel] = listed{rel: rel, size: size, checksum: version, modified: time.Now()}
}

// locate is the folder of name and the path in it: an empty rel for the
// folder itself, and no folder for /memory.
func (s *served) locate(op, name string) (session.MemoryFolder, string, bool, error) {
	folder, rel, _ := memory.Locate(name)
	if folder == "" {
		return session.MemoryFolder{}, "", false, nil
	}
	f, ok := s.m.folder(folder)
	if !ok {
		return session.MemoryFolder{}, "", false, notExist(op, name)
	}
	return f, rel, true, nil
}

func (s *served) ReadFile(ctx context.Context, name string) ([]byte, error) {
	f, rel, ok, err := s.locate("open", name)
	switch {
	case err != nil:
		return nil, err
	case !ok || rel == "":
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
	}
	// A file the line listed at a version the session holds the content
	// of is read from it, as a search reads it.
	if l := s.cached(f); l != nil {
		if e, ok := l.files[rel]; ok {
			if data, ok := s.m.recalled(e.checksum); ok {
				s.m.keep(name, e.checksum)
				return data, nil
			}
		}
	}
	data, err := s.m.read(ctx, f, rel, name)
	if errors.Is(err, fs.ErrNotExist) {
		if info, serr := s.Stat(ctx, name); serr == nil && info.IsDir() {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
		}
	}
	return data, shellError("open", name, err)
}

func (s *served) WriteFile(ctx context.Context, name string, data []byte) error {
	f, rel, ok, err := s.locate("write", name)
	switch {
	case err != nil:
		return err
	case !ok || rel == "":
		return &fs.PathError{Op: "write", Path: name, Err: syscall.EISDIR}
	}
	if _, kept := s.m.version(name); !kept {
		// A line that writes a file it never read means to replace it,
		// as a shell's redirection does: the write is conditional on the
		// version the file is at now, so a change between this look and
		// the write is still refused.
		if err := s.look(ctx, f, rel, name); err != nil {
			return shellError("write", name, err)
		}
	}
	if len(data) == 0 {
		// A memory file of no bytes is a file forgotten (spec 093): a
		// line that empties one forgets it, and one that makes an empty
		// file makes none.
		if v, _ := s.m.version(name); v == "" {
			return nil
		}
	}
	if err := s.m.WriteFile(ctx, name, bytes.NewReader(data), 0); err != nil {
		return shellError("write", name, err)
	}
	v, _ := s.m.version(name)
	s.wrote(f, rel, int64(len(data)), v, len(data) == 0)
	return nil
}

// look keeps the version a file the session never read is at now: the
// line's listing's, or a stat's, "" for a file not there.
func (s *served) look(ctx context.Context, f session.MemoryFolder, rel, name string) error {
	if l := s.cached(f); l != nil {
		if e, ok := l.files[rel]; ok {
			s.m.keep(name, e.checksum)
			return nil
		}
		if !l.more {
			s.m.keep(name, "")
			return nil
		}
	}
	_, err := s.m.Stat(ctx, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *served) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	f, rel, ok, err := s.locate("stat", name)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return shell.NewFileInfo(path.Base(memory.Root), 0, fs.ModeDir|0o555, s.now), nil
	case rel == "":
		return shell.NewFileInfo(f.Name, 0, dirMode(f), s.now), nil
	}
	l, err := s.listing(ctx, f)
	if err != nil {
		return nil, shellError("stat", name, err)
	}
	if e, ok := l.files[rel]; ok {
		return shell.NewFileInfo(path.Base(rel), e.size, fileMode(f), e.modified), nil
	}
	if mod, ok := l.dir(rel); ok {
		return shell.NewFileInfo(path.Base(rel), 0, dirMode(f), mod), nil
	}
	if !l.more {
		return nil, notExist("stat", name)
	}
	// A folder past the listing's bound may hold the file all the same.
	info, err := s.m.Stat(ctx, name)
	if err != nil {
		return nil, shellError("stat", name, err)
	}
	return shell.NewFileInfo(path.Base(rel), info.Size, info.Mode, info.ModTime), nil
}

// dir reports whether rel is a directory of the listing, one a file's
// path implies, and the time of the newest file under it.
func (l *folderListing) dir(rel string) (time.Time, bool) {
	var mod time.Time
	found := false
	for p, e := range l.files {
		if strings.HasPrefix(p, rel+"/") {
			found = true
			if e.modified.After(mod) {
				mod = e.modified
			}
		}
	}
	return mod, found
}

func (s *served) ReadDir(ctx context.Context, name string) ([]fs.FileInfo, error) {
	f, rel, ok, err := s.locate("readdir", name)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		out := make([]fs.FileInfo, 0, len(s.m.folders))
		for _, f := range s.m.folders {
			out = append(out, shell.NewFileInfo(f.Name, 0, dirMode(f), s.now))
		}
		return out, nil
	}
	l, err := s.listing(ctx, f)
	if err != nil {
		return nil, shellError("readdir", name, err)
	}
	prefix := ""
	if rel != "" {
		if _, file := l.files[rel]; file {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: syscall.ENOTDIR}
		}
		prefix = rel + "/"
	}
	type entry struct {
		size int64
		mod  time.Time
		dir  bool
	}
	entries := map[string]entry{}
	for p, e := range l.files {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		head, _, nested := strings.Cut(rest, "/")
		old, seen := entries[head]
		switch {
		case !nested:
			entries[head] = entry{size: e.size, mod: e.modified}
		case !seen || e.modified.After(old.mod):
			entries[head] = entry{mod: e.modified, dir: true}
		}
	}
	if rel != "" && len(entries) == 0 {
		return nil, notExist("readdir", name)
	}
	out := make([]fs.FileInfo, 0, len(entries))
	for _, head := range slices.Sorted(maps.Keys(entries)) {
		e := entries[head]
		if e.dir {
			out = append(out, shell.NewFileInfo(head, 0, dirMode(f), e.mod))
		} else {
			out = append(out, shell.NewFileInfo(head, e.size, fileMode(f), e.mod))
		}
	}
	return out, nil
}

func (s *served) Remove(ctx context.Context, name string) error {
	f, rel, ok, err := s.locate("remove", name)
	switch {
	case err != nil:
		return err
	case !ok || rel == "":
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.EBUSY}
	}
	if l := s.cached(f); l != nil {
		if _, file := l.files[rel]; !file {
			if _, dir := l.dir(rel); dir || !l.more {
				// A directory is there while a file is under it, and goes
				// with the last of them.
				return notExist("remove", name)
			}
		}
	}
	if err := s.m.Remove(ctx, name); err != nil {
		return shellError("remove", name, err)
	}
	s.wrote(f, rel, 0, "", true)
	return nil
}

// shellError is an error of the memory machine as a command line says
// it: a file changed since the session read it in the file tools' words,
// a refusal of the files store in its own, a folder read only as a
// read-only file system, a file past the bound as one too large, a
// store not reached as an input/output error, and a path that is not a
// directory as such.
func shellError(op, name string, err error) error {
	var (
		refusal     *memory.Refusal
		readOnly    *memory.ReadOnly
		tooLarge    *memory.TooLarge
		unreachable *memory.Unreachable
		pathErr     *fs.PathError
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, fs.ErrNotExist):
		return err
	case errors.Is(err, memory.ErrChanged):
		return &shell.Explained{Words: changedWords, Err: err}
	case errors.As(err, &refusal):
		return &shell.Explained{Words: strings.TrimSpace(strings.TrimSpace(refusal.Message) + " " + strings.TrimSpace(refusal.Detail)), Err: err}
	case errors.As(err, &readOnly):
		return &fs.PathError{Op: op, Path: name, Err: errors.Join(syscall.EROFS, err)}
	case errors.As(err, &tooLarge):
		return &fs.PathError{Op: op, Path: name, Err: errors.Join(syscall.EFBIG, err)}
	case errors.As(err, &unreachable):
		return &fs.PathError{Op: op, Path: name, Err: errors.Join(syscall.EIO, err)}
	case errors.As(err, &pathErr) && pathErr.Err.Error() == "not a directory":
		return &fs.PathError{Op: op, Path: name, Err: syscall.ENOTDIR}
	case errors.As(err, &pathErr) && pathErr.Err.Error() == "is a directory":
		return &fs.PathError{Op: op, Path: name, Err: syscall.EISDIR}
	}
	return err
}
