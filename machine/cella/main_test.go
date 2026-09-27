// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// helperBin is the topos-machine helper built for the machine the tests
// run on, which is where the stub Cella runs its sandboxes' commands.
var helperBin []byte

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "topos-machine-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "the go command builds the helper:", err)
		return 1
	}
	out := filepath.Join(dir, "topos-machine")
	cmd := exec.Command(goTool, "build", "-o", out, "latere.ai/x/topos/cmd/topos-machine")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build the helper: %v\n%s", err, b)
		return 1
	}
	if helperBin, err = os.ReadFile(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}
