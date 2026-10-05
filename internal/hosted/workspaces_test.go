// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// readAll reads a file through r.
func readAll(t *testing.T, r machine.FileReader, p string) string {
	t.Helper()
	rc, err := r.ReadFile(t.Context(), p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	b, err := io.ReadAll(rc)
	if err := errors.Join(err, rc.Close()); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWorkspacesReadARunningSandboxWithTheSessionsToken: the API reads a
// Cella session's file with the session's own Cella token, the one its
// drive presents, and with the installation's bearer when the
// installation mints none; a stopped sandbox is not running and stays
// stopped.
func TestWorkspacesReadARunningSandboxWithTheSessionsToken(t *testing.T) {
	creds := &issued{life: 15 * time.Minute}
	f := newCredentialFixture(t, helper(t), creds, client.StaticToken("installation-bearer"))
	ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil))
	cfg, err := open(ctx, f.h, f.s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
			t.Error(err)
		}
	}()
	if err := cfg.Machine.WriteFile(ctx, "poem.html", strings.NewReader("<p>a poem</p>"), 0); err != nil {
		t.Fatal(err)
	}
	mint := func(ctx context.Context, _, audience, workload string) (runner.Credential, error) {
		return creds.Credential(ctx, audience, workload)
	}
	before := len(f.cella.Requests())
	read := Workspaces(WorkspaceOptions{CellaURL: f.cella.URL(), CellaToken: client.StaticToken("installation-bearer"), Mint: mint})
	r, err := read(t.Context(), f.s)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, r, "poem.html"); got != "<p>a poem</p>" {
		t.Fatalf("read %q", got)
	}
	for _, rq := range f.cella.Requests()[before:] {
		if !strings.HasPrefix(rq.Header.Get("Authorization"), "Bearer cella-session-") {
			t.Fatalf("%s %s carried %q", rq.Method, rq.Path, rq.Header.Get("Authorization"))
		}
	}

	// An installation that mints no Cella token presents its bearer.
	before = len(f.cella.Requests())
	unminted := func(context.Context, string, string, string) (runner.Credential, error) {
		return runner.Credential{}, runner.ErrNotMinted
	}
	r, err = Workspaces(WorkspaceOptions{CellaURL: f.cella.URL(), CellaToken: client.StaticToken("installation-bearer"), Mint: unminted})(t.Context(), f.s)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, r, "poem.html")
	for _, rq := range f.cella.Requests()[before:] {
		if rq.Header.Get("Authorization") != "Bearer installation-bearer" {
			t.Fatalf("%s %s carried %q", rq.Method, rq.Path, rq.Header.Get("Authorization"))
		}
	}

	// A sandbox stopped for idleness is not running, and is not woken.
	f.cella.Stop(cella.SandboxName(f.s.ID))
	if _, err := read(t.Context(), f.s); !errors.Is(err, machine.ErrNotRunning) {
		t.Fatalf("a stopped sandbox: %v", err)
	}
	if sb, _ := f.cella.Sandbox(cella.SandboxName(f.s.ID)); sb.Status.Phase != "Stopped" {
		t.Fatalf("the read left the sandbox %s", sb.Status.Phase)
	}
}

func TestWorkspacesRefuseWhatTheyCannotReach(t *testing.T) {
	s := session.Session{ID: session.NewID(session.PrefixSession), Machine: session.Machine{Kind: session.MachineCella}}
	if _, err := Workspaces(WorkspaceOptions{})(t.Context(), s); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("no Cella: %v", err)
	}
	// A mint that fails is the read's failure, not a bearer's fallback.
	failing := func(context.Context, string, string, string) (runner.Credential, error) {
		return runner.Credential{}, errors.New("the identity provider is down")
	}
	if _, err := Workspaces(WorkspaceOptions{CellaURL: "http://127.0.0.1:1", Mint: failing})(t.Context(), s); err == nil || errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("a failed mint: %v", err)
	}
	s.Machine.Kind = session.MachineHost
	if _, err := Workspaces(WorkspaceOptions{})(t.Context(), s); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("no host sessions: %v", err)
	}
	if _, err := Workspaces(WorkspaceOptions{DataDir: t.TempDir()})(t.Context(), s); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("a host session that never ran here: %v", err)
	}
}

// TestWorkspacesReadAHostSessionsDirectory: a host session's file is read
// from its working directory by an absolute or a relative path, and
// nothing outside it is, through a symlink or a credential name.
func TestWorkspacesReadAHostSessionsDirectory(t *testing.T) {
	data := t.TempDir()
	s := session.Session{ID: session.NewID(session.PrefixSession), Machine: session.Machine{Kind: session.MachineHost}}
	work, _, _, err := hostDirs(data, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(data, "secret.txt")
	for p, body := range map[string]string{
		filepath.Join(work, "site", "index.html"): "<h1>hi</h1>",
		filepath.Join(work, ".env"):               "KEY=value",
		outside:                                   "secret",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(work, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := Workspaces(WorkspaceOptions{DataDir: data})(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"site/index.html", filepath.Join(real, "site", "index.html")} {
		if got := readAll(t, r, p); got != "<h1>hi</h1>" {
			t.Errorf("read %s = %q", p, got)
		}
		fi, err := r.Stat(t.Context(), p)
		if err != nil || fi.Size != int64(len("<h1>hi</h1>")) || fi.IsDir || fi.Path != filepath.Join(real, "site", "index.html") {
			t.Errorf("stat %s = %+v, %v", p, fi, err)
		}
	}
	if fi, err := r.Stat(t.Context(), "site"); err != nil || !fi.IsDir {
		t.Errorf("stat of a directory = %+v, %v", fi, err)
	}
	for p, want := range map[string]error{
		"missing.txt":    fs.ErrNotExist,
		"../secret.txt":  machine.ErrOutside,
		outside:          machine.ErrOutside,
		"link.txt":       machine.ErrOutside,
		".env":           machine.ErrDenied,
		"site/../../x":   machine.ErrOutside,
		"/etc/passwd":    machine.ErrOutside,
		"site/../.env.1": machine.ErrDenied,
	} {
		if _, err := r.Stat(t.Context(), p); !errors.Is(err, want) {
			t.Errorf("stat %s: %v, want %v", p, err, want)
		}
		if _, err := r.ReadFile(t.Context(), p); !errors.Is(err, want) {
			t.Errorf("read %s: %v, want %v", p, err, want)
		}
	}
}
