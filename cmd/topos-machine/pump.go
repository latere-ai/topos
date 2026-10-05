// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
)

// pump copies its input, the output pipe of a command moved to the
// background (spec 045), to its output, the job's log, until every
// process that writes to the pipe has closed it, and then ends the log
// with a line naming the job, as a background job's log ends. The run
// mode starts it in a session of its own, so it outlives the helper and
// the exec session that ran the command. The log is the only place left
// to report to, so a failed copy is written there.
func pump(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pump", flag.ContinueOnError)
	flags.SetOutput(stderr)
	job := flags.Int("job", 0, "the job's pid, which the log's last line names")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *job <= 0 {
		return fail(stderr, "pump -job <pid>")
	}
	end := fmt.Sprintf("\n[job %d output ended]\n", *job)
	if _, err := io.Copy(stdout, stdin); err != nil {
		end = fmt.Sprintf("\n[job %d: the log stopped: %v]\n", *job, err)
	}
	if _, err := io.WriteString(stdout, end); err != nil {
		return 3
	}
	return 0
}
