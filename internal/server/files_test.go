// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// memFiles is a session's working directory in memory, at /workspace.
type memFiles struct {
	mu    sync.Mutex
	files map[string][]byte
	// opened is the error the reader answers for every session: nil
	// reads files, machine.ErrNotRunning a machine that does not run.
	opened error
	// sessions are the sessions the reader was asked for.
	sessions []string
	// reads counts the ReadFile calls.
	reads int
}

const memWorkdir = "/workspace"

func (m *memFiles) workspaces(_ context.Context, s session.Session) (machine.FileReader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, s.ID)
	if m.opened != nil {
		return nil, m.opened
	}
	return m, nil
}

func (m *memFiles) resolve(p string) (string, error) {
	if !path.IsAbs(p) {
		p = path.Join(memWorkdir, p)
	}
	p = path.Clean(p)
	switch {
	case p != memWorkdir && !strings.HasPrefix(p, memWorkdir+"/"):
		return "", fmt.Errorf("%w: %s", machine.ErrOutside, p)
	case machine.DenyList{}.Path(p):
		return "", fmt.Errorf("%w: %s", machine.ErrDenied, p)
	}
	return p, nil
}

func (m *memFiles) Stat(_ context.Context, p string) (machine.FileInfo, error) {
	a, err := m.resolve(p)
	if err != nil {
		return machine.FileInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.files[a]; ok {
		return machine.FileInfo{Path: a, Size: int64(len(b))}, nil
	}
	for f := range m.files {
		if strings.HasPrefix(f, a+"/") {
			return machine.FileInfo{Path: a, IsDir: true}, nil
		}
	}
	return machine.FileInfo{}, &fs.PathError{Op: "stat", Path: a, Err: fs.ErrNotExist}
}

func (m *memFiles) ReadFile(_ context.Context, p string) (io.ReadCloser, error) {
	a, err := m.resolve(p)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	b, ok := m.files[a]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: a, Err: fs.ErrNotExist}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// filesFixture is the API over a session of alice's whose working
// directory is m.
func filesFixture(t *testing.T, m *memFiles) (*fixture, session.Session) {
	t.Helper()
	f := newFixture(t, func(o *Options) { o.Workspaces = m.workspaces })
	f.apply("alice", "reviewer", "Review.")
	return f, f.create("alice", "reviewer")
}

func fileURL(id, p string) string {
	return "/v1/sessions/" + id + "/files?path=" + url.QueryEscape(p)
}

// TestAFileIsAnsweredAsADownload: a file of the working directory, by
// its absolute or its relative path, is its bytes labeled a download
// that never runs as a page of the API's origin: its media type, its
// length, an attachment disposition with its name, nosniff, a sandboxing
// policy and no-store. HEAD answers the headers alone.
func TestAFileIsAnsweredAsADownload(t *testing.T) {
	page := "<!doctype html><title>A poem</title><script>alert(1)</script>"
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	m := &memFiles{files: map[string][]byte{
		"/workspace/poetry.html":     []byte(page),
		"/workspace/out/chart.png":   []byte(png),
		"/workspace/notes":           []byte("plain words\n"),
		"/workspace/data.bin":        {0, 1, 2, 3},
		"/workspace/Grüße \"x\".txt": []byte("hallo"),
	}}
	f, s := filesFixture(t, m)
	for _, c := range []struct{ path, media, body, name string }{
		{"/workspace/poetry.html", "text/html; charset=utf-8", page, `attachment; filename=poetry.html`},
		{"poetry.html", "text/html; charset=utf-8", page, `attachment; filename=poetry.html`},
		{"out/chart.png", "image/png", png, `attachment; filename=chart.png`},
		{"notes", "text/plain; charset=utf-8", "plain words\n", `attachment; filename=notes`},
		{"data.bin", "application/octet-stream", "\x00\x01\x02\x03", `attachment; filename=data.bin`},
		{"Grüße \"x\".txt", "", "hallo", `attachment; filename*=utf-8''Gr%C3%BC%C3%9Fe%20%22x%22.txt`},
	} {
		a := f.do(http.MethodGet, fileURL(s.ID, c.path), "alice", "")
		if a.status != http.StatusOK || string(a.body) != c.body {
			t.Errorf("%s: %d %q", c.path, a.status, a.body)
			continue
		}
		h := a.header
		if c.media != "" && h.Get("Content-Type") != c.media {
			t.Errorf("%s: Content-Type %q, want %q", c.path, h.Get("Content-Type"), c.media)
		}
		for k, want := range map[string]string{
			"Content-Length": strconv.Itoa(len(c.body)), "Content-Disposition": c.name, "X-Content-Type-Options": "nosniff",
			"Content-Security-Policy": "sandbox; default-src 'none'", "Cache-Control": "no-store",
		} {
			if h.Get(k) != want {
				t.Errorf("%s: %s %q, want %q", c.path, k, h.Get(k), want)
			}
		}
	}
	reads := m.reads
	head := f.do(http.MethodHead, fileURL(s.ID, "poetry.html"), "alice", "")
	if head.status != http.StatusOK || len(head.body) != 0 || head.header.Get("Content-Length") != strconv.Itoa(len(page)) || head.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("HEAD: %d %v %q", head.status, head.header, head.body)
	}
	if m.reads != reads+1 {
		t.Errorf("HEAD read the file %d times", m.reads-reads)
	}
	for _, id := range m.sessions {
		if id != s.ID {
			t.Errorf("the reader was asked for %s", id)
		}
	}
}

