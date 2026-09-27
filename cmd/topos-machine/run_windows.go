// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
)

// errNoCommands answers run and job on Windows. The helper runs inside
// the Linux sandboxes of Cella; its commands need /bin/sh and a process
// group of their own, and it is built for Windows only so that the
// module compiles there.
var errNoCommands = errors.New("machine: topos-machine runs commands on Linux and macOS only")

// runCommand answers the failure frame run sends for a command that
// could not start.
func runCommand(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) int {
	return sent((&frameWriter{w: stdout}).json(frameFail, failure(errNoCommands)))
}

// job answers the failure job sends for a job that could not start.
func job(_ context.Context, _ []string, stdout, _ io.Writer) int {
	return answer(stdout, response{Error: failure(errNoCommands)})
}
