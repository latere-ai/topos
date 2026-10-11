// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// recovering is a machine that says of itself what a sandbox's machine
// says: the id it has, whether its open made it new or restarted it, and
// each restart a command meets, which stop tells of before the command
// ends as one its machine stopped under.
type recovering struct {
	machine.Machine
	id      string
	created bool
	restart *machine.Restart
	stop    *machine.Stop

	mu   sync.Mutex
	hook func(ctx context.Context, r machine.Restart) error
}

func (m *recovering) Info() machine.Info {
	info := m.Machine.Info()
	info.ID = m.id
	return info
}

func (m *recovering) Created() bool { return m.created }

func (m *recovering) Restarted() (machine.Restart, bool) {
	if m.restart == nil {
		return machine.Restart{}, false
	}
	return *m.restart, true
}

func (m *recovering) OnRestart(hook func(ctx context.Context, r machine.Restart) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hook = hook
}

func (m *recovering) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	if m.stop == nil || !strings.Contains(r.Command, "npm install") {
		return m.Machine.Exec(ctx, r)
	}
	m.mu.Lock()
	hook := m.hook
	m.mu.Unlock()
	if m.stop.Restarted && hook != nil {
		if err := hook(ctx, machine.Restart{OutOfMemory: m.stop.OutOfMemory}); err != nil {
			return machine.ExecResult{}, err
		}
	}
	return machine.ExecResult{Output: []byte("added 12 packages\n"), ExitCode: machine.ExitKilled, Stopped: m.stop}, nil
}

// machines are the session's session.machine records, in order.
func (f *fixture) machines(ctx context.Context) []session.SessionMachine {
	f.t.Helper()
	var out []session.SessionMachine
	for _, e := range f.events(ctx, session.TypeSessionMachine) {
		var p session.SessionMachine
		if err := e.Decode(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// said reports whether the stub's request n carries text in a message.
func (f *fixture) said(n int, text string) bool {
	f.t.Helper()
	reqs := f.stub.Requests()
	if n >= len(reqs) {
		f.t.Fatalf("request %d of %d", n, len(reqs))
	}
	for _, m := range reqs[n].Request.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, text) {
				return true
			}
			if b.ToolResult != nil {
				for _, c := range b.ToolResult.Blocks {
					if strings.Contains(c.Text, text) {
						return true
					}
				}
			}
		}
	}
	return false
}

// first drives a turn whose command opens the session's machine as id.
func (f *fixture) first(ctx context.Context, id string) {
	f.t.Helper()
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{wrap: func(m machine.Machine) machine.Machine { return &recovering{Machine: m, id: id, created: true} }})
	f.stub.Script(model, reply(toolUse("toolu_0", "bash", `{"command":"echo one > f.txt"}`)), reply(ir.Block{Type: ir.BlockText, Text: "Made."}))
	f.message(ctx, "Make a file.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		f.t.Fatal(err)
	}
}

// TestAMachineRestartedAtItsOpenIsRecorded: a drive whose open finds the
// session's machine failed and has it started again appends
// session.machine with reason restarted and the cause the machine named,
// beside the call that opened it, restating the context the machine's
// first record holds, and no session.error. The model's next request
// reads that the machine restarted with its files (spec 009).
func TestAMachineRestartedAtItsOpenIsRecorded(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.first(ctx, "sbx_1")
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{wrap: func(m machine.Machine) machine.Machine {
		return &recovering{Machine: m, id: "sbx_1", restart: &machine.Restart{OutOfMemory: true}}
	}})
	f.stub.Script(model, reply(toolUse("toolu_1", "bash", `{"command":"cat f.txt"}`)), reply(ir.Block{Type: ir.BlockText, Text: "Still there."}))
	f.message(ctx, "Is the file there?")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	ms := f.machines(ctx)
	if len(ms) != 2 || ms[0].Reason != session.MachineAttached || ms[1].Reason != session.MachineRestarted || ms[1].Cause != session.MachineOutOfMemory {
		t.Fatalf("session.machine records %+v", ms)
	}
	if ms[1].Machine != ms[0].Machine || ms[1].Context != ms[0].Context || ms[1].Context == "" || ms[1].Repositories != nil || ms[1].Checkpoint != nil {
		t.Fatalf("the restarted record does not restate the machine's: %+v, first %+v", ms[1], ms[0])
	}
	if n := f.count(ctx, session.TypeSessionError); n != 0 {
		t.Fatalf("%d session.error events for a machine that kept its files", n)
	}
	evs := f.events(ctx, session.TypeSessionMachine)
	uses, results := f.events(ctx, session.TypeAgentToolUse), f.events(ctx, session.TypeToolResult)
	if uses[1].Seq >= evs[1].Seq || evs[1].Seq >= results[1].Seq {
		t.Fatalf("the record at %d, the call at %d, its result at %d", evs[1].Seq, uses[1].Seq, results[1].Seq)
	}
	note := prompts.Render(prompts.TranscriptMachineRestarted, prompts.Data{"OutOfMemory": true})
	last := len(f.stub.Requests()) - 1
	if !f.said(last, note) || !f.said(last, "one") || f.said(last-1, note) {
		t.Fatalf("the request after the open does not read the restart, or one before it does")
	}
	// A drive whose open found the machine running records nothing more.
	f.onDemandWith(f.work, &opens, demand{wrap: func(m machine.Machine) machine.Machine { return &recovering{Machine: m, id: "sbx_1"} }})
	f.stub.Script(model, reply(toolUse("toolu_2", "bash", `{"command":"true"}`)), reply(ir.Block{Type: ir.BlockText, Text: "Fine."}))
	f.message(ctx, "And now?")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(f.machines(ctx)); n != 2 {
		t.Fatalf("%d session.machine records after a drive on a running machine", n)
	}
}

