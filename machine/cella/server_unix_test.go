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
