// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
)

// deny is the credential deny-list inside a sandbox: the base-name
// entries that hold in any root. A sandbox has none of the person's home
// directory, so the home entries name nothing there (spec 018).
var deny = machine.DenyList{}

// target is a path resolved against the machine's roots.
type target struct {
	abs  string
	root string
	// routes is set for a path in the workspace, which Cella's file
	// routes serve; the helper serves the rest.
	routes bool
}

// resolve makes p absolute, refuses a path the deny-list names or one in
// no root, and finds the deepest root it is in.
func (m *Machine) resolve(p string) (target, error) {
	if _, err := m.usable(); err != nil {
		return target{}, err
	}
	a := m.abs(p)
	if deny.Path(a) {
		return target{}, fmt.Errorf("%w: %s", machine.ErrDenied, a)
	}
	m.mu.Lock()
	roots, workspace := m.roots, m.workspace
	m.mu.Unlock()
	best := ""
	for _, r := range roots {
		if under(a, r) && len(r) > len(best) {
			best = r
		}
	}
	if best == "" {
		return target{}, fmt.Errorf("%w: %s", machine.ErrOutside, a)
	}
	return target{abs: a, root: best, routes: workspace != "" && under(a, workspace)}, nil
}

// under reports whether p is root or below it.
func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// routeErr maps a file route's failure to the error the host machine
// returns for the same failure: not_found is the path's, because call has
// already told a sandbox that is gone.
func routeErr(op, p string, err error) error {
	if err == nil || errors.Is(err, ErrLost) || errors.Is(err, machine.ErrReleased) {
		return err
	}
	var ce *client.Error
	if errors.As(err, &ce) {
		switch ce.Code {
		case "not_found":
			return &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist}
		case "forbidden":
			return &fs.PathError{Op: op, Path: p, Err: fs.ErrPermission}
		}
		return fmt.Errorf("machine: %s %s: %w", op, p, refusal{ce})
	}
	return fmt.Errorf("machine: %s %s: %w", op, p, err)
}

func (m *Machine) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	t, err := m.resolve(p)
	if err != nil {
		return nil, err
	}
	if !t.routes {
		return m.helperRead(ctx, t)
	}
	var rc io.ReadCloser
	err = m.call(ctx, true, func(id string) error {
		var err error
		rc, err = m.c.FileGet(ctx, id, t.abs)
		return err
	})
	if err != nil {
		return nil, routeErr("open", t.abs, err)
	}
	return rc, nil
}

