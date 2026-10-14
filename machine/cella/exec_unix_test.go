// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cella

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

func TestAHelperThatDoesNotAnswerACancelIsClosed(t *testing.T) {
	f := open(t)
	oldGrace, oldMargin := killGrace, closeMargin
	killGrace, closeMargin = 0, 200*time.Millisecond
	t.Cleanup(func() { killGrace, closeMargin = oldGrace, oldMargin })
	ctx, cancel := context.WithCancel(t.Context())
	st, err := f.m.ExecStream(ctx, machine.ExecRequest{Command: "echo $PPID; sleep 5"})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readUntil(t, st, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	// SIGSTOP holds the helper, so it answers no kill frame.
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})
	cancel()
	res, err := st.Wait()
	if err != nil || !res.Canceled || res.ExitCode != -1 {
		t.Errorf("a helper that did not answer = %+v %v", res, err)
	}
}

// TestAHelperKilledAloneIsAKilledCommand: a command whose helper is
// killed while its sandbox runs on ends as a killed command does, with
// the exit code of a kill and what it wrote before, and says nothing of
// the machine: nothing stopped, and nothing says the machine ran out of
// memory (spec 009).
func TestAHelperKilledAloneIsAKilledCommand(t *testing.T) {
	f := open(t)
	st, err := f.m.ExecStream(t.Context(), machine.ExecRequest{Command: "echo $PPID; sleep 0.3"})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readUntil(t, st, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	res, err := st.Wait()
	if err != nil || res.ExitCode != machine.ExitKilled || res.Stopped != nil || res.Canceled || res.TimedOut {
		t.Fatalf("a command whose helper was killed = %+v %v", res, err)
	}
	if sb, _ := f.stub.Sandbox(f.m.Info().ID); sb.Status.Phase != "Running" || f.stub.Count(cellastub.OpStart) != 0 {
		t.Errorf("the sandbox is %s after %d starts", sb.Status.Phase, f.stub.Count(cellastub.OpStart))
	}
	if err := execErr(t, f.m); err != nil {
		t.Errorf("the next command: %v", err)
	}
}

// TestCommandsAPersonInstalledRunByName: on a sandbox whose image's npm
// directory cannot be written, as on Cella's read-only root filesystem, a
// command installed for the person under HOME, as `pip install` does, and
// one installed into npm's global directory run by name, beside a command
// the image put in its npm directory, in the foreground and as a job.
func TestCommandsAPersonInstalledRunByName(t *testing.T) {
	image := filepath.Join(t.TempDir(), "npm-global")
	tool := filepath.Join(image, "bin", "tool")
	if err := errors.Join(os.MkdirAll(filepath.Dir(tool), 0o755), os.WriteFile(tool, []byte("#!/bin/sh\necho image\n"), 0o755)); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.Chmod(filepath.Dir(tool), 0o555), os.Chmod(image, 0o555)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(os.Chmod(image, 0o755), os.Chmod(filepath.Dir(tool), 0o755)); err != nil {
			t.Error(err)
		}
	})
	f := open(t, func(_ *cellastub.Server, o *Options) { o.Env = map[string]string{"NPM_CONFIG_PREFIX": image} })
	ctx := t.Context()
	install := `mkdir -p "$HOME/.local/bin" &&
printf '#!/bin/sh\necho pip\n' >"$HOME/.local/bin/frompip" &&
printf '#!/bin/sh\necho npm\n' >"$NPM_CONFIG_PREFIX/bin/fromnpm" &&
chmod +x "$HOME/.local/bin/frompip" "$NPM_CONFIG_PREFIX/bin/fromnpm"`
	if res, err := f.m.Exec(ctx, machine.ExecRequest{Command: install}); err != nil || res.ExitCode != 0 {
		t.Fatalf("install: exit %d, output %q, %v", res.ExitCode, res.Output, err)
	}
	res, err := f.m.Exec(ctx, machine.ExecRequest{Command: "frompip; fromnpm; tool"})
	if err != nil || res.ExitCode != 0 || string(res.Output) != "pip\nnpm\nimage\n" {
		t.Errorf("run by name: exit %d, output %q, %v", res.ExitCode, res.Output, err)
	}
	res, err = f.m.Exec(ctx, machine.ExecRequest{Command: "frompip; fromnpm; tool", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("pip\nnpm\nimage\n\n[job %d exited with code 0]\n", res.PID)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		rc, err := f.m.ReadFile(ctx, res.Log)
		if err != nil {
			t.Fatal(err)
		}
		b, rerr := io.ReadAll(rc)
		if err := errors.Join(rerr, rc.Close()); err != nil {
			t.Fatal(err)
		}
		if string(b) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job log = %q, want %q", b, want)
		}
	}
}
