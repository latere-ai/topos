// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cella

import (
	"context"
	"errors"
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
