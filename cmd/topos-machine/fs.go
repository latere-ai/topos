// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fileOp is one file operation on a path outside the sandbox's workspace,
// such as the spill directory, which Cella's file routes do not reach.
// Each opens its root with os.Root, so a symlink or ".." cannot leave it,
// and answers as the host machine's operation of the same name does.
func fileOp(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 3 {
		return fail(stderr, "fs <op> <root> <path> [<arg>]")
	}
	op, root, p, rest := args[0], args[1], args[2], args[3:]
	want := map[string]int{"stat": 0, "list": 0, "remove": 0, "read": 0, "rename": 1, "write": 1}
	n, ok := want[op]
	if !ok || len(rest) != n {
		return fail(stderr, "fs stat|list|remove|read <root> <path>, fs rename <root> <from> <to>, fs write <root> <path> <mode>")
	}
	r, rel, err := rooted(root, p)
	if err != nil {
		if op == "read" {
			return sent((&frameWriter{w: stdout}).json(frameFail, failure(err)))
		}
		return answer(stdout, response{Error: failure(err)})
	}
	abs := path.Join(root, rel)
	var res response
	switch op {
	case "stat":
		res, err = stat(r, rel, abs)
	case "list":
		res, err = list(r, rel, abs)
	case "remove":
		res, err = remove(r, rel, abs)
	case "rename":
		res, err = rename(r, root, rel, abs, rest[0])
	case "write":
		res, err = write(r, rel, abs, rest[0], stdin)
	case "read":
		err = read(ctx, r, rel, abs, stdout)
		return sent(errors.Join(err, r.Close()))
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	return answer(stdout, res)
}

func info(abs string, fi fs.FileInfo) entry {
	return entry{Path: abs, Size: fi.Size(), Mode: uint32(fi.Mode()), ModTime: fi.ModTime(), IsDir: fi.IsDir()}
}

func stat(r *os.Root, rel, abs string) (response, error) {
	fi, err := r.Stat(rel)
	if err != nil {
		return response{}, err
	}
	return response{Entries: []entry{info(abs, fi)}}, nil
}

// list answers a directory's entries, sorted by name, without the ones
// the deny-list names and the ones that vanished while it was read.
func list(r *os.Root, rel, abs string) (response, error) {
	f, err := r.Open(rel)
	if err != nil {
		return response{}, err
	}
	entries, rerr := f.ReadDir(-1)
	if err := errors.Join(rerr, f.Close()); err != nil {
		return response{}, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		ea := path.Join(abs, e.Name())
		if deny.Path(ea) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, info(ea, fi))
	}
	// An empty directory still answers entries, as an empty list.
	return response{Entries: out, Done: true}, nil
}

// remove removes a file or an empty directory; the root itself stays.
func remove(r *os.Root, rel, abs string) (response, error) {
	if rel == "." {
		return response{}, fmt.Errorf("machine: %s is a root and is not removed", abs)
	}
	if err := r.Remove(rel); err != nil {
		return response{}, err
	}
	return response{Done: true}, nil
}

func rename(r *os.Root, root, rel, abs, to string) (response, error) {
	if !path.IsAbs(to) {
		to = path.Join(root, to)
	}
	to = path.Clean(to)
	relTo, ok := within(root, to)
	if !ok {
		return response{}, fmt.Errorf("machine: %s and %s are in different roots", abs, to)
	}
	if deny.Path(to) {
		return response{}, &fs.PathError{Op: "rename", Path: to, Err: fs.ErrPermission}
	}
	if err := r.Rename(rel, relTo); err != nil {
		return response{}, err
	}
	return response{Done: true}, nil
}

// write writes the input frames through a temporary file in the same
// directory and renames it over the path, creating the parent
// directories. A mode of zero keeps an existing file's permissions, or is
// 0644. The input is framed because the exec socket cannot end it.
func write(r *os.Root, rel, abs, modeArg string, stdin io.Reader) (response, error) {
	if rel == "." {
		return response{}, &fs.PathError{Op: "write", Path: abs, Err: syscall.EISDIR}
	}
	m, err := strconv.ParseUint(modeArg, 8, 32)
	if err != nil || m > 0o777 {
		return response{}, fmt.Errorf("machine: the mode %q is not octal permissions", modeArg)
	}
	mode := fs.FileMode(m)
	if dir := path.Dir(rel); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return response{}, err
		}
	}
	if mode == 0 {
		mode = 0o644
		if fi, err := r.Stat(rel); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	tmp := rel + ".topos-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".tmp"
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return response{}, err
	}
	werr := copyFrames(f, stdin)
	if err := errors.Join(werr, f.Close()); err != nil {
		return response{}, errors.Join(fmt.Errorf("machine: write %s: %w", abs, err), r.Remove(tmp))
	}
	if err := r.Rename(tmp, rel); err != nil {
		return response{}, errors.Join(err, r.Remove(tmp))
	}
	return response{Done: true}, nil
}

// copyFrames copies input frames to w until the end frame. A kill frame
// or an input that ends first abandons the write.
func copyFrames(w io.Writer, stdin io.Reader) error {
	for {
		kind, payload, err := readFrame(stdin)
		if err != nil {
			return fmt.Errorf("the input ended before its end frame: %w", err)
		}
		switch kind {
		case frameInput:
			if _, err := w.Write(payload); err != nil {
				return err
			}
		case frameEOF:
			return nil
		case frameKill:
			return errors.New("the write was canceled")
		}
	}
}

// read streams a file: a start frame once it is open, its bytes in output
// frames, and an exit frame at its end, or a failure frame at any point.
func read(ctx context.Context, r *os.Root, rel, abs string, stdout io.Writer) error {
	out := &frameWriter{w: stdout}
	f, err := r.Open(rel)
	if err != nil {
		return out.json(frameFail, failure(err))
	}
	fi, err := f.Stat()
	if err == nil && fi.IsDir() {
		err = &fs.PathError{Op: "read", Path: abs, Err: syscall.EISDIR}
	}
	if err != nil {
		return errors.Join(out.json(frameFail, failure(err)), f.Close())
	}
	if err := out.frame(frameStart, nil); err != nil {
		return errors.Join(err, f.Close())
	}
	buf := make([]byte, chunk)
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(out.json(frameFail, failure(err)), f.Close())
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := out.frame(frameOutput, buf[:n]); err != nil {
				return errors.Join(err, f.Close())
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return errors.Join(out.json(frameFail, failure(rerr)), f.Close())
		}
	}
	if err := f.Close(); err != nil {
		return out.json(frameFail, failure(err))
	}
	return out.json(frameExit, exitBody{})
}
