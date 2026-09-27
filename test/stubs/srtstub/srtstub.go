// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package srtstub writes stand-ins for srt, the program behind the host
// sandbox, and for the programs the sandbox's preflight checks for, so
// a test drives a server's start-up checks on a machine without srt.
// No stand-in confines anything.
package srtstub

import (
	"os"
	"path/filepath"
	"testing"
)

// The behaviors of the stand-in srt.
const (
	// Unconfined drops `--settings <file> --` and runs the command, as a
	// sandbox that launches and confines nothing would.
	Unconfined = "#!/bin/sh\nshift 3\nexec \"$@\"\n"
	// Broken runs nothing and fails, as Bubblewrap does in a container
	// without user namespaces.
	Broken = "#!/bin/sh\necho 'bwrap: No permissions to creating new namespace' >&2\nexit 1\n"
)

// Bin returns a directory holding srt with the given script and every
// program the preflight checks for on either platform: bwrap, socat
// and rg, each of which only exits zero.
func Bin(t testing.TB, srt string) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{"srt": srt, "bwrap": "#!/bin/sh\n", "socat": "#!/bin/sh\n", "rg": "#!/bin/sh\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
