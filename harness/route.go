// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
)

// lineRunner is a memory machine that runs a command line itself when the
// line reaches its folders alone (spec 097). Runs answers before anything
// runs: an empty reason for a line it runs started in dir, and otherwise
// why not and the program the line names.
type lineRunner interface {
	Runs(command, dir string) (reason, program string)
}

// Where a bash call of a session whose memory machine runs command lines
// ran (spec 097).
const (
	WhereProcess = "process"
	WhereMachine = "machine"
)

// The reasons a call ran on the session's machine, or opened it, beside
// the reasons of the memory machine's shell (spec 097).
const (
	// ReasonBackground is a bash call that starts its command in the
	// background, which only a machine runs.
	ReasonBackground = "background"
	// ReasonSpill is a call on the memory machine whose long result was
	// written to a file on the session's machine.
	ReasonSpill = "spill"
	// ReasonTool is a call of a tool that acts on the session's machine,
	// about which no shell was asked.
	ReasonTool = "tool"
)

// Meter counts where calls run (spec 097). Its methods are called on the
// call's goroutine and must not block.
type Meter interface {
	// Ran counts a bash call of a session whose memory machine runs
	// command lines: where it ran, WhereProcess or WhereMachine, and for
	// the machine, why.
	Ran(ctx context.Context, where, reason string)
	// Opened counts a call that opened the session's machine opened on
	// demand: its tool and why.
	Opened(ctx context.Context, tool, reason string)
}

// route is where a call runs.
type route struct {
	m machine.Machine
	// memory is set for a call on the memory machine, which opens no
	// other.
	memory bool
	// asked is set for a bash call the memory machine was asked about:
	// reason is why it runs on the session's machine, empty when it runs
	// on the memory machine, and program the program the shell named.
	asked           bool
	reason, program string
}

// route is the machine a call runs on, started in dir: the memory
// machine for one onMemory names, and for a bash call whose line the
// memory machine runs itself (spec 097); the session's for any other. A
// bash call in the background runs on the session's machine, which alone
// keeps a job.
func (h *Harness) route(tool tools.Tool, input []byte, dir string) route {
	if h.onMemory(tool, input) {
		return route{m: h.c.Memory, memory: true}
	}
	lr, ok := h.c.Memory.(lineRunner)
	if !ok || tool.Definition().Name != tools.NameBash {
		return route{m: h.c.Machine}
	}
	var in struct {
		Command    string `json:"command"`
		Background bool   `json:"background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		// A call the tool cannot decode is answered by the tool, which
		// says why, on the machine any other call of it runs on.
		return route{m: h.c.Machine}
	}
	r := route{m: h.c.Machine, asked: true}
	if in.Background {
		r.reason = ReasonBackground
		return r
	}
	r.reason, r.program = lr.Runs(in.Command, dir)
	if r.reason == "" {
		r.m, r.memory = h.c.Memory, true
	}
	return r
}

// runsLines reports whether the session's memory machine runs command
// lines itself, so a bash call may need no other machine.
func (h *Harness) runsLines() bool {
	_, ok := h.c.Memory.(lineRunner)
	return ok
}

// onMemory reports whether a call runs on the session's memory machine
// (spec 093): a call of a tool that names its paths, in a session with
// memory folders, every path of which is under /memory/ once cleaned. A
// relative path, and a call that names none, act in the working
// directory and run on the session's machine. A bash call the memory
// machine runs is not one: its verdict is a command's (spec 097).
func (h *Harness) onMemory(tool tools.Tool, input []byte) bool {
	p, ok := tool.(tools.Pather)
	if !ok || h.c.Memory == nil {
		return false
	}
	paths := p.Paths(input)
	return len(paths) > 0 && !slices.ContainsFunc(paths, func(p string) bool { return !memory.Under(p) })
}

// opener is a machine opened on demand, which tells whether it opened.
type opener interface {
	Opened() machine.Machine
}

// unopened reports whether the session's machine opens on demand and has
// not opened.
func (h *Harness) unopened() bool {
	o, ok := h.c.Machine.(opener)
	return ok && o.Opened() == nil
}

// recordOpen records the call that opened the session's machine opened
// on demand (spec 097), once for the machine: a line of Config.Log with
// the tool, why and the program the shell named, and Config.Meter's
// count without the program. Calls of one step that opened it beside
// each other record it once.
func (t *turn) recordOpen(ctx context.Context, c plannedCall, reason, program string) {
	if o, ok := t.h.c.Machine.(opener); !ok || o.Opened() == nil || !t.h.opened.CompareAndSwap(false, true) {
		return
	}
	tool := c.tool.Definition().Name
	t.h.c.Log.LogAttrs(ctx, slog.LevelInfo, "a tool call opened the session's machine",
		slog.String("session", t.s.ID), slog.String("subagent", t.thread), slog.Int("turn", t.num), slog.Int("step", t.step),
		slog.String("tool", tool), slog.String("tool_use_id", c.id), slog.String("reason", reason), slog.String("program", program))
	if t.h.c.Meter != nil {
		t.h.c.Meter.Opened(ctx, tool, reason)
	}
}

// recordRun counts where a bash call the memory machine was asked about
// ran (spec 097).
func (t *turn) recordRun(ctx context.Context, r route) {
	if !r.asked || t.h.c.Meter == nil {
		return
	}
	if r.memory {
		t.h.c.Meter.Ran(ctx, WhereProcess, "")
		return
	}
	t.h.c.Meter.Ran(ctx, WhereMachine, r.reason)
}
