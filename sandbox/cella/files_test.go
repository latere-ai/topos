// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"testing"

	"latere.ai/x/topos/sandbox"
)

// tarEntry describes one file/dir the fake export server should emit.
type tarEntry struct {
	name string // tar name, e.g. "src/main.go" or "src/" for a dir
	body string // file content (ignored for dirs)
	dir  bool
}

// exportServer returns a handler that serves the given entries as a tar from
// files/export. It echoes back all entries regardless of the requested paths
// (sufficient for unit tests; the real server filters with tar -C).
func exportServer(t *testing.T, entries []tarEntry) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		tw := tar.NewWriter(w)
		for _, e := range entries {
			hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
			if e.dir {
				hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Errorf("write tar header: %v", err)
			}
			if !e.dir {
				_, _ = tw.Write([]byte(e.body))
			}
		}
		_ = tw.Close()
	}
}

func TestReadFileReturnsEntry(t *testing.T) {
	p := newProvider(t, exportServer(t, []tarEntry{
		{name: "foo/bar.txt", body: "hello world"},
	}))
	data, err := p.ReadFile(context.Background(), "sb_1", "foo/bar.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("data = %q, want hello world", data)
	}
}

func TestReadFileNormalizesLeadingDotSlash(t *testing.T) {
	// The server may prefix entries with "./"; the path is still found.
	p := newProvider(t, exportServer(t, []tarEntry{
		{name: "./notes.md", body: "content"},
	}))
	data, err := p.ReadFile(context.Background(), "sb_1", "notes.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("data = %q, want content", data)
	}
}

func TestReadFileMissingIsNotFound(t *testing.T) {
	// An empty tar (the file does not exist) yields ErrNotFound.
	p := newProvider(t, exportServer(t, nil))
	_, err := p.ReadFile(context.Background(), "sb_1", "nope.txt")
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReadFilePropagatesAPIError(t *testing.T) {
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError, map[string]string{"code": "internal", "message": "boom"})
	}))
	_, err := p.ReadFile(context.Background(), "sb_1", "x")
	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
}

func TestReadFileMalformedTar(t *testing.T) {
	// A body that claims to be a tar but is garbage surfaces a tar error.
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write([]byte("this is not a tar archive at all, just bytes"))
	}))
	_, err := p.ReadFile(context.Background(), "sb_1", "x.txt")
	if err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want a tar parse error", err)
	}
}

func TestListFilesPropagatesError(t *testing.T) {
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "gone"})
	}))
	_, err := p.ListFiles(context.Background(), "sb_1", ".")
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListFilesImmediateChildrenOnly(t *testing.T) {
	// A recursive tar of the root; ListFiles must collapse it to the top level.
	p := newProvider(t, exportServer(t, []tarEntry{
		{name: "./", dir: true},
		{name: "README.md", body: "readme"},
		{name: "src/", dir: true},
		{name: "src/main.go", body: "package main"},
		{name: "src/sub/", dir: true},
		{name: "src/sub/deep.go", body: "deep"},
	}))

	got, err := p.ListFiles(context.Background(), "sb_1", ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	names := fileNames(got)
	want := []string{"README.md", "src"}
	if !equalStrings(names, want) {
		t.Fatalf("children = %v, want %v", names, want)
	}
	for _, fi := range got {
		switch fi.Name {
		case "README.md":
			if fi.IsDir || fi.Size != int64(len("readme")) {
				t.Errorf("README.md = %+v, want file size 6", fi)
			}
		case "src":
			if !fi.IsDir {
				t.Errorf("src = %+v, want dir", fi)
			}
		}
	}
}

func TestListFilesInfersDirFromDescendantWithoutHeader(t *testing.T) {
	// No explicit "src/" header, only a descendant: src must still appear as a dir.
	p := newProvider(t, exportServer(t, []tarEntry{
		{name: "src/main.go", body: "x"},
	}))
	got, err := p.ListFiles(context.Background(), "sb_1", ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(got) != 1 || got[0].Name != "src" || !got[0].IsDir {
		t.Fatalf("children = %+v, want a single dir 'src'", got)
	}
}

func TestListFilesOfSubdir(t *testing.T) {
	p := newProvider(t, exportServer(t, []tarEntry{
		{name: "src/", dir: true},
		{name: "src/main.go", body: "m"},
		{name: "src/util.go", body: "u"},
		{name: "src/sub/inner.go", body: "i"},
	}))
	got, err := p.ListFiles(context.Background(), "sb_1", "src")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	names := fileNames(got)
	want := []string{"main.go", "sub", "util.go"}
	if !equalStrings(names, want) {
		t.Fatalf("children = %v, want %v", names, want)
	}
}

func TestWriteFileImportsOneEntryTar(t *testing.T) {
	var gotName, gotDest string
	var gotBody []byte
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotDest = r.FormValue("dest")
		f, _, err := r.FormFile("tarball")
		if err != nil {
			t.Fatalf("FormFile tarball: %v", err)
		}
		defer f.Close() //nolint:errcheck
		raw, _ := io.ReadAll(f)
		tr := tar.NewReader(bytes.NewReader(raw))
		hdr, err := tr.Next()
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		gotName = hdr.Name
		gotBody, _ = io.ReadAll(tr)
		writeJSON(t, w, http.StatusOK, map[string]string{"status": "imported"})
	}))

	if err := p.WriteFile(context.Background(), "sb_1", "dir/file.txt", []byte("payload")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if gotName != "dir/file.txt" {
		t.Errorf("tar entry name = %q, want dir/file.txt", gotName)
	}
	if string(gotBody) != "payload" {
		t.Errorf("tar entry body = %q, want payload", gotBody)
	}
	if gotDest != "" {
		t.Errorf("dest = %q, want empty (server defaults to /workspace)", gotDest)
	}
}

func TestWriteFileEmptyPath(t *testing.T) {
	p := newProvider(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no request expected for empty path")
	}))
	if err := p.WriteFile(context.Background(), "sb_1", "", []byte("x")); err == nil {
		t.Fatal("empty path: want error, got nil")
	}
}

func TestWriteFilePropagatesError(t *testing.T) {
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "no sandbox"})
	}))
	err := p.WriteFile(context.Background(), "missing", "a.txt", []byte("x"))
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func fileNames(in []sandbox.FileInfo) []string {
	out := make([]string, len(in))
	for i, fi := range in {
		out[i] = fi.Name
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
