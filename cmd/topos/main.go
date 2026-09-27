// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command topos is the core's scripting and test client: it runs a local
// session in print mode in the working directory and continues it after
// a confirmation (spec 024). It has no interactive terminal.
package main

import (
	"context"
	"io"
	"os"

	"latere.ai/x/topos/internal/toposcli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run returns the process exit code of spec 024.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return toposcli.Run(context.Background(), args, toposcli.Env{
		Getenv: os.Getenv, Stdin: stdin, Stdout: stdout, Stderr: stderr, Dir: wd,
	})
}
