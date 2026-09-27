// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"latere.ai/x/topos/machine"
)

// TestTheModuleBuildsForWindows holds every package of the module, the
// Windows process code of this one and the Unix-only code its build
// constraints leave out, to compiling and passing go vet for Windows,
// and links the topos binary the windows/amd64 archive carries. No
// Windows runner is available, so this proves the Windows code
// type-checks against the calls it makes; it does not run it.
func TestTheModuleBuildsForWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the module")
	}
	env := append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	vet := exec.CommandContext(t.Context(), "go", "vet", "latere.ai/x/topos/...")
	vet.Env = env
	if out, err := vet.CombinedOutput(); err != nil {
		t.Fatalf("go vet for windows: %v\n%s", err, out)
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(t.TempDir(), "topos.exe"), "latere.ai/x/topos/cmd/topos")
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build of topos for windows: %v\n%s", err, out)
	}
}

// TestFindShell: a Windows host runs commands under sh on PATH, else
// under the sh.exe beside the git of Git for Windows, whose installer
// puts only its cmd directory on PATH, else refuses with a message
// that names the remedy.
func TestFindShell(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Git")
	git, sh := filepath.Join(root, "cmd", "git.exe"), filepath.Join(root, "bin", "sh.exe")
	on := func(found map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if p, ok := found[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		}
	}
	at := func(p string) func(string) bool { return func(q string) bool { return q == p } }
	for name, c := range map[string]struct {
		look   func(string) (string, error)
		exists func(string) bool
		want   string
	}{
		"sh on PATH":        {on(map[string]string{"sh": "/msys/usr/bin/sh.exe", "git": git}), at(sh), "/msys/usr/bin/sh.exe"},
		"beside git":        {on(map[string]string{"git": git}), at(sh), sh},
		"git without sh":    {on(map[string]string{"git": git}), at(""), ""},
		"neither sh or git": {on(nil), at(sh), ""},
	} {
		got, err := findShell(c.look, c.exists)
		switch {
		case c.want == "" && !errors.Is(err, errNoShell):
			t.Errorf("%s: %q, %v, want the refusal", name, got, err)
		case c.want != "" && (err != nil || got != c.want):
			t.Errorf("%s: %q, %v, want %q", name, got, err, c.want)
		}
	}
}

// TestAHostWithoutASandbox: a host machine given no sandbox records
// none on Windows, which has no mechanism the host sandbox is built on,
// and nothing on Linux and macOS, which have one.
func TestAHostWithoutASandbox(t *testing.T) {
	for goos, want := range map[string]string{"windows": machine.SandboxNone, "darwin": "", "linux": ""} {
		if got := unsandboxed(goos); got != want {
			t.Errorf("unsandboxed(%s) = %q, want %q", goos, got, want)
		}
	}
	if got := open(t).h.Info().Sandbox; got != unsandboxed(runtime.GOOS) {
		t.Fatalf("this host records %q", got)
	}
}

// TestReportedDir reads the directory the exit trap wrote, and no
// directory from a shell that was killed before it could write one.
func TestReportedDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d")
	for body, want := range map[string]string{"": "", "\n": "", dir + "/sub/\n": filepath.Join(dir, "sub")} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := reportedDir(p); err != nil || got != want {
			t.Errorf("reportedDir of %q = %q, %v, want %q", body, got, err, want)
		}
	}
	if _, err := reportedDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("read a missing directory file")
	}
}
