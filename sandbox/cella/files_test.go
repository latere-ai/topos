// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"errors"
	"slices"
	"testing"

	cellaclient "latere.ai/x/cella/client"

	"latere.ai/x/topos/sandbox"
)

// TestFilesRoundTrip reaches the one-file routes with relative paths resolved
// under the workspace, which is how the harness's file tools address them.
func TestFilesRoundTrip(t *testing.T) {
	f, p, id := running(t)
	if err := p.WriteFile(t.Context(), id, "notes/todo.txt", []byte("ship it\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f.mu.Lock()
	stored := string(f.files["/workspace/notes/todo.txt"])
	f.mu.Unlock()
	if stored != "ship it\n" {
		t.Fatalf("stored = %q at /workspace/notes/todo.txt", stored)
	}
	data, err := p.ReadFile(t.Context(), id, "./notes/todo.txt")
	if err != nil || string(data) != "ship it\n" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	data, err = p.ReadFile(t.Context(), id, "/workspace/notes/todo.txt")
	if err != nil || string(data) != "ship it\n" {
		t.Fatalf("ReadFile absolute = %q, %v", data, err)
	}
}

func TestReadFileMissingIsNotFound(t *testing.T) {
	_, p, id := running(t)
	if _, err := p.ReadFile(t.Context(), id, "absent.txt"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("ReadFile = %v, want ErrNotFound", err)
	}
}

func TestWriteFileEmptyPathSendsNothing(t *testing.T) {
	f, p, id := running(t)
	before := len(f.requestLog())
	if err := p.WriteFile(t.Context(), id, "", []byte("x")); err == nil {
		t.Fatal("WriteFile with an empty path succeeded")
	}
	if len(f.requestLog()) != before {
		t.Error("a request was sent")
	}
}

func TestWriteFileMissingSandbox(t *testing.T) {
	_, p, _ := running(t)
	if err := p.WriteFile(t.Context(), "sbx_missing", "a.txt", []byte("x")); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("WriteFile = %v, want ErrNotFound", err)
	}
}

// TestListFilesMapsEntries: the root and a subdirectory, the octal mode read
// into permission bits, and a directory's size left at zero.
func TestListFilesMapsEntries(t *testing.T) {
	f, p, id := running(t)
	f.dirs["/workspace"] = []cellaclient.FileEntry{
		{Name: "src", Path: "/workspace/src", Size: 4096, Mode: "0755", IsDir: true},
		{Name: "go.mod", Path: "/workspace/go.mod", Size: 42, Mode: "0644"},
	}
	f.dirs["/workspace/src"] = []cellaclient.FileEntry{{Name: "main.go", Size: 7, Mode: "0600"}}
	got, err := p.ListFiles(t.Context(), id, "")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	want := []sandbox.FileInfo{{Name: "go.mod", Size: 42, Mode: 0o644}, {Name: "src", Mode: 0o755, IsDir: true}}
	if !slices.Equal(got, want) {
		t.Errorf("ListFiles = %+v, want %+v", got, want)
	}
	got, err = p.ListFiles(t.Context(), id, "src")
	if err != nil || !slices.Equal(got, []sandbox.FileInfo{{Name: "main.go", Size: 7, Mode: 0o600}}) {
		t.Errorf("ListFiles src = %+v, %v", got, err)
	}
}

func TestListFilesErrors(t *testing.T) {
	f, p, id := running(t)
	if _, err := p.ListFiles(t.Context(), id, "absent"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("ListFiles missing = %v, want ErrNotFound", err)
	}
	f.dirs["/workspace/odd"] = []cellaclient.FileEntry{{Name: "x", Mode: "rw-r--r--"}}
	if _, err := p.ListFiles(t.Context(), id, "odd"); err == nil {
		t.Error("ListFiles accepted a mode that is not octal")
	}
}
