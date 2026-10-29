// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// readers is how many files a grep reads from the store at once.
const readers = 4

// Search runs a glob or a grep over the folders the search's path is in
// (spec 093): each folder's listing, at most memory.MaxListed files, and for a
// grep each file it covers, read once per checksum for the session and
// matched as the machine's own search matches. A path under no folder of
// the session is not there.
func (m *Machine) Search(ctx context.Context, q machine.SearchRequest) (machine.SearchResult, error) {
	if !memory.Under(q.Path) {
		o, err := m.outside(q.Path)
		if err != nil {
			return machine.SearchResult{}, err
		}
		return o.Search(ctx, q)
	}
	name, _, _ := memory.Locate(q.Path)
	scope := m.folders
	if name != "" {
		f, ok := m.folder(name)
		if !ok {
			return machine.SearchResult{}, notExist("search", q.Path)
		}
		scope = []session.MemoryFolder{f}
	}
	t := &tree{m: m, ctx: ctx, files: map[string]treeFile{}, dirs: map[string][]string{".": nil}}
	unlisted := false
	for _, f := range scope {
		files, more, err := m.list(ctx, f)
		if err != nil {
			return machine.SearchResult{}, err
		}
		unlisted = unlisted || more
		t.add(f, files)
	}
	t.sort()
	rel := strings.TrimPrefix(strings.TrimPrefix(path.Clean(q.Path), memory.Root), "/")
	if rel == "" {
		rel = "."
	}
	if q.Kind == machine.SearchGrep {
		if err := t.prefetch(rel); err != nil {
			return machine.SearchResult{}, err
		}
	}
	res, err := machine.SearchFS(ctx, t, memory.Root, rel, q)
	if err != nil {
		return machine.SearchResult{}, err
	}
	res.Unlisted = unlisted
	return res, nil
}

// treeFile is one listed file of a tree, by its folder.
type treeFile struct {
	folder session.MemoryFolder
	listed
}

// tree is the folders a search covers as an fs.FS rooted at /memory: a
// directory per folder, the directories its files' paths imply, and its
// files, whose contents are read from the store when a search opens
// them.
type tree struct {
	m     *Machine
	ctx   context.Context
	files map[string]treeFile
	dirs  map[string][]string
}

// add puts f's files in the tree, under the folder's name.
func (t *tree) add(f session.MemoryFolder, files []listed) {
	t.dir(f.Name)
	for _, e := range files {
		name := f.Name + "/" + e.rel
		t.files[name] = treeFile{folder: f, listed: e}
		t.dir(path.Dir(name))
		t.child(path.Dir(name), path.Base(name))
	}
}

// dir makes the directory name and each above it.
func (t *tree) dir(name string) {
	for name != "." {
		if _, ok := t.dirs[name]; ok {
			return
		}
		t.dirs[name] = nil
		t.child(path.Dir(name), path.Base(name))
		name = path.Dir(name)
	}
}

func (t *tree) child(dir, base string) {
	if !slices.Contains(t.dirs[dir], base) {
		t.dirs[dir] = append(t.dirs[dir], base)
	}
}

func (t *tree) sort() {
	for _, c := range t.dirs {
		slices.Sort(c)
	}
}

// prefetch reads the contents of the files under rel the session has not
// read at their checksum, readers at a time, so a grep's walk reads none
// from the store. A file past memory.MaxFileBytes is left for the walk to pass
// over, and one gone since the listing is left out.
func (t *tree) prefetch(rel string) error {
	var todo []string
	for name, f := range t.files {
		if rel != "." && name != rel && !strings.HasPrefix(name, rel+"/") {
			continue
		}
		if _, ok := t.m.recalled(f.checksum); ok || f.size > memory.MaxFileBytes {
			continue
		}
		todo = append(todo, name)
	}
	slices.Sort(todo)
	var (
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
		next  = make(chan string)
	)
	for range min(readers, len(todo)) {
		wg.Go(func() {
			for name := range next {
				if _, err := t.content(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		})
	}
	for _, name := range todo {
		next <- name
	}
	close(next)
	wg.Wait()
	return first
}

// content is the content of the file name: the one the session read at
// its checksum, or a read from the store.
func (t *tree) content(name string) ([]byte, error) {
	f, ok := t.files[name]
	if !ok {
		return nil, notExist("open", name)
	}
	if b, ok := t.m.recalled(f.checksum); ok {
		return b, nil
	}
	if f.size > memory.MaxFileBytes {
		return nil, &memory.TooLarge{Path: memory.Root + "/" + name, Size: f.size}
	}
	return t.m.read(t.ctx, f.folder, f.rel, memory.Root+"/"+name)
}

func (t *tree) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	info, err := t.Stat(name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return &treeDir{t: t, name: name, info: info}, nil
	}
	b, err := t.content(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &treeOpen{info: info, r: bytes.NewReader(b)}, nil
}

func (t *tree) Stat(name string) (fs.FileInfo, error) {
	if f, ok := t.files[name]; ok {
		return treeInfo{name: path.Base(name), size: f.size, mod: f.modified}, nil
	}
	if _, ok := t.dirs[name]; ok {
		return treeInfo{name: path.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (t *tree) ReadDir(name string) ([]fs.DirEntry, error) {
	children, ok := t.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(children))
	for _, c := range children {
		info, err := t.Stat(path.Join(name, c))
		if err != nil {
			return nil, err
		}
		out = append(out, fs.FileInfoToDirEntry(info))
	}
	return out, nil
}

// treeInfo is a tree's file or directory as fs.FileInfo.
type treeInfo struct {
	name string
	size int64
	mod  time.Time
	dir  bool
}

func (i treeInfo) Name() string { return i.name }
func (i treeInfo) Size() int64  { return i.size }
func (i treeInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i treeInfo) ModTime() time.Time { return i.mod }
func (i treeInfo) IsDir() bool        { return i.dir }
func (i treeInfo) Sys() any           { return nil }

// treeOpen is an open file of a tree.
type treeOpen struct {
	info fs.FileInfo
	r    *bytes.Reader
}

func (f *treeOpen) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *treeOpen) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *treeOpen) Close() error               { return nil }

// treeDir is an open directory of a tree; entries are what its reads
// have not answered yet, nil before the first.
type treeDir struct {
	t       *tree
	name    string
	info    fs.FileInfo
	entries []fs.DirEntry
	opened  bool
}

func (d *treeDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *treeDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}
func (d *treeDir) Close() error { return nil }

// ReadDir answers the directory's next n entries, every one left for n
// <= 0, and io.EOF for n > 0 once none is left.
func (d *treeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.opened {
		entries, err := d.t.ReadDir(d.name)
		if err != nil {
			return nil, err
		}
		d.entries, d.opened = entries, true
	}
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}
