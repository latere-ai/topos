// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellArgsRunsAScriptPastTheArgumentLimit: a script longer than one
// argument may be runs through a file, removed afterwards, and a short
// one stays the -c argument.
func TestShellArgsRunsAScriptPastTheArgumentLimit(t *testing.T) {
	dir := t.TempDir()
	short, remove, err := ShellArgs(dir, "echo short")
	if err != nil || len(short) != 2 || short[0] != "-c" || remove() != nil {
		t.Fatalf("a short script: %v, %v", short, err)
	}
	long := "cd /\necho start\n" + strings.Repeat(": padding\n", 16<<10) + "echo end\n"
	args, remove, err := ShellArgs(dir, long)
	if err != nil || len(args) != 1 {
		t.Fatalf("a long script: %v, %v", args, err)
	}
	out, err := exec.CommandContext(t.Context(), "/bin/sh", args...).CombinedOutput()
	if err != nil || string(out) != "start\nend\n" {
		t.Fatalf("the long script ran: %q, %v", out, err)
	}
	if err := remove(); err != nil {
		t.Fatal(err)
	}
	if err := remove(); err != nil {
		t.Fatalf("a second remove: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
	if _, _, err := ShellArgs(filepath.Join(dir, "missing"), long); err == nil {
		t.Fatal("wrote a script into a missing directory")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, _, err := ShellArgs(dir, long); err == nil && os.Geteuid() != 0 {
		t.Fatal("wrote a script into a read-only directory")
	}
}
