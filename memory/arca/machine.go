// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package arca is a session's memory machine over an Arca-compatible
// files API (spec 093): the machine.Machine the runner carries a file
// tool's call on when every path the call names is under /memory/. Each memory folder an allow
// names is served at /memory/<name>/ from a folder of an Arca-compatible
// files API, with the session's own token, so no machine holds a
// credential and a call that reaches memory alone opens no machine.
//
// A read keeps the version the store answered, its ETag, for the path;
// a write sends If-Match on it, or If-None-Match: * for a path last seen
// absent or never seen, so a file another session changed since this
// one read it is refused and nothing is overwritten. A write of no bytes
// forgets the file: a DELETE with If-Match. A folder whose access is
// read_only refuses every change before any request. Commands and moves
// are refused. A spill file of a long result, the one path a memory call
// writes outside /memory, goes to the session's own machine.
package arca

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"unicode/utf8"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// maxCached bounds the bytes of the files a search keeps for the
// session, by checksum, so a second grep reads only what changed.
const maxCached = 32 << 20

// errCommands is an Exec on the memory machine: commands run on the
// session's own machine, which does not see /memory.
var errCommands = errors.New("memory: commands do not run in memory folders; read, write, edit, glob and grep reach them")

// errRename is a Rename into or out of a memory folder.
var errRename = errors.New("memory: a memory file is not moved; write it at the new path and write the old one empty to forget it")

// cleanRel reports whether rel is a path in a folder the tools can name:
// relative, with no empty, "." or ".." segment.
func cleanRel(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Machine is a session's memory machine.
type Machine struct {
	folders  []session.MemoryFolder
	token    Token
	api      *http.Client
	fallback machine.Machine

	mu sync.Mutex
	// versions are the version the store last answered for each path the
	// machine read, stat or wrote, "" for a path last seen absent.
	versions map[string]string
	// cache holds file contents by checksum, at most maxCached bytes.
	cache  map[string][]byte
	cached int
}

var _ machine.Machine = (*Machine)(nil)

// New is the memory machine of folders, each reached through client with
// the bearer token answers. fallback is the session's own machine, which
// takes a spill file of a long result and any path outside /memory; nil
// takes none.
func New(folders []session.MemoryFolder, token Token, client *http.Client, fallback machine.Machine) (*Machine, error) {
	if err := session.CheckMemory(folders); err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	if len(folders) == 0 || token == nil || client == nil {
		return nil, fmt.Errorf("memory: no folders, no token or no HTTP client")
	}
	api := *client
	api.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Machine{folders: folders, token: token, api: &api, fallback: fallback, versions: map[string]string{}, cache: map[string][]byte{}}, nil
}

// folder is the folder named name, false for none of the session's.
func (m *Machine) folder(name string) (session.MemoryFolder, bool) {
	for _, f := range m.folders {
		if f.Name == name {
			return f, true
		}
	}
	return session.MemoryFolder{}, false
}

// Folders are the session's memory folders.
func (m *Machine) Folders() []session.MemoryFolder { return m.folders }

func (m *Machine) Info() machine.Info {
	return machine.Info{Kind: memory.Kind, ID: memory.Kind, Workdir: memory.Root, OS: "memory", Arch: "memory"}
}

func (m *Machine) Roots() []string { return []string{memory.Root} }

// SpillDir is the session machine's, where a long result's spill file
// goes.
func (m *Machine) SpillDir() string {
	if m.fallback == nil {
		return ""
	}
	return m.fallback.SpillDir()
}

// Open opens the session's machine opened on demand, which only a spill
// file needs: the harness opens no machine for a memory call.
func (m *Machine) Open(ctx context.Context) error {
	if m.fallback == nil {
		return nil
	}
	return machine.Open(ctx, m.fallback)
}

func (m *Machine) Exec(context.Context, machine.ExecRequest) (machine.ExecResult, error) {
	return machine.ExecResult{}, errCommands
}

func (m *Machine) ExecStream(context.Context, machine.ExecRequest) (machine.ExecStream, error) {
	return nil, errCommands
}

// outside is the session's machine for a path outside /memory, an error
// when the memory machine has none.
func (m *Machine) outside(p string) (machine.Machine, error) {
	if m.fallback == nil {
		return nil, fmt.Errorf("%w: %s", machine.ErrOutside, p)
	}
	return m.fallback, nil
}

// place is where p is: its folder and its path in the folder. A path
// under /memory that names no folder of the session is not there.
func (m *Machine) place(op, p string) (session.MemoryFolder, string, error) {
	name, rel, _ := memory.Locate(p)
	f, ok := m.folder(name)
	if !ok {
		return session.MemoryFolder{}, "", notExist(op, p)
	}
	return f, rel, nil
}

func notExist(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist} }

func isDir(op, p string) error {
	return &fs.PathError{Op: op, Path: p, Err: fmt.Errorf("is a directory")}
}

