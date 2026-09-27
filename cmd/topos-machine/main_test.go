// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"latere.ai/x/topos/machine"
)

// tempDir is a temporary directory with its links resolved, so the paths
// the helper reports compare equal to it.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// decode reads the one JSON response a mode wrote.
func decode(t *testing.T, out *bytes.Buffer) response {
	t.Helper()
	var r response
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return r
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"search"}, {"run"}, {"run", "-nope"}, {"job"}, {"job", "-nope"}, {"fs"}, {"fs", "nope", "/", "x"}, {"fs", "stat", "/", "x", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, strings.NewReader(""), &stdout, &stderr); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("%q wrote %q to standard output", args, stdout.String())
		}
	}
	var stderr bytes.Buffer
	run(t.Context(), nil, nil, io.Discard, &stderr)
	if !strings.Contains(stderr.String(), "usage: topos-machine") {
		t.Errorf("usage = %q", stderr.String())
	}
	if code := fail(failingWriter{}, "x"); code != 3 {
		t.Errorf("a usage error that cannot be printed exits %d, want 3", code)
	}
}

// failingWriter refuses every write, as a closed exec socket does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestSum(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(b)
	var stdout bytes.Buffer
	if code := run(t.Context(), []string{"sum"}, nil, &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != hex.EncodeToString(want[:]) {
		t.Errorf("sum = %s, want %x", got, want)
	}
	if code := sum(io.Discard, io.Discard, func() (string, error) { return "", errors.New("no executable") }); code != 2 {
		t.Errorf("no executable: exit %d", code)
	}
	missing := filepath.Join(t.TempDir(), "gone")
	if code := sum(io.Discard, io.Discard, func() (string, error) { return missing, nil }); code != 2 {
		t.Errorf("a missing executable: exit %d", code)
	}
	dir := t.TempDir()
	if code := sum(io.Discard, io.Discard, func() (string, error) { return dir, nil }); code != 2 {
		t.Errorf("a directory: exit %d", code)
	}
	if code := sum(failingWriter{}, io.Discard, os.Executable); code != 3 {
		t.Errorf("an answer that cannot be written: exit %d", code)
	}
}

func TestKindOf(t *testing.T) {
	for err, want := range map[error]string{
		machine.ErrOutside: kindOutside,
		machine.ErrDenied:  kindDenied,
		&fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}:                           kindNotExist,
		&fs.PathError{Op: "open", Path: "x", Err: fs.ErrExist}:                              kindExist,
		&fs.PathError{Op: "open", Path: "x", Err: syscall.ENOTDIR}:                          kindNotDir,
		&fs.PathError{Op: "open", Path: "x", Err: syscall.EISDIR}:                           kindIsDir,
		&fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}:                         kindPermission,
		&fs.PathError{Op: "openat", Path: "x", Err: errors.New("path escapes from parent")}: kindOutside,
		errors.New("anything"): kindOther,
	} {
		if got := kindOf(err); got != want {
			t.Errorf("kindOf(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestFrames(t *testing.T) {
	var b bytes.Buffer
	w := &frameWriter{w: &b}
	if err := w.frame(frameOutput, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.json(frameExit, exitBody{Code: 3}); err != nil {
		t.Fatal(err)
	}
	if err := w.json(frameExit, func() {}); err == nil {
		t.Error("a payload that does not encode was written")
	}
	kind, payload, err := readFrame(&b)
	if err != nil || kind != frameOutput || string(payload) != "hello" {
		t.Errorf("frame = %c %q %v", kind, payload, err)
	}
	kind, payload, err = readFrame(&b)
	if err != nil || kind != frameExit || string(payload) != `{"code":3}` {
		t.Errorf("frame = %c %q %v", kind, payload, err)
	}
	if _, _, err := readFrame(&b); !errors.Is(err, io.EOF) {
		t.Errorf("an empty stream: %v", err)
	}
	huge := []byte{frameInput, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(huge[1:], maxFrame+1)
	if _, _, err := readFrame(bytes.NewReader(huge)); err == nil {
		t.Error("a frame past the bound was read")
	}
	short := []byte{frameInput, 0, 0, 0, 9, 'a'}
	if _, _, err := readFrame(bytes.NewReader(short)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a cut frame: %v", err)
	}
	if code := answer(failingWriter{}, response{Done: true}); code != 3 {
		t.Errorf("an answer that cannot be written: exit %d", code)
	}
	if code := sent(errors.New("x")); code != 3 {
		t.Errorf("sent = %d", code)
	}
}

func TestSearch(t *testing.T) {
	root := tempDir(t)
	writeFile(t, filepath.Join(root, "a.go"), "package a\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(root, "sub", "b.go"), "package b\n// hello\n")
	writeFile(t, filepath.Join(root, "ignored", "c.go"), "hello\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
	writeFile(t, filepath.Join(root, ".env"), "HELLO=secret\n")
	writeFile(t, filepath.Join(root, "sub", "key.pem"), "hello\n")
	query := func(q machine.SearchRequest, args ...string) response {
		t.Helper()
		body, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		if code := run(t.Context(), append([]string{"search"}, args...), bytes.NewReader(body), &stdout, io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return decode(t, &stdout)
	}
	res := query(machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "(?i)hello"}, root)
	if res.Error != nil || res.Result == nil {
		t.Fatalf("grep = %+v", res)
	}
	want := []string{filepath.Join(root, "a.go"), filepath.Join(root, "sub", "b.go")}
	if strings.Join(res.Result.Lines, ",") != strings.Join(want, ",") {
		t.Errorf("grep = %q, want %q: the ignored, the .env and the key file are skipped", res.Result.Lines, want)
	}
	res = query(machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*.go", Path: "sub"}, root)
	if res.Result == nil || len(res.Result.Lines) != 1 || res.Result.Lines[0] != filepath.Join(root, "sub", "b.go") {
		t.Errorf("glob below the root = %+v", res)
	}
	for name, c := range map[string]struct {
		q    machine.SearchRequest
		kind string
	}{
		"outside":     {machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "/"}, kindOutside},
		"denied":      {machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: filepath.Join(root, ".env")}, kindDenied},
		"missing":     {machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: filepath.Join(root, "gone")}, kindNotExist},
		"bad pattern": {machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "("}, kindOther},
	} {
		res := query(c.q, root)
		if res.Error == nil || res.Error.Kind != c.kind {
			t.Errorf("%s: %+v, want the kind %s", name, res.Error, c.kind)
		}
	}
	if res := query(machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*"}, "relative"); res.Error == nil {
		t.Error("a relative root was searched")
	}
	if res := query(machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*"}, filepath.Join(root, "gone")); res.Error == nil || res.Error.Kind != kindNotExist {
		t.Errorf("a missing root: %+v", res.Error)
	}
	var stdout bytes.Buffer
	run(t.Context(), []string{"search", root}, strings.NewReader("{"), &stdout, io.Discard)
	if res := decode(t, &stdout); res.Error == nil || !strings.Contains(res.Error.Message, "the search request") {
		t.Errorf("a request that does not decode: %+v", res.Error)
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		root, p, rel string
		ok           bool
	}{
		{"/w", "/w", ".", true},
		{"/w", "/w/a/b", "a/b", true},
		{"/w", "/wx", "", false},
		{"/", "/a", "a", true},
		{"/", "/", ".", true},
	} {
		rel, ok := within(c.root, c.p)
		if rel != c.rel || ok != c.ok {
			t.Errorf("within(%s, %s) = %q %v", c.root, c.p, rel, ok)
		}
	}
}
