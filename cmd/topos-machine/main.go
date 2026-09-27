// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command topos-machine is the helper a Cella machine uploads into its
// sandbox (spec 009). It is a static binary of the standard library and
// the machine package, so a sandbox needs nothing in its image for the
// tools: search runs the Go-native grep and glob of spec 008 where the
// files are, run gives a command its own process group, a timeout, an
// input that ends and a report of its final directory, job starts a
// background job, and fs reaches the files outside the workspace, which
// Cella's file routes do not serve.
//
// Every mode that answers the machine writes its answer on standard
// output alone: the exec socket carries standard output and standard
// error as one stream, so nothing is written to standard error once a
// mode has started.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// usage is the one line a mistaken invocation prints. The machine never
// makes one; a person running the helper by hand reads it.
const usage = "usage: topos-machine sum | search <root> | run [flags] -- <script> | job [flags] -- <script> | fs <op> <root> <path> [<arg>]"

// run dispatches the mode and returns the exit code: 0 once a mode has
// answered, whatever the answer, and 2 for a usage error.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, usage)
	}
	switch args[0] {
	case "sum":
		return sum(stdout, stderr, os.Executable)
	case "search":
		return search(ctx, args[1:], stdin, stdout, stderr)
	case "run":
		return runCommand(ctx, args[1:], stdin, stdout, stderr)
	case "job":
		return job(ctx, args[1:], stdout, stderr)
	case "fs":
		return fileOp(ctx, args[1:], stdin, stdout, stderr)
	}
	return fail(stderr, usage)
}

// fail prints a usage error and returns its exit code.
func fail(stderr io.Writer, msg string) int {
	if _, err := fmt.Fprintln(stderr, "topos-machine: "+msg); err != nil {
		return 3
	}
	return 2
}

// sum prints the SHA-256 of the helper's own executable, which is how the
// machine tells that the helper in a sandbox is the build it would upload.
func sum(stdout, stderr io.Writer, executable func() (string, error)) int {
	exe, err := executable()
	if err != nil {
		return fail(stderr, err.Error())
	}
	f, err := os.Open(exe)
	if err != nil {
		return fail(stderr, err.Error())
	}
	h := sha256.New()
	_, cerr := io.Copy(h, f)
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return fail(stderr, cerr.Error())
	}
	if _, err := fmt.Fprintln(stdout, hex.EncodeToString(h.Sum(nil))); err != nil {
		return 3
	}
	return 0
}
