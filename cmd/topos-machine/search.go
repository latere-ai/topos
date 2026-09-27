// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"latere.ai/x/topos/machine"
)

// deny is the credential deny-list inside a sandbox: the base-name
// entries that hold in any root. A sandbox has none of the person's home
// directory, so the home entries name nothing there (spec 018).
var deny = machine.DenyList{}

// search runs one grep or glob over the root: the request is one JSON
// SearchRequest on standard input, read without waiting for the input to
// end, and the answer is one JSON response.
func search(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return fail(stderr, "search <root>")
	}
	var q machine.SearchRequest
	if err := json.NewDecoder(stdin).Decode(&q); err != nil {
		return answer(stdout, response{Error: &errorBody{Kind: kindOther, Message: "machine: the search request: " + err.Error()}})
	}
	r, rel, err := rooted(args[0], q.Path)
	if err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	fsys := denyFS{FS: r.FS(), base: args[0]}
	res, err := machine.SearchFS(ctx, fsys, args[0], rel, q)
	if cerr := r.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	return answer(stdout, response{Result: &res})
}

// rooted opens root, an absolute clean path, and returns p relative to
// it; an empty p is the root. A path outside the root, or one the
// deny-list names, is refused before anything is opened.
func rooted(root, p string) (*os.Root, string, error) {
	if !path.IsAbs(root) || path.Clean(root) != root {
		return nil, "", fmt.Errorf("machine: the root %q is not an absolute clean path", root)
	}
	if p == "" {
		p = root
	}
	if !path.IsAbs(p) {
		p = path.Join(root, p)
	}
	p = path.Clean(p)
	rel, ok := within(root, p)
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", machine.ErrOutside, p)
	}
	if deny.Path(p) {
		return nil, "", fmt.Errorf("%w: %s", machine.ErrDenied, p)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	return r, rel, nil
}

// within returns p relative to root, or false when p is not root or
// below it.
func within(root, p string) (string, bool) {
	if p == root {
		return ".", true
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if rest, ok := strings.CutPrefix(p, prefix); ok {
		return rest, true
	}
	return "", false
}

// denyFS hides the deny-list's paths from a search, as the host machine's
// does.
type denyFS struct {
	fs.FS
	base string
}

func (d denyFS) denied(name string) bool { return deny.Path(path.Join(d.base, name)) }

func (d denyFS) Open(name string) (fs.File, error) {
	if d.denied(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.FS.Open(name)
}

func (d denyFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(d.FS, name)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool {
		return d.denied(path.Join(name, e.Name()))
	}), nil
}
