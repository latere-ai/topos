// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command topos is the core's scripting and test client: it runs a session
// in print mode in the working directory, attaches to a session, and
// applies manifests (spec 024). It has no interactive terminal. Until spec
// 024 lands it prints its build identity.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"latere.ai/x/topos/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the process exit code: 0 for -version, 2 for anything else
// until the commands of spec 024 exist.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("topos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String("topos"))
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "topos: no commands yet; spec 024 builds them")
	return 2
}
