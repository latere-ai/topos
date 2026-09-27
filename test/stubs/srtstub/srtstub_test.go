// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package srtstub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBin(t *testing.T) {
	dir := Bin(t, Unconfined)
	for _, name := range []string{"srt", "bwrap", "socat", "rg"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Mode().Perm()&0o100 == 0 {
			t.Fatalf("%s: %v, %v", name, fi, err)
		}
	}
	out, err := exec.CommandContext(t.Context(), filepath.Join(dir, "srt"), "--settings", "s.json", "--", "/bin/echo", "ran").Output()
	if err != nil || strings.TrimSpace(string(out)) != "ran" {
		t.Fatalf("the unconfined stand-in: %q, %v", out, err)
	}
	if err := exec.CommandContext(t.Context(), filepath.Join(Bin(t, Broken), "srt"), "--", "/bin/echo").Run(); err == nil {
		t.Fatal("the broken stand-in ran")
	}
}

// unwritable is a test whose temporary directory cannot be written, and
// which records its failure.
type unwritable struct {
	testing.TB
	failed bool
}

func (u *unwritable) TempDir() string       { return "/nonexistent/srtstub" }
func (u *unwritable) Fatal(args ...any)     { u.failed = true }
func (u *unwritable) Helper()               {}
func (u *unwritable) Fatalf(string, ...any) { u.failed = true }

func TestBinFailsTheTestItCannotWrite(t *testing.T) {
	u := &unwritable{TB: t}
	Bin(u, Unconfined)
	if !u.failed {
		t.Fatal("a directory that cannot be written did not fail the test")
	}
}