// WriteFile writes a file and its missing parents; Cella's route and the
// helper both write beside the path and rename over it. A mode of zero
// keeps an existing file's permissions, or is 0644.
func (m *Machine) WriteFile(ctx context.Context, p string, src io.Reader, mode fs.FileMode) error {
	t, err := m.resolve(p)
	if err != nil {
		return err
	}
	if t.abs == t.root {
		return fmt.Errorf("machine: %s is a directory", t.abs)
	}
	body, off, err := replayable(src)
	if err != nil {
		return fmt.Errorf("machine: read what to write to %s: %w", t.abs, err)
	}
	if !t.routes {
		return m.helperWrite(ctx, t, body, off, mode)
	}
	perm := ""
	if mode != 0 {
		perm = octal(mode)
	} else {
		fi, err := m.Stat(ctx, t.abs)
		switch {
		case err == nil:
			perm = octal(fi.Mode)
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	return routeErr("write", t.abs, m.call(ctx, true, func(id string) error {
		if _, err := body.Seek(off, io.SeekStart); err != nil {
			return err
		}
		return m.c.FilePut(ctx, id, t.abs, perm, body)
	}))
}

// octal renders permissions as Cella's file routes read them.
func octal(mode fs.FileMode) string { return fmt.Sprintf("%04o", mode.Perm()) }

// replayable returns a reader that can be sent again from where it
// starts, for a write that runs again after the sandbox was started: r
// itself when it seeks, and its bytes otherwise.
func replayable(r io.Reader) (io.ReadSeeker, int64, error) {
	if r == nil {
		return bytes.NewReader(nil), 0, nil
	}
	if rs, ok := r.(io.ReadSeeker); ok {
		if off, err := rs.Seek(0, io.SeekCurrent); err == nil {
			return rs, off, nil
		}
	}
	b, err := io.ReadAll(r)
	return bytes.NewReader(b), 0, err
}

func (m *Machine) Stat(ctx context.Context, p string) (machine.FileInfo, error) {
	t, err := m.resolve(p)
	if err != nil {
		return machine.FileInfo{}, err
	}
	if !t.routes {
		resp, err := m.helperFS(ctx, "stat", t)
		if err != nil {
			return machine.FileInfo{}, err
		}
		if len(resp.Entries) != 1 {
			return machine.FileInfo{}, fmt.Errorf("machine: the helper answered %d entries for %s", len(resp.Entries), t.abs)
		}
		return resp.Entries[0].info(), nil
	}
	var e client.FileEntry
	err = m.call(ctx, true, func(id string) error {
		var err error
		e, _, err = m.c.FileStat(ctx, id, t.abs)
		return err
	})
	if err != nil {
		return machine.FileInfo{}, routeErr("stat", t.abs, err)
	}
	return routeInfo(e)
}

// routeInfo reads an entry of Cella's file routes, whose mode is octal
// permissions.
func routeInfo(e client.FileEntry) (machine.FileInfo, error) {
	perm, err := strconv.ParseUint(e.Mode, 8, 32)
	if err != nil {
		return machine.FileInfo{}, fmt.Errorf("machine: the mode %q of %s: %w", e.Mode, e.Path, err)
	}
	mode := fs.FileMode(perm).Perm()
	if e.IsDir {
		mode |= fs.ModeDir
	}
	return machine.FileInfo{Path: e.Path, Size: e.Size, Mode: mode, ModTime: e.ModTime, IsDir: e.IsDir}, nil
}

// List answers a directory's entries without the ones the deny-list names.
func (m *Machine) List(ctx context.Context, p string) ([]machine.FileInfo, error) {
	t, err := m.resolve(p)
	if err != nil {
		return nil, err
	}
	if !t.routes {
		resp, err := m.helperFS(ctx, "list", t)
		if err != nil {
			return nil, err
		}
		out := make([]machine.FileInfo, 0, len(resp.Entries))
		for _, e := range resp.Entries {
			out = append(out, e.info())
		}
		return out, nil
	}
	entries, err := m.list(ctx, t)
	if err != nil {
		return nil, err
	}
	out := make([]machine.FileInfo, 0, len(entries))
	for _, e := range entries {
		if deny.Path(e.Path) {
			continue
		}
		fi, err := routeInfo(e)
		if err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

func (m *Machine) list(ctx context.Context, t target) ([]client.FileEntry, error) {
	var entries []client.FileEntry
	err := m.call(ctx, true, func(id string) error {
		var err error
		entries, _, err = m.c.FileList(ctx, id, t.abs)
		return err
	})
	return entries, routeErr("list", t.abs, err)
}

// Remove removes a file or an empty directory, as os.Remove does on the
// host; Cella's route removes a whole tree, so a directory with entries
// is refused before the route is asked.
func (m *Machine) Remove(ctx context.Context, p string) error {
	t, err := m.resolve(p)
	if err != nil {
		return err
	}
	if t.abs == t.root {
		return fmt.Errorf("machine: %s is a root and is not removed", t.abs)
	}
	if !t.routes {
		_, err := m.helperFS(ctx, "remove", t)
		return err
	}
	fi, err := m.Stat(ctx, t.abs)
	if err != nil {
		return err
	}
	if fi.IsDir {
		entries, err := m.list(ctx, t)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return &fs.PathError{Op: "remove", Path: t.abs, Err: syscall.ENOTEMPTY}
		}
	}
	return routeErr("remove", t.abs, m.call(ctx, true, func(id string) error {
		return m.c.FileRemove(ctx, id, t.abs)
	}))
}

// Rename renames within the workspace through Cella's route, or within
// one other root through the helper.
func (m *Machine) Rename(ctx context.Context, from, to string) error {
	tf, err := m.resolve(from)
	if err != nil {
		return err
	}
	tt, err := m.resolve(to)
	if err != nil {
		return err
	}
	switch {
	case tf.routes && tt.routes:
		return routeErr("rename", tf.abs, m.call(ctx, true, func(id string) error {
			return m.c.FileMove(ctx, id, tf.abs, tt.abs)
		}))
	case !tf.routes && !tt.routes && tf.root == tt.root:
		_, err := m.helperFS(ctx, "rename", tf, tt.abs)
		return err
	}
	return fmt.Errorf("machine: %s and %s are in different roots", tf.abs, tt.abs)
}

// helperFS runs one of the helper's file operations whose answer is JSON.
func (m *Machine) helperFS(ctx context.Context, op string, t target, extra ...string) (response, error) {
	resp, err := m.runHelper(ctx, nil, append([]string{"fs", op, t.root, t.abs}, extra...)...)
	if err != nil {
		return response{}, err
	}
	if resp.Error != nil {
		return response{}, resp.Error.err(op, t.abs)
	}
	return resp, nil
}

// helperRead streams a file through the helper's read mode.
func (m *Machine) helperRead(ctx context.Context, t target) (io.ReadCloser, error) {
	var fr *fileReader
	err := m.call(ctx, true, func(id string) error {
		sess, err := m.c.ExecSession(ctx, id, client.ExecRequest{Command: m.invoke("fs", "read", t.root, t.abs), Timeout: maxSession.String()})
		if err != nil {
			return err
		}
		if err := opened(sess, "open", t.abs); err != nil {
			return err
		}
		fr = &fileReader{sess: sess}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fr, nil
}

// fileReader is a file's bytes, decoded from the helper's frames.
type fileReader struct {
	sess    *client.Session
	pending []byte
	err     error

	once     sync.Once
	closeErr error
}

func (f *fileReader) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		kind, payload, err := readFrame(f.sess)
		switch {
		case err != nil:
			f.err = fmt.Errorf("machine: the read ended early: %w", err)
		case kind == frameOutput:
			f.pending = payload
		case kind == frameExit:
			f.err = io.EOF
		case kind == frameFail:
			f.err = failed(payload, "read", "")
		default:
			f.err = fmt.Errorf("machine: the helper sent a frame of kind %q", kind)
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *fileReader) Close() error {
	f.once.Do(func() { f.closeErr = closeSession(f.sess) })
	return f.closeErr
}

// failed decodes a failure frame.
func failed(payload []byte, op, p string) error {
	var e errorBody
	if err := jsonDecode(payload, &e); err != nil {
		return err
	}
	return e.err(op, p)
}

// helperWrite writes a file through the helper's write mode, whose input
// is framed because the exec session cannot end it.
func (m *Machine) helperWrite(ctx context.Context, t target, body io.ReadSeeker, off int64, mode fs.FileMode) error {
	return m.call(ctx, true, func(id string) error {
		if _, err := body.Seek(off, io.SeekStart); err != nil {
			return err
		}
		sess, err := m.c.ExecSession(ctx, id, client.ExecRequest{
			Command: m.invoke("fs", "write", t.root, t.abs, strconv.FormatUint(uint64(mode.Perm()), 8)),
			Timeout: maxSession.String(),
		})
		if err != nil {
			return err
		}
		w := &sender{s: sess}
		sent := make(chan error, 1)
		go func() { sent <- w.input(body) }()
		out, rerr := readAll(sess, 1<<16)
		code, werr := sess.Wait()
		ierr := <-sent
		cerr := closeSession(sess)
		resp, derr := decodeResponse(out)
		switch {
		case derr != nil && (code == 126 || code == 127):
			return fmt.Errorf("%w: exit %d: %s", errNoHelper, code, truncate(out))
		case derr != nil:
			return errors.Join(derr, rerr, werr)
		case resp.Error != nil:
			return resp.Error.err("write", t.abs)
		}
		return errors.Join(rerr, werr, ierr, cerr)
	})
}

// awake makes sure the sandbox runs before a call whose body cannot be
// sent twice.
func (m *Machine) awake(ctx context.Context) error {
	id, err := m.usable()
	if err != nil {
		return err
	}
	sb, _, err := m.c.GetSandbox(ctx, id)
	switch {
	case client.CodeOf(err) == "not_found":
		return m.markLost()
	case err != nil:
		return fmt.Errorf("machine: read the sandbox %s: %w", m.name, err)
	case sb.Status.Phase != "Running":
		return m.wakeUp(ctx, id)
	}
	return nil
}

// ImportTar extracts a tar stream below dest, a directory in the
// workspace; the runner delivers repositories (spec 019) and restores
// checkpoints (spec 034) with it.
func (m *Machine) ImportTar(ctx context.Context, dest string, r io.Reader) error {
	t, err := m.resolve(dest)
	if err != nil {
		return err
	}
	if !t.routes {
		return fmt.Errorf("machine: %s is outside the sandbox's workspace, where archives are extracted", t.abs)
	}
	if err := m.awake(ctx); err != nil {
		return err
	}
	return routeErr("import", t.abs, m.call(ctx, false, func(id string) error {
		return m.c.ImportTar(ctx, id, t.abs, r)
	}))
}

// ExportTar streams the paths, each in the workspace, as one tar archive;
// a failure after the first byte ends the stream with an error.
func (m *Machine) ExportTar(ctx context.Context, paths []string) (io.ReadCloser, error) {
	abs := make([]string, 0, len(paths))
	for _, p := range paths {
		t, err := m.resolve(p)
		if err != nil {
			return nil, err
		}
		if !t.routes {
			return nil, fmt.Errorf("machine: %s is outside the sandbox's workspace, which archives hold", t.abs)
		}
		abs = append(abs, t.abs)
	}
	var ts *client.TarStream
	err := m.call(ctx, true, func(id string) error {
		var err error
		ts, err = m.c.ExportTar(ctx, id, abs)
		return err
	})
	if err != nil {
		return nil, routeErr("export", strings.Join(abs, " "), err)
	}
	return tarReader{ts}, nil
}

// tarReader ends an archive with the failure Cella's trailer reported.
type tarReader struct{ ts *client.TarStream }

func (t tarReader) Read(p []byte) (int, error) {
	n, err := t.ts.Read(p)
	if errors.Is(err, io.EOF) {
		if terr := t.ts.Err(); terr != nil {
			return n, fmt.Errorf("machine: the archive: %w", terr)
		}
	}
	return n, err
}

func (t tarReader) Close() error { return t.ts.Close() }
