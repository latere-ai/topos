// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/topos/machine"
)

// Timeouts of bash (spec 008), in milliseconds.
const (
	BashDefaultTimeoutMS = 120000
	BashMaxTimeoutMS     = 600000
)

//go:embed descriptions/bash.md
var bashDescription string

const bashSchema = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "minLength": 1, "description": "The command, run by /bin/sh -c."},
    "timeout_ms": {"type": "integer", "minimum": 1, "maximum": 600000, "description": "Milliseconds before the command is killed; default 120000."},
    "background": {"type": "boolean", "description": "Start the command detached and return at once with its pid and log; default false."},
    "description": {"type": "string", "description": "What the command does, in a few words."}
  },
  "required": ["command"],
  "additionalProperties": false
}`

func bashTool() Tool {
	return newBuiltin(NameBash, bashDescription, bashSchema, Properties{Effect: EffectWrite}, runBash)
}

type bashInput struct {
	Command     string `json:"command"`
	TimeoutMS   int    `json:"timeout_ms"`
	Background  bool   `json:"background"`
	Description string `json:"description"`
}

func runBash(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in bashInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return b.result(ctx, c, OutcomeError, "The command is empty.", nil)
	}
	timeout := in.TimeoutMS
	if timeout <= 0 {
		timeout = BashDefaultTimeoutMS
	}
	timeout = min(timeout, BashMaxTimeoutMS)
	req := machine.ExecRequest{
		Command:    in.Command,
		Dir:        c.State.Dir,
		Timeout:    time.Duration(timeout) * time.Millisecond,
		Background: in.Background,
		ReportDir:  !in.Background,
	}
	res, err := c.Machine.Exec(ctx, req)
	note := ""
	if err != nil && req.Dir != "" && missingDir(err) {
		// The persistent directory is gone, removed by an earlier
		// command; the call runs in the working directory instead, and
		// the directory it ends in replaces the lost one.
		note = fmt.Sprintf("[%s no longer exists; the command ran in the working directory %s]\n", req.Dir, c.Machine.Info().Workdir)
		req.Dir = ""
		res, err = c.Machine.Exec(ctx, req)
	}
	if err != nil {
		if errors.Is(err, machine.ErrReleased) {
			return Result{}, err
		}
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("The command did not start: %v.", err), nil)
	}
	if in.Background {
		text := fmt.Sprintf("%sStarted in the background as pid %d; its output goes to %s. Read the log with read, and stop the job with bash: kill -TERM -%d.", note, res.PID, res.Log, res.PID)
		return b.result(ctx, c, OutcomeOK, text, nil)
	}
	code := res.ExitCode
	meta := &Meta{Dir: res.Dir, ExitCode: &code}
	if note != "" && res.Dir == "" {
		meta.Dir = c.Machine.Info().Workdir
	}
	var out strings.Builder
	out.WriteString(note)
	out.Write(res.Output)
	if len(res.Output) > 0 && res.Output[len(res.Output)-1] != '\n' {
		out.WriteByte('\n')
	}
	if len(res.Output) == 0 {
		out.WriteString("(no output)\n")
	}
	outcome := OutcomeOK
	switch {
	case res.TimedOut:
		outcome = OutcomeTimeout
		fmt.Fprintf(&out, "The command passed its timeout of %s and was killed with its process group.", time.Duration(timeout)*time.Millisecond)
	case res.Canceled:
		outcome = OutcomeCanceled
		out.WriteString("The command was canceled and killed with its process group.")
	default:
		fmt.Fprintf(&out, "Exit code: %d", code)
	}
	return b.result(ctx, c, outcome, out.String(), meta)
}

// missingDir reports an exec that failed because its start directory is
// gone or is no longer a directory.
func missingDir(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
