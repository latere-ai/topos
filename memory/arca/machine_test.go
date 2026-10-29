// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

const owner = "https://issuer.example|ada"

// fallback is a session's machine that records what reaches it.
type fallback struct {
	machine.Machine
	opened int
	wrote  []string
}

func (f *fallback) SpillDir() string { return "/spill" }

func (f *fallback) Open(context.Context) error { f.opened++; return nil }

func (f *fallback) WriteFile(_ context.Context, p string, _ io.Reader, _ fs.FileMode) error {
	f.wrote = append(f.wrote, p)
	return nil
}

func (f *fallback) Stat(_ context.Context, p string) (machine.FileInfo, error) {
	return machine.FileInfo{Path: p}, nil
}

// folders are a read-only user folder and a writable project folder at
// the stub.
func folders(arca *arcastub.Server) []session.MemoryFolder {
	return []session.MemoryFolder{
		{Name: "user", URL: arca.URL(), Owner: owner, Path: "files/memory/you", Access: session.MemoryReadOnly},
		{Name: "project", URL: arca.URL(), Owner: owner, Path: "files/memory/projects/p1", Access: session.MemoryReadWrite},
	}
}

func newMachine(t *testing.T, arca *arcastub.Server, fb machine.Machine) *Machine {
	t.Helper()
	m, err := New(folders(arca), func(context.Context) (string, error) { return "session-token", nil }, arca.Client(), fb)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func readAll(t *testing.T, m *Machine, p string) (string, error) {
	t.Helper()
	rc, err := m.ReadFile(t.Context(), p)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func write(t *testing.T, m *Machine, p, content string) error {
	t.Helper()
	return m.WriteFile(t.Context(), p, strings.NewReader(content), 0)
}

// last is the stub's last request of op.
func last(t *testing.T, arca *arcastub.Server, op string) arcastub.Recorded {
	t.Helper()
	rs := arca.Requests()
	for _, r := range slices.Backward(rs) {
		if r.Op == op {
			return r
		}
	}
	t.Fatalf("no %s request among %d", op, len(rs))
	return arcastub.Recorded{}
}

// TestTheMemoryMachine: over the files store stub, a read keeps the ETag
// it was answered; a new file is written If-None-Match: * and an
// overwrite If-Match on the kept ETag; a 412 is memory.ErrChanged and
// overwrites nothing; an empty write forgets the file with If-Match; a
// rename and a command are refused; a read_only folder refuses a change
// before any request; and every request to the files API carries the
// session's token, while the read at a presigned address carries none
// (spec 093).
func TestTheMemoryMachine(t *testing.T) {
	arca := arcastub.New(t)
	arca.KeepTrash()
	arca.RequireToken(func(tok string) bool { return tok == "session-token" })
	fb := &fallback{}
	m := newMachine(t, arca, fb)
	ctx := t.Context()
	const project = "files/memory/projects/p1"

	if err := write(t, m, "/memory/project/audience.md", "Builders.\n"); err != nil {
		t.Fatal(err)
	}
	put := last(t, arca, arcastub.OpPut)
	if put.Header.Get("If-None-Match") != "*" || put.Header.Get("If-Match") != "" || put.Header.Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("a new file was put with %v", put.Header)
	}
	if o, ok := arca.Object(owner, project+"/audience.md"); !ok || string(o.Data) != "Builders.\n" || o.Token != "session-token" {
		t.Fatalf("the stub holds %+v %t", o, ok)
	}

	arca.Put(owner, project+"/trains.md", arcastub.Object{Data: []byte("The 8:12.\n")})
	if got, err := readAll(t, m, "/memory/project/trains.md"); err != nil || got != "The 8:12.\n" {
		t.Fatalf("read %q, %v", got, err)
	}
	if err := write(t, m, "/memory/project/trains.md", "The 8:42.\n"); err != nil {
		t.Fatal(err)
	}
	if put := last(t, arca, arcastub.OpPut); put.Header.Get("If-Match") != `"`+checksum("The 8:12.\n")+`"` || put.Header.Get("If-None-Match") != "" {
		t.Fatalf("an overwrite was put with %v", put.Header)
	}

	// Another session changes the file after this one wrote it; this
	// one's next write names the version it wrote, and is refused.
	arca.Put(owner, project+"/trains.md", arcastub.Object{Data: []byte("The 9:00.\n")})
	if err := write(t, m, "/memory/project/trains.md", "The 8:42 and the 9:00.\n"); !errors.Is(err, memory.ErrChanged) {
		t.Fatalf("a write over another session's change: %v", err)
	}
	if o, _ := arca.Object(owner, project+"/trains.md"); string(o.Data) != "The 9:00.\n" {
		t.Fatalf("the refused write overwrote the file: %q", o.Data)
	}
	// A file seen absent and then made by another session is refused too.
	if _, err := m.Stat(ctx, "/memory/project/new.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat of a new file: %v", err)
	}
	arca.Put(owner, project+"/new.md", arcastub.Object{Data: []byte("theirs")})
	if err := write(t, m, "/memory/project/new.md", "mine"); !errors.Is(err, memory.ErrChanged) {
		t.Fatalf("a write onto a file another session made: %v", err)
	}

	// An empty write forgets the file it read, with If-Match.
	if _, err := readAll(t, m, "/memory/project/trains.md"); err != nil {
		t.Fatal(err)
	}
	if err := write(t, m, "/memory/project/trains.md", ""); err != nil {
		t.Fatal(err)
	}
	del := last(t, arca, arcastub.OpDelete)
	if del.Header.Get("If-Match") != `"`+checksum("The 9:00.\n")+`"` || del.Query != "" {
		t.Fatalf("the forget was %+v", del)
	}
	if _, ok := arca.Object(owner, project+"/trains.md"); ok {
		t.Fatal("the forgotten file is still there")
	}
	if _, ok := arca.Trashed(owner, project+"/trains.md"); !ok {
		t.Fatal("the forgotten file is not in the space's trash")
	}
	if err := write(t, m, "/memory/project/trains.md", ""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a forget of a file already forgotten: %v", err)
	}
	// A forget of a file never seen asks its version first, and one
	// another session changed since is refused.
	arca.Put(owner, project+"/seen.md", arcastub.Object{Data: []byte("a")})
	if err := m.Remove(ctx, "/memory/project/seen.md"); err != nil {
		t.Fatal(err)
	}
	arca.Put(owner, project+"/raced.md", arcastub.Object{Data: []byte("a")})
	if _, err := readAll(t, m, "/memory/project/raced.md"); err != nil {
		t.Fatal(err)
	}
	arca.Put(owner, project+"/raced.md", arcastub.Object{Data: []byte("b")})
	if err := write(t, m, "/memory/project/raced.md", ""); !errors.Is(err, memory.ErrChanged) {
		t.Fatalf("a forget of a changed file: %v", err)
	}

	// Moves and commands are refused; a read_only folder refuses every
	// change before any request.
	if err := m.Rename(ctx, "/memory/project/a.md", "/memory/project/b.md"); !errors.Is(err, errRename) {
		t.Fatalf("rename: %v", err)
	}
	if _, err := m.Exec(ctx, machine.ExecRequest{Command: "ls"}); !errors.Is(err, errCommands) {
		t.Fatalf("exec: %v", err)
	}
	if _, err := m.ExecStream(ctx, machine.ExecRequest{Command: "ls"}); !errors.Is(err, errCommands) {
		t.Fatalf("exec stream: %v", err)
	}
	before := len(arca.Requests())
	var ro *memory.ReadOnly
	if err := write(t, m, "/memory/user/name.md", "Ada"); !errors.As(err, &ro) || ro.Folder != "user" {
		t.Fatalf("a write to a read_only folder: %v", err)
	}
	if err := m.Remove(ctx, "/memory/user/name.md"); !errors.As(err, &ro) {
		t.Fatalf("a remove in a read_only folder: %v", err)
	}
	if n := len(arca.Requests()); n != before {
		t.Fatalf("a refused change sent %d requests", n-before)
	}

	// The bounds hold both ways, before any request for a write.
	if err := write(t, m, "/memory/project/big.md", strings.Repeat("x", memory.MaxFileBytes+1)); !errors.As(err, new(*memory.TooLarge)) {
		t.Fatalf("a write past the bound: %v", err)
	}
	if n := len(arca.Requests()); n != before {
		t.Fatalf("a write past the bound sent %d requests", n-before)
	}
	arca.Put(owner, project+"/big.md", arcastub.Object{Data: []byte(strings.Repeat("x", memory.MaxFileBytes+1))})
	if _, err := readAll(t, m, "/memory/project/big.md"); !errors.As(err, new(*memory.TooLarge)) {
		t.Fatalf("a read past the bound: %v", err)
	}

	// Paths: /memory and a folder are directories, a path under /memory
	// that names no folder of the session is not there, and a folder
	// itself is not a file.
	for p, dir := range map[string]bool{"/memory": true, "/memory/user": true, "/memory/project/": true} {
		if fi, err := m.Stat(ctx, p); err != nil || fi.IsDir != dir {
			t.Fatalf("stat %s: %+v %v", p, fi, err)
		}
	}
	if fi, err := m.Stat(ctx, "/memory/project/audience.md"); err != nil || fi.IsDir || fi.Size != int64(len("Builders.\n")) || fi.Mode != 0o644 {
		t.Fatalf("stat of a file: %+v %v", fi, err)
	}
	for _, p := range []string{"/memory/other/a.md", "/memory/other"} {
		if _, err := m.Stat(ctx, p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s: %v", p, err)
		}
		if _, err := readAll(t, m, p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read %s: %v", p, err)
		}
	}
	if _, err := readAll(t, m, "/memory/project"); err == nil {
		t.Fatal("a folder read as a file")
	}
	if err := write(t, m, "/memory/project", "x"); err == nil {
		t.Fatal("a folder written as a file")
	}
	root, err := m.List(ctx, "/memory")
	if err != nil || len(root) != 2 || root[0].Path != "/memory/user" || !root[1].IsDir || root[0].Mode != fs.ModeDir|0o555 {
		t.Fatalf("list /memory: %+v %v", root, err)
	}
	arca.Put(owner, project+"/notes/one.md", arcastub.Object{Data: []byte("1")})
	got, err := m.List(ctx, "/memory/project")
	var names []string
	for _, e := range got {
		names = append(names, fmt.Sprintf("%s:%t", e.Path, e.IsDir))
	}
	if err != nil || !slices.Equal(names, []string{"/memory/project/audience.md:false", "/memory/project/big.md:false", "/memory/project/new.md:false", "/memory/project/notes:true", "/memory/project/raced.md:false"}) {
		t.Fatalf("list the folder: %v %v", names, err)
	}
	if got, err := m.List(ctx, "/memory/project/notes"); err != nil || len(got) != 1 || got[0].Path != "/memory/project/notes/one.md" {
		t.Fatalf("list a directory: %+v %v", got, err)
	}
	if _, err := m.List(ctx, "/memory/project/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("list of no directory: %v", err)
	}
	if _, err := m.List(ctx, "/memory/project/audience.md"); err == nil {
		t.Fatal("a file listed as a directory")
	}

	// Every request to the files API carried the session's token; the
	// stub fails the test on a bearer at the bucket, where every read
	// went.
	for _, r := range arca.Requests() {
		if r.Op != "bucket-read" && r.Header.Get("Authorization") != "Bearer session-token" {
			t.Fatalf("%s %s carried %q", r.Method, r.Path, r.Header.Get("Authorization"))
		}
	}

	// A path outside /memory goes to the session's machine, which a spill
	// file of a long result opens.
	if m.SpillDir() != "/spill" || m.Open(ctx) != nil || fb.opened != 1 {
		t.Fatalf("spill dir %q, opened %d", m.SpillDir(), fb.opened)
	}
	if err := write(t, m, "/spill/tool-1.txt", "long"); err != nil || !slices.Equal(fb.wrote, []string{"/spill/tool-1.txt"}) {
		t.Fatalf("a spill file: %v %v", err, fb.wrote)
	}
	if _, err := m.Stat(ctx, "/workspace/a.txt"); err != nil {
		t.Fatalf("stat outside: %v", err)
	}
	if err := m.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if info := m.Info(); info.Kind != memory.Kind || info.Workdir != memory.Root || !slices.Equal(m.Roots(), []string{memory.Root}) || len(m.Folders()) != 2 {
		t.Fatalf("info %+v", info)
	}
	alone, err := New(folders(arca), func(context.Context) (string, error) { return "t", nil }, arca.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if alone.SpillDir() != "" || alone.Open(ctx) != nil {
		t.Fatal("a machine with no session machine has a spill directory")
	}
	if _, err := alone.Stat(ctx, "/workspace/a.txt"); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("outside with no session machine: %v", err)
	}
}

