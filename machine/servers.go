// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ServerPoll is how often a foreground command past its ServerGrace has
// its process group's listening sockets read again (spec 045).
const ServerPoll = 500 * time.Millisecond

// ErrServersUnseen is a system whose processes' listening sockets this
// package does not read; a command there keeps its timeout alone.
var ErrServersUnseen = errors.New("machine: this system's listening sockets are not read")

// SeesServers reports whether ServerPorts reads this system's sockets.
const SeesServers = seesServers

// ServerPorts are the TCP ports on which a process of the process group
// pgid listens, below the low bound of the system's ephemeral range,
// sorted and without repeats (spec 045). A port in the ephemeral range is
// one the system chose for a listener bound to port 0, as a test's
// server is, so it does not count: a server someone is meant to reach
// listens on a port it chose.
func ServerPorts(ctx context.Context, pgid int) ([]int, error) { return serverPorts(ctx, pgid) }

// Ephemeral range defaults, used when the system does not say: Linux's
// own default low bound, and the IANA dynamic range macOS keeps.
const (
	linuxEphemeralLow  = 32768
	darwinEphemeralLow = 49152
)

// procServerPorts is ServerPorts over a proc file system mounted at
// root: the processes whose pgrp in <pid>/stat is pgid, their sockets'
// inodes from <pid>/fd, and the inodes net/tcp and net/tcp6 list in
// state LISTEN. A process that exits between the listing and the read,
// or one whose descriptors this user may not read, has nothing to add
// and is passed over; a socket table that cannot be read is an error.
func procServerPorts(root string, pgid int) ([]int, error) {
	inodes, err := procGroupSockets(root, pgid)
	if err != nil || len(inodes) == 0 {
		return nil, err
	}
	low := procEphemeralLow(root)
	var ports []int
	for _, table := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(root, "net", table))
		if errors.Is(err, fs.ErrNotExist) && table == "tcp6" {
			// A kernel without IPv6 has no tcp6 table.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("machine: read the %s socket table: %w", table, err)
		}
		ports = append(ports, procListening(b, inodes, low)...)
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

// procGroupSockets are the socket inodes the processes of group pgid
// hold open.
func procGroupSockets(root string, pgid int) (map[string]bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("machine: list the processes: %w", err)
	}
	inodes := map[string]bool{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			// The process exited after the listing.
			continue
		}
		if g, ok := statGroup(stat); !ok || g != pgid {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(root, e.Name(), "fd"))
		if err != nil {
			// The process exited, or its descriptors are not this
			// user's to read: neither is a server of this command's.
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(root, e.Name(), "fd", fd.Name()))
			if err != nil {
				// The descriptor closed after the listing.
				continue
			}
			if inode, ok := strings.CutPrefix(link, "socket:["); ok {
				inodes[strings.TrimSuffix(inode, "]")] = true
			}
		}
	}
	return inodes, nil
}

// statGroup reads the process group from a <pid>/stat line: the third
// field after the command name, which is in parentheses and may itself
// hold spaces and parentheses, so the fields start after the last ")".
func statGroup(stat []byte) (int, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 3 {
		return 0, false
	}
	g, err := strconv.Atoi(fields[2])
	return g, err == nil
}

// procEphemeralLow is the low bound of the ephemeral range from
// sys/net/ipv4/ip_local_port_range, or Linux's default when the file
// cannot be read, as inside a sandbox that hides it.
func procEphemeralLow(root string) int {
	b, err := os.ReadFile(filepath.Join(root, "sys", "net", "ipv4", "ip_local_port_range"))
	if err != nil {
		return linuxEphemeralLow
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return linuxEphemeralLow
	}
	low, err := strconv.Atoi(fields[0])
	if err != nil || low <= 0 {
		return linuxEphemeralLow
	}
	return low
}

// listenState is TCP_LISTEN as the socket tables print it.
const listenState = "0A"

// procListening are the ports below low of the listening sockets of a
// net/tcp or net/tcp6 table whose inode is in inodes. A row is "sl
// local_address rem_address st ... uid timeout inode ...", the local
// address hex IP and hex port.
func procListening(table []byte, inodes map[string]bool, low int) []int {
	var ports []int
	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[3] != listenState || !inodes[f[9]] {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		port, err := strconv.ParseUint(f[1][i+1:], 16, 16)
		if err == nil && port > 0 && int(port) < low {
			ports = append(ports, int(port))
		}
	}
	return ports
}

// lsofPorts are the ports below low that lsof -F n names, one "n" line
// per socket, as *:8000, 127.0.0.1:8000 or [::1]:8000.
func lsofPorts(out []byte, low int) []int {
	var ports []int
	for line := range strings.Lines(string(out)) {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "n")
		if !ok {
			continue
		}
		i := strings.LastIndexByte(name, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(name[i+1:])
		if err == nil && port > 0 && port < low {
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}
