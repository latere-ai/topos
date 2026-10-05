// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
)

// Timeouts of bash (spec 008), in milliseconds.
const (
	BashDefaultTimeoutMS = 120000
	BashMaxTimeoutMS     = 600000
)

// BashServerGrace is how long a foreground command runs before a server
// it started is moved to the background (spec 045): long enough that a
// command which listens for a moment and ends is left alone, short
// enough that a server does not hold the turn.
const BashServerGrace = 3 * time.Second

// serverGrace is BashServerGrace, which a test shortens.
var serverGrace = BashServerGrace

// bashSchema states the timeouts from their constants, so the schema
// cannot drift from what runBash enforces.
var bashSchema = fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "minLength": 1, "description": "The command, run by /bin/sh -c."},
    "timeout_ms": {"type": "integer", "minimum": 1, "maximum": %d, "description": "Milliseconds before the command is killed; default %d."},
    "background": {"type": "boolean", "description": "Start the command detached and return at once with its pid and log; default false."},
    "description": {"type": "string", "description": "What the command does, in a few words."}
  },
  "required": ["command"],
  "additionalProperties": false
}`, BashMaxTimeoutMS, BashDefaultTimeoutMS)

func bashTool() Tool {
	return newBuiltin(NameBash, prompts.Text(prompts.ToolBash), bashSchema, Properties{Effect: EffectWrite}, runBash)
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
		return b.result(ctx, c, OutcomeError, prompts.Text(prompts.BashEmpty), nil)
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
	if !in.Background {
		req.ServerGrace = serverGrace
	}
	res, err := c.Machine.Exec(ctx, req)
	note := ""
	if err != nil && req.Dir != "" && missingDir(err) {
		// The persistent directory is gone, removed by an earlier
		// command; the call runs in the working directory instead, and
		// the directory it ends in replaces the lost one.
		note = prompts.Render(prompts.BashDirGone, prompts.Data{"Dir": req.Dir, "Workdir": c.Machine.Info().Workdir}) + "\n"
		req.Dir = ""
		res, err = c.Machine.Exec(ctx, req)
	}
	if err != nil {
		if harnessError(err) {
			return Result{}, err
		}
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.BashNotStarted, prompts.Data{"Error": err.Error()}), nil)
	}
	if in.Background {
		text := note + prompts.Render(prompts.BashBackground, prompts.Data{"PID": res.PID, "Log": res.Log})
		return b.result(ctx, c, OutcomeOK, text, nil)
	}
	if res.Moved {
		// A server the command started keeps running as a job; the turn
		// goes on with what it wrote so far.
		var out strings.Builder
		out.WriteString(note)
		out.Write(res.Output)
		if len(res.Output) > 0 && res.Output[len(res.Output)-1] != '\n' {
			out.WriteByte('\n')
		}
		out.WriteString(prompts.Render(prompts.BashMoved, prompts.Data{"Grace": serverGrace.String(), "Ports": portList(res.Ports), "PID": res.PID, "Log": res.Log}))
		return b.result(ctx, c, OutcomeOK, out.String(), nil)
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
		out.WriteString(prompts.Text(prompts.BashNoOutput) + "\n")
	}
	outcome := OutcomeOK
	switch {
	case res.TimedOut:
		outcome = OutcomeTimeout
		out.WriteString(prompts.Render(prompts.BashTimeout, prompts.Data{"Timeout": (time.Duration(timeout) * time.Millisecond).String()}))
		if res.ServerErr != nil {
			// The model learns why a server it ran in the foreground held
			// the call to the timeout.
			out.WriteString(" " + prompts.Render(prompts.BashUnchecked, prompts.Data{"Error": res.ServerErr.Error()}))
		}
	case res.Canceled:
		outcome = OutcomeCanceled
		out.WriteString(prompts.Text(prompts.BashCanceled))
	default:
		out.WriteString(prompts.Render(prompts.BashExitCode, prompts.Data{"Code": code}))
	}
	return b.result(ctx, c, outcome, out.String(), meta)
}

// missingDir reports an exec that failed because its start directory is
// gone or is no longer a directory.
func missingDir(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// portList names the ports a moved command listens on: "port 8000", or
// "ports 3000 and 8000", or "ports 3000, 5173 and 8000".
func portList(ports []int) string {
	names := make([]string, len(ports))
	for i, p := range ports {
		names[i] = strconv.Itoa(p)
	}
	switch len(names) {
	case 0:
		return "a port"
	case 1:
		return "port " + names[0]
	}
	return "ports " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
