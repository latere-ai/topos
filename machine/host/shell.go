// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"path/filepath"
)

// errNoShell refuses a command on a Windows host that has no POSIX
// shell, which every command and the bash tool's contract assume.
var errNoShell = errors.New("machine: no POSIX shell runs commands on this host: install Git for Windows, whose sh.exe runs them, or put an sh.exe on PATH")

// findShell finds the POSIX shell of a Windows host: sh on PATH, else
// the sh.exe of the Git for Windows whose git is on PATH. Its installer
// puts only <root>\cmd on PATH, which holds git.exe, and keeps sh.exe
// in <root>\bin beside it. look is exec.LookPath and exists reports a
// regular file; both are parameters, so the search is tested on any
// host.
func findShell(look func(string) (string, error), exists func(string) bool) (string, error) {
	if p, err := look("sh"); err == nil {
		return p, nil
	}
	git, err := look("git")
	if err != nil {
		return "", errNoShell
	}
	if p := filepath.Join(filepath.Dir(filepath.Dir(git)), "bin", "sh.exe"); exists(p) {
		return p, nil
	}
	return "", errNoShell
}