// TestAFileReadIsRefused: each refusal of the route, with its code.
func TestAFileReadIsRefused(t *testing.T) {
	m := &memFiles{files: map[string][]byte{
		"/workspace/site/index.html": []byte("<p>"),
		"/workspace/big.txt":         bytes.Repeat([]byte("a"), MaxFileBytes+1),
		"/workspace/.env":            []byte("KEY=x"),
	}}
	f, s := filesFixture(t, m)
	for _, c := range []struct {
		name, url, code string
		status          int
	}{
		{"no path", "/v1/sessions/" + s.ID + "/files", CodeInvalidRequest, http.StatusBadRequest},
		{"an empty path", "/v1/sessions/" + s.ID + "/files?path=", CodeInvalidRequest, http.StatusBadRequest},
		{"two paths", "/v1/sessions/" + s.ID + "/files?path=a&path=b", CodeInvalidRequest, http.StatusBadRequest},
		{"a directory", fileURL(s.ID, "site"), CodeInvalidRequest, http.StatusBadRequest},
		{"outside the working directory", fileURL(s.ID, "../etc/passwd"), CodeInvalidRequest, http.StatusBadRequest},
		{"a credential", fileURL(s.ID, ".env"), CodeInvalidRequest, http.StatusBadRequest},
		{"a missing file", fileURL(s.ID, "missing.txt"), CodeNotFound, http.StatusNotFound},
		{"a file past the bound", fileURL(s.ID, "big.txt"), CodeFileTooLarge, http.StatusRequestEntityTooLarge},
		{"another's session", fileURL(s.ID, "site/index.html") + "&as=bob", CodeNotFound, http.StatusNotFound},
	} {
		token := "alice"
		if strings.HasSuffix(c.url, "&as=bob") {
			token = "bob"
		}
		a := f.do(http.MethodGet, c.url, token, "")
		if a.status != c.status || a.code() != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, a.status, a.body, c.status, c.code)
		}
	}
	var big struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	f.do(http.MethodGet, fileURL(s.ID, "big.txt"), "alice", "").decode(t, &big)
	if big.Error.Details["limit"] != float64(MaxFileBytes) || big.Error.Details["size"] != float64(MaxFileBytes+1) {
		t.Errorf("file_too_large details: %v", big.Error.Details)
	}
	if m.reads != 0 {
		t.Errorf("a refused read read the file %d times", m.reads)
	}
}

// TestAFileOfAMachineThatDoesNotRunIsUnavailable: a machine stopped,
// never opened or gone is file_unavailable, and so is every read of a
// server with no reader; any other failure is the server's.
func TestAFileOfAMachineThatDoesNotRunIsUnavailable(t *testing.T) {
	m := &memFiles{opened: fmt.Errorf("%w: the sandbox is Stopped", machine.ErrNotRunning)}
	f, s := filesFixture(t, m)
	a := f.do(http.MethodGet, fileURL(s.ID, "poetry.html"), "alice", "")
	if a.status != http.StatusConflict || a.code() != CodeFileUnavailable {
		t.Errorf("a stopped machine: %d %s", a.status, a.body)
	}
	m.opened = errors.New("the network is down")
	if a := f.do(http.MethodGet, fileURL(s.ID, "poetry.html"), "alice", ""); a.status != http.StatusInternalServerError || a.code() != CodeInternal {
		t.Errorf("a failed read: %d %s", a.status, a.body)
	}
	g := newFixture(t)
	g.apply("alice", "reviewer", "Review.")
	gs := g.create("alice", "reviewer")
	if a := g.do(http.MethodGet, fileURL(gs.ID, "poetry.html"), "alice", ""); a.status != http.StatusConflict || a.code() != CodeFileUnavailable {
		t.Errorf("a server with no reader: %d %s", a.status, a.body)
	}
}