// keep records the version the store answered for p.
func (m *Machine) keep(p, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.versions[path.Clean(p)] = version
}

// version is the version kept for p, and whether any was.
func (m *Machine) version(p string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[path.Clean(p)]
	return v, ok
}

// remember keeps a file's content by its checksum, emptying the cache
// first when it would hold more than maxCached bytes.
func (m *Machine) remember(checksum string, data []byte) {
	if checksum == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cache[checksum]; ok {
		return
	}
	if m.cached+len(data) > maxCached {
		m.cache, m.cached = map[string][]byte{}, 0
	}
	m.cache[checksum] = data
	m.cached += len(data)
}

// recalled is the content kept for checksum.
func (m *Machine) recalled(checksum string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.cache[checksum]
	return b, ok
}

func (m *Machine) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	if !memory.Under(p) {
		o, err := m.outside(p)
		if err != nil {
			return nil, err
		}
		return o.ReadFile(ctx, p)
	}
	f, rel, err := m.place("read", p)
	if err != nil {
		return nil, err
	}
	if rel == "" {
		return nil, isDir("read", p)
	}
	data, err := m.read(ctx, f, rel, p)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// read reads rel of f and keeps the version it read.
func (m *Machine) read(ctx context.Context, f session.MemoryFolder, rel, p string) ([]byte, error) {
	a, err := m.send(ctx, http.MethodGet, fileURL(f, rel), nil, nil, memory.MaxFileBytes, p)
	if err != nil {
		return nil, err
	}
	if a.status != http.StatusOK {
		if a.status == http.StatusNotFound {
			m.keep(p, "")
		}
		return nil, failed("read", p, a)
	}
	v := etagOf(a.header)
	m.keep(p, v)
	m.remember(v, a.body)
	return a.body, nil
}

func (m *Machine) Stat(ctx context.Context, p string) (machine.FileInfo, error) {
	if !memory.Under(p) {
		o, err := m.outside(p)
		if err != nil {
			return machine.FileInfo{}, err
		}
		return o.Stat(ctx, p)
	}
	name, _, _ := memory.Locate(p)
	if name == "" {
		return machine.FileInfo{Path: memory.Root, IsDir: true, Mode: fs.ModeDir | 0o555}, nil
	}
	f, rel, err := m.place("stat", p)
	if err != nil {
		return machine.FileInfo{}, err
	}
	if rel == "" {
		return machine.FileInfo{Path: path.Clean(p), IsDir: true, Mode: dirMode(f)}, nil
	}
	a, err := m.send(ctx, http.MethodHead, fileURL(f, rel), nil, nil, noBody, p)
	if err != nil {
		return machine.FileInfo{}, err
	}
	switch {
	case a.status == http.StatusNotFound:
		m.keep(p, "")
		return machine.FileInfo{}, notExist("stat", p)
	case a.status/100 == 4 && a.status != http.StatusPreconditionFailed:
		// An answer to a HEAD has no body, so the refusal's envelope is
		// read from the same request as a read.
		_, err := m.read(ctx, f, rel, p)
		if err == nil {
			err = failed("stat", p, a)
		}
		return machine.FileInfo{}, err
	case a.status/100 != 2 && a.status/100 != 3:
		return machine.FileInfo{}, failed("stat", p, a)
	}
	m.keep(p, etagOf(a.header))
	info := machine.FileInfo{Path: path.Clean(p), Size: max(a.length, 0), Mode: fileMode(f)}
	if t, err := http.ParseTime(a.header.Get("Last-Modified")); err == nil {
		info.ModTime = t
	}
	return info, nil
}

func dirMode(f session.MemoryFolder) fs.FileMode {
	if f.Writable() {
		return fs.ModeDir | 0o755
	}
	return fs.ModeDir | 0o555
}

func fileMode(f session.MemoryFolder) fs.FileMode {
	if f.Writable() {
		return 0o644
	}
	return 0o444
}

