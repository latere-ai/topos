// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ScriptArgMax is the longest script a machine hands its shell as the
// -c argument. Linux refuses any one argument past 128 KiB
// (MAX_ARG_STRLEN) with E2BIG whatever the total, and a command the
// model writes, a long heredoc among them, can pass that, so a longer
// script is handed to the shell as a file instead.
const ScriptArgMax = 64 << 10

// ShellArgs are the arguments after the shell's path that run script:
// -c and the script, or, past ScriptArgMax, the path of a file in dir
// that holds it, which the shell reads as its script. remove deletes
// that file once the shell has exited and is a no-op for the -c form.
// The two forms differ only in $0, which a file script sees as its
// path.
func ShellArgs(dir, script string) (args []string, remove func() error, err error) {
	if len(script) <= ScriptArgMax {
		return []string{"-c", script}, func() error { return nil }, nil
	}
	f, err := os.CreateTemp(dir, "topos-script-*.sh")
	if err != nil {
		return nil, nil, fmt.Errorf("machine: create the script file: %w", err)
	}
	name := f.Name()
	remove = func() error {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("machine: remove the script file: %w", err)
		}
		return nil
	}
	_, werr := f.WriteString(script)
	if err := errors.Join(werr, f.Close()); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("machine: write the script file: %w", err), remove())
	}
	return []string{name}, remove, nil
}
