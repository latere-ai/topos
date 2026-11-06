// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMakeRunStopsWithMake: `make run` execs toposd, so the server is
// make's own child whatever the recipe's shell does with its last
// command, and the SIGTERM make passes its child when it is stopped
// reaches the server. The shell here is bash with an EXIT trap, which
// stays the parent of its last command; without the exec, make's SIGTERM
// ended that shell and left the server running. A stand-in takes
// out/toposd's place and records the signal and its environment.
func TestMakeRunStopsWithMake(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// An issuer key that exists keeps the target from generating one.
	if err := os.WriteFile(filepath.Join(out, "local-issuer.pem"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile, envFile, gotFile := filepath.Join(dir, "pid"), filepath.Join(dir, "env"), filepath.Join(dir, "got")
	stand := "#!/bin/sh\nenv > " + envFile + "\ntrap 'echo term > " + gotFile + "; exit 0' TERM\necho $$ > " + pidFile + "\nwhile :; do sleep 0.1; done\n"
	if err := os.WriteFile(filepath.Join(out, "toposd"), []byte(stand), 0o755); err != nil {
		t.Fatal(err)
	}
	bashEnv := filepath.Join(dir, "env.sh")
	if err := os.WriteFile(bashEnv, []byte("trap true EXIT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// GO=true makes the build a no-op, so the stand-in stays in place.
	cmd := exec.CommandContext(t.Context(), makeBin, "-C", root, "run", "GO=true", "OUT_DIR="+out, "SHELL="+bash)
	cmd.Env = append(os.Environ(), "BASH_ENV="+bashEnv)
	// make's output goes to a file, not a pipe: a server left running
	// would hold a pipe open, and Wait would wait for it.
	logFile := filepath.Join(dir, "make.log")
	logOut, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logOut.Close() }()
	cmd.Stdout, cmd.Stderr = logOut, logOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	said := func() string {
		b, err := os.ReadFile(logFile)
		if err != nil {
			return err.Error()
		}
		return string(b)
	}
	var pid int
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in never started; make said %s", said())
		}
	}
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill the stand-in: %v", err)
		}
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Errorf("make exited 0 on SIGTERM")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server runs on after make was stopped; make said %s", said())
		}
	}
	if b, err := os.ReadFile(gotFile); err != nil || strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("the server did not get SIGTERM: %q, %v", b, err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TOPOS_PUBLIC_URL=http://127.0.0.1:8080\n", "TOPOS_PUBLIC_ADDR=127.0.0.1:8080\n", "TOPOS_INTERNAL_ADDR=127.0.0.1:8081\n", "TOPOS_ADMIN_SUBJECTS=http://127.0.0.1:8080|admin\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("the server's environment lacks %q", want)
		}
	}
}
