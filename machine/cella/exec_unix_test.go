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
