// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package tools

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
)

// The variables that make the test binary a listener: the address to
// listen on, and how long to serve before exiting, empty for as long as
// it is let.
const (
	envListen    = "TOPOS_TEST_LISTEN"
	envListenFor = "TOPOS_TEST_LISTEN_FOR"
)

// TestListenHelper is not a test: run with TOPOS_TEST_LISTEN set, the
// test binary is a server for the bash tool to start, which prints the
// port it listens on, answers every connection with "ok", and logs each
// one it accepts.
func TestListenHelper(t *testing.T) {
	addr := os.Getenv(envListen)
	if addr == "" {
		t.Skip("the listener of the server tests; it runs as their command")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(2)
	}
	fmt.Printf("listening on %d\n", ln.Addr().(*net.TCPAddr).Port)
	if d, err := time.ParseDuration(os.Getenv(envListenFor)); err == nil {
		time.AfterFunc(d, func() {
			fmt.Println("served long enough")
			os.Exit(0)
		})
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Println("accept:", err)
			os.Exit(2)
		}
		fmt.Println("accepted")
		if _, err := c.Write([]byte("ok\n")); err != nil {
			fmt.Println("write:", err)
		}
		if err := c.Close(); err != nil {
			fmt.Println("close:", err)
		}
	}
}

// serverPort is a free port below every system's ephemeral range: a port
// a person's server chooses, not one the system would pick.
func serverPort(t *testing.T) int {
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

// listenCommand is the bash command that runs the test binary as a
// listener on addr, after printing a line of its own.
func listenCommand(t *testing.T, addr, serveFor string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := envListen + "=" + addr
	if serveFor != "" {
		env += " " + envListenFor + "=" + serveFor
	}
	return fmt.Sprintf("echo starting; %s exec '%s' -test.run='^TestListenHelper$'", env, exe)
}

// shortGrace makes the bash tool's grace short for a test.
func shortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	was := serverGrace
	serverGrace = d
	t.Cleanup(func() { serverGrace = was })
}

// dial reads the server's answer on port, or the error of reaching it.
func dial(port int) (string, error) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", err
	}
	return bufio.NewReader(c).ReadString('\n')
}

func TestBashMovesAServerToTheBackground(t *testing.T) {
	if !machine.SeesServers {
		t.Skip("this system's listening sockets are not read")
	}
	shortGrace(t, 300*time.Millisecond)
	f := open(t)
	bash := builtinTool(t, NameBash)
	port := serverPort(t)
	begun := time.Now()
	res := run(t.Context(), t, bash, f.h, State{}, mustInput(t, map[string]any{"command": listenCommand(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ""), "timeout_ms": 60000}))
	took := time.Since(begun)
	t.Logf("the call returned %s after it started, with a grace of %s and a poll of %s", took, serverGrace, machine.ServerPoll)
	if took > serverGrace+machine.ServerPoll+3*time.Second {
		t.Errorf("the call held for %s", took)
	}
	out := text(res)
	m := regexp.MustCompile(`moved to the background as pid (\d+) and keeps running\. Its output so far is above; what it writes from now on goes to (\S+)\. `).FindStringSubmatch(out)
	if res.Outcome != OutcomeOK || res.IsError() || m == nil || res.Meta != nil {
		t.Fatalf("result %s %q meta %+v", res.Outcome, out, res.Meta)
	}
	if !strings.HasPrefix(out, "starting\n") || !strings.Contains(out, fmt.Sprintf("listening on port %d,", port)) ||
		!strings.Contains(out, "kill -TERM -"+m[1]) {
		t.Errorf("result %q", out)
	}
	log := m[2]
	if got, err := dial(port); err != nil || got != "ok\n" {
		t.Fatalf("the server after the move: %q %v", got, err)
	}
	// What the server writes after the move is in its log.
	waitFor(t, func() bool { return strings.Contains(get(t, log), "accepted\n") })

	if err := f.h.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := dial(port)
		return err != nil
	})
	waitFor(t, func() bool { return strings.Contains(get(t, log), "[job "+m[1]+" exited with code") })
}

func TestBashLeavesAListenerTheSystemPlacedInTheForeground(t *testing.T) {
	if !machine.SeesServers {
		t.Skip("this system's listening sockets are not read")
	}
	shortGrace(t, 200*time.Millisecond)
	f := open(t)
	bash := builtinTool(t, NameBash)
	serve := 6 * machine.ServerPoll
	begun := time.Now()
	res := run(t.Context(), t, bash, f.h, State{}, mustInput(t, map[string]any{"command": listenCommand(t, "127.0.0.1:0", serve.String()), "timeout_ms": 60000}))
	out := text(res)
	if took := time.Since(begun); took < serve {
		t.Errorf("the call returned after %s, before the listener ended", took)
	}
	if res.Outcome != OutcomeOK || !strings.Contains(out, "served long enough") || !strings.HasSuffix(out, "Exit code: 0") || strings.Contains(out, "moved") {
		t.Fatalf("result %s %q", res.Outcome, out)
	}
}

func TestBashLeavesAServerToItsTimeoutWhenTheGraceIsLonger(t *testing.T) {
	if !machine.SeesServers {
		t.Skip("this system's listening sockets are not read")
	}
	shortGrace(t, time.Hour)
	f := open(t)
	bash := builtinTool(t, NameBash)
	port := serverPort(t)
	res := run(t.Context(), t, bash, f.h, State{}, mustInput(t, map[string]any{"command": listenCommand(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ""), "timeout_ms": 800}))
	if res.Outcome != OutcomeTimeout || !strings.Contains(text(res), "passed its timeout") {
		t.Fatalf("result %s %q", res.Outcome, text(res))
	}
	if _, err := dial(port); err == nil {
		t.Error("the server outlived its timeout")
	}
}

func TestBashACommandThatNeverListensIsUnchanged(t *testing.T) {
	shortGrace(t, 100*time.Millisecond)
	f := open(t)
	bash := builtinTool(t, NameBash)
	res := run(t.Context(), t, bash, f.h, State{}, `{"command":"sleep 1; echo done"}`)
	if res.Outcome != OutcomeOK || text(res) != "done\nExit code: 0" || res.Meta == nil || res.Meta.Dir != f.work {
		t.Fatalf("result %s %q %+v", res.Outcome, text(res), res.Meta)
	}
}

func TestPortList(t *testing.T) {
	for want, ports := range map[string][]int{
		"a port":                    nil,
		"port 8000":                 {8000},
		"ports 3000 and 8000":       {3000, 8000},
		"ports 3000, 5173 and 8000": {3000, 5173, 8000},
	} {
		if got := portList(ports); got != want {
			t.Errorf("portList(%v) = %q, want %q", ports, got, want)
		}
	}
}

// waitFor polls cond for a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not hold in time")
		}
	}
}
