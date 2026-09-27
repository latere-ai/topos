// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command server answers every connection on a free loopback port with
// one line of health, writes the address it listens on to addr.txt in
// its working directory, and runs until it is stopped.
package main

import (
	"fmt"
	"net"
	"os"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile("addr.txt", []byte(l.Addr().String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening on", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(c, "healthy: 3 workers, queue empty")
		if err := c.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}
