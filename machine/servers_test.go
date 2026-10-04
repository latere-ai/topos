// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// procTree builds a proc file system under a temporary directory: each
// process its stat line and its descriptors, each a symlink to what it
// names, and the socket tables.
type procTree struct {
	t    *testing.T
	root string
}

func newProcTree(t *testing.T) procTree {
	t.Helper()
	return procTree{t: t, root: t.TempDir()}
}

func (p procTree) write(rel, body string) {
	p.t.Helper()
	path := filepath.Join(p.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p procTree) process(pid, stat string, fds map[string]string) {
	p.t.Helper()
	p.write(filepath.Join(pid, "stat"), stat)
	dir := filepath.Join(p.root, pid, "fd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.t.Fatal(err)
	}
	for fd, target := range fds {
		if err := os.Symlink(target, filepath.Join(dir, fd)); err != nil {
			p.t.Fatal(err)
		}
	}
}

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestProcServerPorts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's descriptors are symlinks")
	}
	p := newProcTree(t)
	// The shell leads group 100; python, a child in the group, holds the
	// server on 8000 (0x1F40) and a listener the system placed on 40000
	// (0x9C40); a process of another group holds 3000.
	p.process("100", "100 (sh) S 1 100 100 0 -1", map[string]string{"0": "/dev/null", "1": "pipe:[7]"})
	p.process("101", "101 (python3 -m http (x)) S 100 100 100 0 -1", map[string]string{"3": "socket:[5001]", "4": "socket:[5002]", "5": "socket:[5004]"})
	p.process("200", "200 (node) S 1 200 200 0 -1", map[string]string{"3": "socket:[5003]"})
	p.write("self", "not a process")
	p.write("net/tcp", tcpHeader+
		"   0: 00000000:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5001 1 0000000000000000 100 0 0 10 0\n"+
		"   1: 0100007F:9C40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5002 1 0000000000000000 100 0 0 10 0\n"+
		"   2: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5003 1 0000000000000000 100 0 0 10 0\n"+
		"   3: 0100007F:1F41 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000  1000        0 5004 1 0000000000000000 100 0 0 10 0\n")
	p.write("net/tcp6", tcpHeader+
		"   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5004 1 0000000000000000 100 0 0 10 0\n")
	p.write("sys/net/ipv4/ip_local_port_range", "32768\t60999\n")

	got, err := procServerPorts(p.root, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 8000 listens; 40000 is in the ephemeral range; 8001 is a connection,
	// not a listener; 8080 (0x1F90) listens on IPv6 on inode 5004.
	if want := []int{8000, 8080}; !slices.Equal(got, want) {
		t.Errorf("ports = %v, want %v", got, want)
	}
	if got, err := procServerPorts(p.root, 300); err != nil || got != nil {
		t.Errorf("a group with no process: %v %v", got, err)
	}

	// A lower ephemeral bound counts 8000 as the system's choice.
	p.write("sys/net/ipv4/ip_local_port_range", "1024\t60999\n")
	if got, err := procServerPorts(p.root, 100); err != nil || len(got) != 0 {
		t.Errorf("below a low ephemeral range: %v %v", got, err)
	}
	// An unreadable range is Linux's default.
	if err := os.Remove(filepath.Join(p.root, "sys/net/ipv4/ip_local_port_range")); err != nil {
		t.Fatal(err)
	}
	if low := procEphemeralLow(p.root); low != linuxEphemeralLow {
		t.Errorf("default low = %d", low)
	}
	p.write("sys/net/ipv4/ip_local_port_range", "x")
	if low := procEphemeralLow(p.root); low != linuxEphemeralLow {
		t.Errorf("unreadable low = %d", low)
	}
	p.write("sys/net/ipv4/ip_local_port_range", "")
	if low := procEphemeralLow(p.root); low != linuxEphemeralLow {
		t.Errorf("empty low = %d", low)
	}

	// No tcp6 table is a kernel without IPv6; no tcp table is an error.
	if err := os.Remove(filepath.Join(p.root, "net/tcp6")); err != nil {
		t.Fatal(err)
	}
	if got, err := procServerPorts(p.root, 100); err != nil || !slices.Equal(got, []int{8000}) {
		t.Errorf("without tcp6: %v %v", got, err)
	}
	if err := os.Remove(filepath.Join(p.root, "net/tcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := procServerPorts(p.root, 100); err == nil {
		t.Error("a missing tcp table read as no server")
	}
	if _, err := procServerPorts(filepath.Join(p.root, "gone"), 100); err == nil {
		t.Error("a missing proc root read as no server")
	}
}

func TestStatGroup(t *testing.T) {
	for stat, want := range map[string]int{
		"1 (init) S 0 1 1 0":                 1,
		"42 (a) b) (c) R 7 42 42":            42,
		"9 (x) S 1":                          -1,
		"no parenthesis":                     -1,
		"5 (y) S 1 notanumber 5":             -1,
		"77 (tmux: server) S 1 77 77 0 -1 4": 77,
	} {
		g, ok := statGroup([]byte(stat))
		if (want < 0) == ok || (ok && g != want) {
			t.Errorf("statGroup(%q) = %d %v, want %d", stat, g, ok, want)
		}
	}
}

func TestLsofPorts(t *testing.T) {
	out := []byte("p501\nf3\nn*:8000\nf4\nn127.0.0.1:50123\nf5\nn[::1]:3000\np502\nf6\nn[::]:8000\nnnoport\nn:x\nfzz\n")
	if got, want := lsofPorts(out, darwinEphemeralLow), []int{3000, 8000}; !slices.Equal(got, want) {
		t.Errorf("ports = %v, want %v", got, want)
	}
	if got := lsofPorts(nil, darwinEphemeralLow); len(got) != 0 {
		t.Errorf("no output: %v", got)
	}
}

func TestServerPortsOfThisProcess(t *testing.T) {
	if !SeesServers {
		if _, err := ServerPorts(t.Context(), os.Getpid()); err == nil {
			t.Error("a system whose sockets are not read answered no error")
		}
		t.Skip("this system's sockets are not read")
	}
	// The test's own group listens on no server port of its own making;
	// whatever the answer, it is read without an error.
	if _, err := ServerPorts(t.Context(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
}