// TestRefusalsAndFailures: a 4xx with the store's envelope is a memory.Refusal
// carrying its message, and the authorizer's reason and words when its
// detail names one; a 5xx, an answer that does not decode, a store that
// does not answer and a token that cannot be had are memory.Unreachable; and a
// HEAD's refusal is read from the same request as a read (spec 093).
func TestRefusalsAndFailures(t *testing.T) {
	arca := arcastub.New(t)
	m := newMachine(t, arca, nil)
	ctx := t.Context()
	arca.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Message: "You do not have permission to do this.", Detail: "memory_off: Memory is off for this person."})
	var r *memory.Refusal
	if err := write(t, m, "/memory/project/a.md", "a"); !errors.As(err, &r) || r.Reason != "memory_off" || r.Detail != "Memory is off for this person." || r.Message != "You do not have permission to do this." || r.Name() != "memory_off" || r.Status != http.StatusForbidden {
		t.Fatalf("a refusal with a reason: %#v", err)
	}
	if !strings.Contains(r.Error(), "forbidden") {
		t.Fatalf("its error %q", r.Error())
	}
	arca.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusRequestEntityTooLarge, Code: "object_too_large", Message: "The object is too large.", Detail: "past the bound"})
	if err := write(t, m, "/memory/project/a.md", "a"); !errors.As(err, &r) || r.Reason != "" || r.Detail != "" || r.Name() != "object_too_large" {
		t.Fatalf("a refusal with no reason: %#v", err)
	}
	arca.Fail(arcastub.OpHead, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden"})
	arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Detail: "memory_off: Memory is off."})
	if _, err := m.Stat(ctx, "/memory/project/a.md"); !errors.As(err, &r) || r.Reason != "memory_off" {
		t.Fatalf("a refused HEAD: %#v", err)
	}
	arca.Fail(arcastub.OpRead, arcastub.Failure{Status: http.StatusServiceUnavailable, Code: "storage_unavailable"})
	var u *memory.Unreachable
	if _, err := readAll(t, m, "/memory/project/a.md"); !errors.As(err, &u) || !strings.Contains(u.Detail, "503") {
		t.Fatalf("a 5xx: %#v", err)
	}
	arca.Fail(arcastub.OpList, arcastub.Failure{Status: http.StatusBadGateway, Code: "x"})
	if _, err := m.List(ctx, "/memory/project"); !errors.As(err, &u) {
		t.Fatalf("a listing's 5xx: %#v", err)
	}

	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Has("list"):
			_, _ = io.WriteString(w, "not json")
		case r.Method == http.MethodGet:
			http.Redirect(w, r, "ftp://elsewhere/x", http.StatusFound)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "not an envelope")
		}
	}))
	t.Cleanup(garbled.Close)
	fs := []session.MemoryFolder{{Name: "project", URL: garbled.URL, Owner: owner, Path: "files/p", Access: session.MemoryReadWrite}}
	g, err := New(fs, func(context.Context) (string, error) { return "t", nil }, garbled.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.WriteFile(ctx, "/memory/project/a.md", strings.NewReader("a"), 0); !errors.As(err, &u) || !strings.Contains(u.Detail, "does not decode") {
		t.Fatalf("a 4xx with no envelope: %#v", err)
	}
	if _, err := g.ReadFile(ctx, "/memory/project/a.md"); !errors.As(err, &u) || !strings.Contains(u.Detail, "no http or https address") {
		t.Fatalf("a redirect to another scheme: %#v", err)
	}
	if _, err := g.List(ctx, "/memory/project"); !errors.As(err, &u) || !strings.Contains(u.Detail, "listing") {
		t.Fatalf("a listing that does not decode: %#v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	d, err := New([]session.MemoryFolder{{Name: "project", URL: gone.URL, Owner: owner, Path: "files/p", Access: session.MemoryReadWrite}}, func(context.Context) (string, error) { return "t", nil }, http.DefaultClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadFile(ctx, "/memory/project/a.md"); !errors.As(err, &u) || u.Unwrap() == nil || !strings.Contains(u.Error(), "could not be reached") {
		t.Fatalf("a store that does not answer: %#v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.ReadFile(canceled, "/memory/project/a.md"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled call: %v", err)
	}
	broken, err := New(folders(arca), func(context.Context) (string, error) { return "", errors.New("no token") }, arca.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.ReadFile(ctx, "/memory/project/a.md"); !errors.As(err, &u) || !strings.Contains(u.Detail, "token") {
		t.Fatalf("no token: %#v", err)
	}
	if _, err := New(nil, func(context.Context) (string, error) { return "", nil }, http.DefaultClient, nil); err == nil {
		t.Fatal("a machine of no folders")
	}
	if _, err := New([]session.MemoryFolder{{Name: "X"}}, func(context.Context) (string, error) { return "", nil }, http.DefaultClient, nil); err == nil {
		t.Fatal("a machine of a folder that breaks a rule")
	}
}

// TestTheMediaTypes: a file is stored as Markdown for .md, as plain text
// for any other text, and as bytes otherwise.
func TestTheMediaTypes(t *testing.T) {
	if mediaType("a.MD", []byte("a")) != "text/markdown; charset=utf-8" || mediaType("a.txt", []byte{0xff}) != "application/octet-stream" || mediaType("a.txt", []byte("a")) != "text/plain; charset=utf-8" {
		t.Error("media types")
	}
}

func checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestADetailNamesAReason: an envelope's detail names the authorizer's
// reason as "<reason>: <words>" and nothing otherwise, and a path in a
// folder is clean only with no empty, "." or ".." segment.
func TestADetailNamesAReason(t *testing.T) {
	if reason, words := splitReason("memory_read_only: Read only here."); reason != "memory_read_only" || words != "Read only here." {
		t.Errorf("a detail with a reason: %q %q", reason, words)
	}
	if reason, words := splitReason("past the bound"); reason != "" || words != "" {
		t.Errorf("a detail with no reason: %q %q", reason, words)
	}
	for _, rel := range []string{"a/./b", "a//b", "../a"} {
		if cleanRel(rel) {
			t.Errorf("%q is clean", rel)
		}
	}
}

// TestOutsideMemory: a path outside /memory is the session machine's, and
// a move or a remove of one goes there; a move into or out of /memory is
// refused.
func TestOutsideMemory(t *testing.T) {
	arca := arcastub.New(t)
	fb := &movable{}
	m := newMachine(t, arca, fb)
	ctx := t.Context()
	if err := m.Rename(ctx, "/workspace/a", "/workspace/b"); err != nil || fb.moved != 1 {
		t.Fatalf("a move outside: %v", err)
	}
	if err := m.Rename(ctx, "/workspace/a", "/memory/project/b"); !errors.Is(err, errRename) {
		t.Fatalf("a move into memory: %v", err)
	}
	if err := m.Remove(ctx, "/workspace/a"); err != nil || fb.removed != 1 {
		t.Fatalf("a remove outside: %v", err)
	}
	if _, err := m.ReadFile(ctx, "/workspace/a"); err != nil {
		t.Fatalf("a read outside: %v", err)
	}
	if _, err := m.List(ctx, "/workspace"); err != nil {
		t.Fatalf("a list outside: %v", err)
	}
	if _, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "/workspace"}); err != nil {
		t.Fatalf("a search outside: %v", err)
	}
	alone := newMachine(t, arca, nil)
	for name, err := range map[string]error{
		"rename": alone.Rename(ctx, "/a", "/b"),
		"remove": alone.Remove(ctx, "/a"),
		"write":  alone.WriteFile(ctx, "/a", strings.NewReader("a"), 0),
	} {
		if !errors.Is(err, machine.ErrOutside) {
			t.Errorf("%s outside with no session machine: %v", name, err)
		}
	}
	if _, err := alone.ReadFile(ctx, "/a"); !errors.Is(err, machine.ErrOutside) {
		t.Errorf("read outside with no session machine: %v", err)
	}
	if _, err := alone.List(ctx, "/a"); !errors.Is(err, machine.ErrOutside) {
		t.Errorf("list outside with no session machine: %v", err)
	}
	if _, err := alone.Search(ctx, machine.SearchRequest{Path: "/a"}); !errors.Is(err, machine.ErrOutside) {
		t.Errorf("search outside with no session machine: %v", err)
	}
	if err := alone.Remove(ctx, "/memory/other/a.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a remove under no folder: %v", err)
	}
}

// movable is a session's machine that counts what reaches it.
type movable struct {
	fallback
	moved, removed int
}

func (f *movable) Rename(context.Context, string, string) error { f.moved++; return nil }

func (f *movable) Remove(context.Context, string) error { f.removed++; return nil }

func (f *movable) ReadFile(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *movable) List(context.Context, string) ([]machine.FileInfo, error) { return nil, nil }

func (f *movable) Search(context.Context, machine.SearchRequest) (machine.SearchResult, error) {
	return machine.SearchResult{}, nil
}