// TestAMachineThatRestartsUnderACommandIsRecorded: a command whose
// machine stops under it and comes back appends session.machine with
// reason restarted between the call and its result, the result says what
// became of the machine and its files, and nothing says the files are
// gone (spec 009).
func TestAMachineThatRestartsUnderACommandIsRecorded(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{wrap: func(m machine.Machine) machine.Machine {
		return &recovering{Machine: m, id: "sbx_1", created: true, stop: &machine.Stop{OutOfMemory: true, Restarted: true}}
	}})
	f.stub.Script(model,
		reply(toolUse("toolu_1", "bash", `{"command":"echo one > f.txt"}`)),
		reply(toolUse("toolu_2", "bash", `{"command":"npm install"}`)),
		reply(toolUse("toolu_3", "bash", `{"command":"cat f.txt"}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "Installed in smaller steps."}),
	)
	f.message(ctx, "Build the app.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	ms := f.machines(ctx)
	if len(ms) != 2 || ms[1].Reason != session.MachineRestarted || ms[1].Cause != session.MachineOutOfMemory || ms[1].Machine != ms[0].Machine || ms[1].Context != ms[0].Context {
		t.Fatalf("session.machine records %+v", ms)
	}
	evs := f.events(ctx, session.TypeSessionMachine)
	uses, results := f.events(ctx, session.TypeAgentToolUse), f.events(ctx, session.TypeToolResult)
	if uses[1].Seq >= evs[1].Seq || evs[1].Seq >= results[1].Seq {
		t.Fatalf("the record at %d, the command at %d, its result at %d", evs[1].Seq, uses[1].Seq, results[1].Seq)
	}
	var killed session.ToolResult
	if err := results[1].Decode(&killed); err != nil {
		t.Fatal(err)
	}
	want := "added 12 packages\n" + prompts.Render(prompts.BashMachineStopped, prompts.Data{"OutOfMemory": true, "Restarted": true, "Lost": false, "Error": ""})
	if !killed.IsError || killed.Content[0].Text != want {
		t.Fatalf("the killed command's result %+v, want %q", killed, want)
	}
	if !f.said(2, want) || !f.said(2, prompts.Render(prompts.TranscriptMachineRestarted, prompts.Data{"OutOfMemory": true})) {
		t.Fatal("the request after the killed command reads neither its result nor the restart")
	}
	if f.said(3, prompts.Text(prompts.TranscriptMachineReplaced)) || f.count(ctx, session.TypeSessionError) != 0 {
		t.Fatal("a machine that kept its files reads as replaced, or recorded an error")
	}
}

// TestAReplacedMachineIsRecordedAsALoss: a drive whose open made the
// session's machine new, in the place of one the log records, appends
// session.machine with reason replaced and a session.error machine_lost
// that says the files are gone, and the model's next request reads it. A
// machine the session moved to stays a handoff (spec 009).
func TestAReplacedMachineIsRecordedAsALoss(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.first(ctx, "sbx_1")
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{wrap: func(m machine.Machine) machine.Machine { return &recovering{Machine: m, id: "sbx_2", created: true} }})
	f.stub.Script(model, reply(toolUse("toolu_1", "bash", `{"command":"ls"}`)), reply(ir.Block{Type: ir.BlockText, Text: "The files are gone."}))
	f.message(ctx, "Go on.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	ms := f.machines(ctx)
	if len(ms) != 2 || ms[1].Reason != session.MachineReplaced || ms[1].Machine.ID != "sbx_2" || ms[1].Cause != "" || ms[1].Context == "" {
		t.Fatalf("session.machine records %+v", ms)
	}
	var lost []session.SessionError
	for _, e := range f.events(ctx, session.TypeSessionError) {
		var p session.SessionError
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		lost = append(lost, p)
	}
	if len(lost) != 1 || lost[0].Code != machine.CodeLost || lost[0].Message != MessageMachineLost || !lost[0].Retryable || !strings.Contains(lost[0].Detail, "sbx_2") {
		t.Fatalf("session.error events %+v", lost)
	}
	evs, errs := f.events(ctx, session.TypeSessionMachine), f.events(ctx, session.TypeSessionError)
	uses, results := f.events(ctx, session.TypeAgentToolUse), f.events(ctx, session.TypeToolResult)
	if seqs := []uint64{uses[1].Seq, evs[1].Seq, errs[0].Seq, results[1].Seq}; !slices.IsSorted(seqs) {
		t.Fatalf("the call, the record, the error and the result are at %v", seqs)
	}
	last := len(f.stub.Requests()) - 1
	if !f.said(last, prompts.Text(prompts.TranscriptMachineReplaced)) {
		t.Fatal("the request after the open does not read that the files are gone")
	}

	// A machine other than the last one that its open did not make new
	// is one the session moved to.
	f.onDemandWith(f.work, &opens, demand{wrap: func(m machine.Machine) machine.Machine { return &recovering{Machine: m, id: "sbx_3"} }})
	f.stub.Script(model, reply(toolUse("toolu_2", "bash", `{"command":"true"}`)), reply(ir.Block{Type: ir.BlockText, Text: "Moved."}))
	f.message(ctx, "And now?")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if ms := f.machines(ctx); len(ms) != 3 || ms[2].Reason != session.MachineHandoff || f.count(ctx, session.TypeSessionError) != 1 {
		t.Fatalf("after a move: %+v, %d errors", ms, f.count(ctx, session.TypeSessionError))
	}
}
