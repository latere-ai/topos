// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command client connects to the server named by addr.txt and prints the
// line of health it answers. It waits up to 30 seconds for the server to
// start.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		line, err := health(ctx)
		if err == nil {
			fmt.Print(line)
			return
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "the server did not answer:", err)
			os.Exit(1)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func health(ctx context.Context) (string, error) {
	addr, err := os.ReadFile("addr.txt")
	if err != nil {
		return "", err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", strings.TrimSpace(string(addr)))
	if err != nil {
		return "", err
	}
	defer c.Close()
	b, err := io.ReadAll(c)
	return string(b), err
}
