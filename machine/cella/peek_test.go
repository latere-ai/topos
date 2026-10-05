// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// peekOptions are the options a reader outside the drive passes: the
// session's alone, with no helper.
func peekOptions(o Options) Options {
	return Options{URL: o.URL, Token: o.Token, Session: o.Session}
}

func TestPeekReadsTheWorkspaceOfARunningSandbox(t *testing.T) {
	f := open(t)
	if err := f.m.WriteFile(t.Context(), "site/poem.html", strings.NewReader("<p>hi</p>"), 0); err != nil {
		t.Fatal(err)
	}
	r, err := Peek(t.Context(), peekOptions(f.o))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"site/poem.html", path.Join(f.ws(), "site/poem.html"), "site/../site/poem.html"} {
		fi, err := r.Stat(t.Context(), p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Size != int64(len("<p>hi</p>")) || fi.IsDir {
			t.Errorf("stat %s = %+v", p, fi)
		}
		rc, err := r.ReadFile(t.Context(), p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		b, err := io.ReadAll(rc)
		if err := errors.Join(err, rc.Close()); err != nil {
			t.Fatal(err)
		}
		if string(b) != "<p>hi</p>" {
			t.Errorf("read %s = %q", p, b)
		}
	}
	if fi, err := r.Stat(t.Context(), "site"); err != nil || !fi.IsDir {
		t.Errorf("stat of a directory = %+v, %v", fi, err)
	}
}

func TestPeekRefusesWhatIsNotTheWorkspaces(t *testing.T) {
	f := open(t)
	// The machine's own write refuses a credential path, so the file is
	// put on the stub's disk behind its back.
	if err := os.WriteFile(filepath.Join(f.stub.Workspace(f.m.Info().ID), ".env"), []byte("KEY=placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Peek(t.Context(), peekOptions(f.o))
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]error{
		"missing.txt":                  fs.ErrNotExist,
		"../outside.txt":               machine.ErrOutside,
		f.m.SpillDir() + "/tool-1.txt": machine.ErrOutside,
		"/etc/passwd":                  machine.ErrOutside,
		".env":                         machine.ErrDenied,
	} {
		if _, err := r.Stat(t.Context(), p); !errors.Is(err, want) {
			t.Errorf("stat %s: %v, want %v", p, err, want)
		}
		if _, err := r.ReadFile(t.Context(), p); !errors.Is(err, want) {
			t.Errorf("read %s: %v, want %v", p, err, want)
		}
	}
}

func TestPeekNeverWakesTheSandbox(t *testing.T) {
	f := open(t)
	id := f.m.Info().ID
	if err := f.m.WriteFile(t.Context(), "a.txt", strings.NewReader("a"), 0); err != nil {
		t.Fatal(err)
	}
	// A sandbox stopped for idleness stays stopped.
	f.stub.Stop(id)
	if _, err := Peek(t.Context(), peekOptions(f.o)); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("peek at a stopped sandbox: %v", err)
	}
	if sb, _ := f.stub.Sandbox(id); sb.Status.Phase != "Stopped" {
		t.Errorf("peek left the sandbox %s", sb.Status.Phase)
	}
	if n := f.stub.Count(cellastub.OpStart); n != 0 {
		t.Errorf("peek started the sandbox %d times", n)
	}
	// A session whose sandbox was never made has none.
	o := peekOptions(options(t, f.stub))
	if _, err := Peek(t.Context(), o); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("peek at a session with no sandbox: %v", err)
	}
	if n := f.stub.Count(cellastub.OpCreate); n != 1 {
		t.Errorf("peek created a sandbox: %d creates", n)
	}
}

func TestPeekTellsASandboxThatStoppedMidRead(t *testing.T) {
	f := open(t)
	id := f.m.Info().ID
	if err := f.m.WriteFile(t.Context(), "a.txt", strings.NewReader("a"), 0); err != nil {
		t.Fatal(err)
	}
	r, err := Peek(t.Context(), peekOptions(f.o))
	if err != nil {
		t.Fatal(err)
	}
	f.stub.Stop(id)
	if _, err := r.ReadFile(t.Context(), "a.txt"); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("read of a stopped sandbox: %v", err)
	}
	f.stub.Remove(id)
	if _, err := r.Stat(t.Context(), "a.txt"); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("stat of a deleted sandbox: %v", err)
	}
}

func TestPeekRefusals(t *testing.T) {
	stub := cellastub.New(t)
	o := peekOptions(options(t, stub))
	if _, err := Peek(t.Context(), Options{Session: o.Session}); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("no URL: %v", err)
	}
	if _, err := Peek(t.Context(), Options{URL: o.URL, Session: "nope"}); err == nil {
		t.Error("a bad session id was taken")
	}
	stub.RequireToken("other")
	if _, err := Peek(t.Context(), o); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a refused find: %v", err)
	}
}