// List answers the entries directly in a directory: the session's
// folders in /memory, and in a folder or a directory of one the files
// and the directories their paths imply, of the folder's first memory.MaxListed
// files.
func (m *Machine) List(ctx context.Context, p string) ([]machine.FileInfo, error) {
	if !memory.Under(p) {
		o, err := m.outside(p)
		if err != nil {
			return nil, err
		}
		return o.List(ctx, p)
	}
	name, _, _ := memory.Locate(p)
	if name == "" {
		out := make([]machine.FileInfo, 0, len(m.folders))
		for _, f := range m.folders {
			out = append(out, machine.FileInfo{Path: memory.Root + "/" + f.Name, IsDir: true, Mode: dirMode(f)})
		}
		return out, nil
	}
	f, rel, err := m.place("list", p)
	if err != nil {
		return nil, err
	}
	files, _, err := m.list(ctx, f)
	if err != nil {
		return nil, err
	}
	base := memory.Root + "/" + f.Name
	prefix := ""
	if rel != "" {
		base, prefix = base+"/"+rel, rel+"/"
	}
	var out []machine.FileInfo
	seen := map[string]bool{}
	for _, e := range files {
		rest, ok := strings.CutPrefix(e.rel, prefix)
		if !ok {
			if e.rel == rel {
				return nil, &fs.PathError{Op: "list", Path: p, Err: fmt.Errorf("not a directory")}
			}
			continue
		}
		head, _, nested := strings.Cut(rest, "/")
		if seen[head] {
			continue
		}
		seen[head] = true
		if nested {
			out = append(out, machine.FileInfo{Path: base + "/" + head, IsDir: true, Mode: dirMode(f)})
			continue
		}
		out = append(out, machine.FileInfo{Path: base + "/" + head, Size: e.size, ModTime: e.modified, Mode: fileMode(f)})
	}
	if rel != "" && len(out) == 0 {
		return nil, notExist("list", p)
	}
	return out, nil
}

// WriteFile writes the file at p, conditional on the version the machine
// last read of it, and forgets it when r holds no bytes.
func (m *Machine) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
	if !memory.Under(p) {
		o, err := m.outside(p)
		if err != nil {
			return err
		}
		return o.WriteFile(ctx, p, r, mode)
	}
	f, rel, err := m.changed("write", p)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(r, memory.MaxFileBytes+1))
	if err != nil {
		return fmt.Errorf("memory: read what is written to %s: %w", p, err)
	}
	if len(data) > memory.MaxFileBytes {
		return &memory.TooLarge{Path: p, Size: int64(len(data))}
	}
	if len(data) == 0 {
		return m.forget(ctx, f, rel, p)
	}
	h := http.Header{"Content-Type": {mediaType(rel, data)}}
	if v, ok := m.version(p); ok && v != "" {
		h.Set("If-Match", `"`+v+`"`)
	} else {
		h.Set("If-None-Match", "*")
	}
	a, err := m.send(ctx, http.MethodPut, fileURL(f, rel), data, h, maxErrorBody, p)
	if err != nil {
		return err
	}
	if a.status/100 != 2 {
		return failed("write", p, a)
	}
	v := etagOf(a.header)
	m.keep(p, v)
	m.remember(v, data)
	return nil
}

// changed is where a change of p lands: a file of a folder the session
// may change. A folder whose access is read_only refuses it before any
// request.
func (m *Machine) changed(op, p string) (session.MemoryFolder, string, error) {
	f, rel, err := m.place(op, p)
	if err != nil {
		return session.MemoryFolder{}, "", err
	}
	if rel == "" {
		return session.MemoryFolder{}, "", isDir(op, p)
	}
	if !f.Writable() {
		return session.MemoryFolder{}, "", &memory.ReadOnly{Folder: f.Name}
	}
	return f, rel, nil
}

// forget deletes the file at p, conditional on the version the machine
// last read of it: a path last seen absent is not there to forget, and
// one never seen is stat first for its version.
func (m *Machine) forget(ctx context.Context, f session.MemoryFolder, rel, p string) error {
	v, ok := m.version(p)
	if !ok {
		if _, err := m.Stat(ctx, p); err != nil {
			return err
		}
		v, _ = m.version(p)
	}
	if v == "" {
		return notExist("forget", p)
	}
	a, err := m.send(ctx, http.MethodDelete, fileURL(f, rel), nil, http.Header{"If-Match": {`"` + v + `"`}}, maxErrorBody, p)
	if err != nil {
		return err
	}
	if a.status/100 != 2 {
		return failed("forget", p, a)
	}
	m.keep(p, "")
	return nil
}

// Remove forgets the file at p, as a write of no bytes does.
func (m *Machine) Remove(ctx context.Context, p string) error {
	if !memory.Under(p) {
		o, err := m.outside(p)
		if err != nil {
			return err
		}
		return o.Remove(ctx, p)
	}
	f, rel, err := m.changed("remove", p)
	if err != nil {
		return err
	}
	return m.forget(ctx, f, rel, p)
}

// Rename is refused for a path under /memory: a memory file is not
// moved.
func (m *Machine) Rename(ctx context.Context, from, to string) error {
	if memory.Under(from) || memory.Under(to) {
		return errRename
	}
	o, err := m.outside(from)
	if err != nil {
		return err
	}
	return o.Rename(ctx, from, to)
}

// Release keeps nothing to release: the session's machine is the
// harness's to release.
func (m *Machine) Release(context.Context, bool) error { return nil }

// mediaType is the type a written file is stored under: Markdown for a
// .md file, plain text for any other text, and bytes otherwise.
func mediaType(rel string, data []byte) string {
	switch {
	case strings.EqualFold(path.Ext(rel), ".md"):
		return "text/markdown; charset=utf-8"
	case utf8.Valid(data):
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
