// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
)

// TestMain runs the test binary as the helper's log pump, which the run
// mode starts from its own executable, the test binary in a test, and as
// a listener a test's command starts.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "pump":
			os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
		case "test-listen":
			os.Exit(listen(os.Args[2]))
		}
	}
	os.Exit(m.Run())
}

// listen serves addr, answering each connection "ok" and printing that
// it accepted it, until it is killed.
func listen(addr string) int {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("listen:", err)
		return 2
	}
	fmt.Println("listening")
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Println("accept:", err)
			return 2
		}
		fmt.Println("accepted")
		_, werr := c.Write([]byte("ok\n"))
		if err := c.Close(); err != nil || werr != nil {
			fmt.Println("answer:", werr, err)
		}
	}
}

// freePort is a free port below every system's ephemeral range.
func freePort(t *testing.T) int {
	t.Helper()
	for range 50 {
		p := 20000 + rand.IntN(8000)
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Fatal("no free port below the ephemeral range")
	return 0
}

// listenScript runs the test binary as a listener on port after a line
// of its own.
func listenScript(t *testing.T, port int) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("echo starting; exec '%s' test-listen 127.0.0.1:%d", exe, port)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen in time", what)
		}
	}
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunMovesAServerToTheBackground(t *testing.T) {
	if !machine.SeesServers {
		t.Skip("this system's listening sockets are not read")
	}
	dir := tempDir(t)
	jobs := filepath.Join(dir, "spill", "jobs")
	port := freePort(t)
	begun := time.Now()
	d := drive(t, t.Context(), "-dir", dir, "-report-dir", "-server-grace", "200ms", "-jobs", jobs, "--", listenScript(t, port))
	d.started(t)
	frames, code := d.rest(t)
	t.Logf("the helper answered %s after the start", time.Since(begun))
	out, gotDir, exit := outcome(t, frames)
	if code != 0 || out != "starting\nlistening\n" || gotDir != "" || !exit.Moved || exit.PID <= 0 ||
		filepath.Dir(exit.Log) != jobs || !slices.Equal(exit.Ports, []int{port}) || exit.ServerErr != "" {
		t.Fatalf("code %d, output %q, dir %q, exit %+v", code, out, gotDir, exit)
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("the server after the helper's exit: %v", err)
	}
	line, rerr := bufio.NewReader(c).ReadString('\n')
	if err := c.Close(); err != nil || rerr != nil || line != "ok\n" {
		t.Fatalf("the answer %q %v %v", line, rerr, err)
	}
	eventually(t, "the log pump's copy of the server's output", func() bool { return readLog(t, exit.Log) == "accepted\n" })
	if err := syscall.Kill(-exit.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("accepted\n\n[job %d output ended]\n", exit.PID)
	eventually(t, "the log's end", func() bool { return readLog(t, exit.Log) == want })
}

func TestRunKeepsAServerItCannotMove(t *testing.T) {
	if !machine.SeesServers {
		t.Skip("this system's listening sockets are not read")
	}
	dir := tempDir(t)
	file := filepath.Join(dir, "file")
	writeFile(t, file, "x")
	port := freePort(t)
	d := drive(t, t.Context(), "-dir", dir, "-timeout", "1500ms", "-server-grace", "200ms", "-jobs", filepath.Join(file, "jobs"), "--", listenScript(t, port))
	d.started(t)
	frames, code := d.rest(t)
	out, _, exit := outcome(t, frames)
	if code != 0 || out != "starting\nlistening\n" || exit.Moved || !exit.TimedOut || !strings.Contains(exit.ServerErr, "create the job directory") {
		t.Fatalf("code %d, output %q, exit %+v", code, out, exit)
	}
}

func TestRunRefusesAServerGraceWithoutJobs(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"run", "-server-grace", "1s", "--", "true"}, nil, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "-jobs") {
		t.Errorf("exit %d, %q", code, stderr.String())
	}
}

func TestPump(t *testing.T) {
	var log bytes.Buffer
	if code := pump([]string{"-job", "42"}, strings.NewReader("a\nb\n"), &log, io.Discard); code != 0 || log.String() != "a\nb\n\n[job 42 output ended]\n" {
		t.Errorf("exit %d, log %q", code, log.String())
	}
	log.Reset()
	if code := pump([]string{"-job", "42"}, failingReader{}, &log, io.Discard); code != 0 || !strings.Contains(log.String(), "[job 42: the log stopped: disk gone]") {
		t.Errorf("a failed copy: exit %d, log %q", code, log.String())
	}
	for _, args := range [][]string{{}, {"-job", "0"}, {"-job", "1", "extra"}, {"-bad"}} {
		if code := pump(args, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	if code := pump([]string{"-job", "1"}, strings.NewReader(""), failingWriter{}, io.Discard); code != 3 {
		t.Errorf("an unwritable log: exit %d", code)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, fmt.Errorf("disk gone") }
