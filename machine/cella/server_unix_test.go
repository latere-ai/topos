// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cella

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
)

func TestAServerMovesToTheBackground(t *testing.T) {
	if !machine.SeesServers {
		t.Skip("this system's listening sockets are not read")
	}
	f := open(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	for p := 20000 + int(time.Now().UnixNano()%8000); port == 0; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			port = p
			if err := ln.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	begun := time.Now()
	res, err := f.m.Exec(t.Context(), machine.ExecRequest{
		Command: "echo starting; exec '" + exe + "'", Env: map[string]string{envListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))},
		Timeout: time.Minute, ReportDir: true, ServerGrace: 200 * time.Millisecond,
	})
	t.Logf("the call returned %s after it started", time.Since(begun))
	if err != nil || !res.Moved || res.PID <= 0 || filepath.Dir(res.Log) != f.m.SpillDir()+"/jobs" || len(res.Ports) != 1 || res.Ports[0] != port ||
		string(res.Output) != "starting\nlistening\n" || res.ServerErr != nil {
		t.Fatalf("exec = %+v %q %v", res, res.Output, err)
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("the server after the move: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		rc, err := f.m.ReadFile(t.Context(), res.Log)
		if err != nil {
			t.Fatal(err)
		}
		b, rerr := io.ReadAll(rc)
		if err := errors.Join(rerr, rc.Close()); err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for deadline := time.Now().Add(10 * time.Second); read() != "accepted\n"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("log = %q", read())
		}
	}
	if err := syscall.Kill(-res.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("accepted\n\n[job %d output ended]\n", res.PID)
	for deadline := time.Now().Add(10 * time.Second); read() != want; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("log = %q, want %q", read(), want)
		}
	}
}

// TestMoveNowSendsTheFrame: closing MoveNow sends the helper a move
// frame, the helper is given a jobs directory with no server grace, and
// its move surfaces as a server's does, with no ports: the command runs
// on in the sandbox with its output in its job log (spec 098).
func TestMoveNowSendsTheFrame(t *testing.T) {
	f := open(t)
	now := make(chan struct{})
	st, err := f.m.ExecStream(t.Context(), machine.ExecRequest{
		Command: "echo started; while :; do echo tick; /bin/sleep 0.05; done", Timeout: time.Minute, ReportDir: true, MoveNow: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := readUntil(t, st, "started\n")
	begun := time.Now()
	close(now)
	rest, rerr := io.ReadAll(st)
	res, err := st.Wait()
	t.Logf("the call returned %s after the move was asked for", time.Since(begun))
	if pid := res.PID; pid > 0 {
		t.Cleanup(func() {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
				t.Error(err)
			}
		})
	}
	out := before + string(rest)
	if rerr != nil || err != nil || !res.Moved || res.PID <= 0 || filepath.Dir(res.Log) != f.m.SpillDir()+"/jobs" || len(res.Ports) != 0 ||
		res.ServerErr != nil || res.Canceled || res.Dir != "" || !strings.HasPrefix(out, "started\n") || strings.Trim(out[len("started\n"):], "tick\n") != "" {
		t.Fatalf("exec = %+v %q %v %v", res, out, rerr, err)
	}
	read := func() string {
		rc, err := f.m.ReadFile(t.Context(), res.Log)
		if err != nil {
			t.Fatal(err)
		}
		b, rerr := io.ReadAll(rc)
		if err := errors.Join(rerr, rc.Close()); err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(read(), "tick\n"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("log = %q", read())
		}
	}
	if err := syscall.Kill(-res.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	ended := fmt.Sprintf("\n[job %d output ended]\n", res.PID)
	for deadline := time.Now().Add(10 * time.Second); !strings.HasSuffix(read(), ended); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("log = %q, want it to end %q", read(), ended)
		}
	}

	// A command that ends before any move is asked for ends as it would
	// with no MoveNow.
	ran, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "echo done", ReportDir: true, MoveNow: make(chan struct{})})
	if err != nil || ran.Moved || ran.ExitCode != 0 || string(ran.Output) != "done\n" || ran.Dir != f.ws() {
		t.Errorf("a command never moved = %+v %q %v", ran, ran.Output, err)
	}
}
