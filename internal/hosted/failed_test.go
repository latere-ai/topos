// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// reasons are the reasons of the session's session.machine records, in
// order, each with its cause after a colon when it has one.
func (c *cloud) reasons() []string {
	c.t.Helper()
	var out []string
	for _, e := range c.events(session.TypeSessionMachine) {
		var p session.SessionMachine
		if err := e.Decode(&p); err != nil {
			c.t.Fatal(err)
		}
		if p.Cause != "" {
			p.Reason += ":" + p.Cause
		}
		out = append(out, p.Reason)
	}
	return out
}

// resultTexts are the texts of the session's tool results, in order.
func (c *cloud) resultTexts() []string {
	c.t.Helper()
	var out []string
	for _, e := range c.events(session.TypeToolResult) {
		var p session.ToolResult
		if err := e.Decode(&p); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, p.Content[0].Text)
	}
	return out
}

// lastSaid reports whether the last request the model received carries
// text in a message.
func (c *cloud) lastSaid(text string) bool {
	c.t.Helper()
	reqs := c.lux.Requests()
	for _, m := range reqs[len(reqs)-1].Request.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, text) {
				return true
			}
		}
	}
	return false
}

// TestAFailedSandboxIsStartedWithItsFiles: a hosted session whose sandbox
// failed for memory between two turns, on a core that starts a failed
// sandbox, goes on in the same sandbox with the files its first turn
// made. No delete is sent, the log records the machine as restarted for
// memory and records no error, and the model reads that the machine
// restarted with its files (spec 009).
func TestAFailedSandboxIsStartedWithItsFiles(t *testing.T) {
	c := newCloud(t, nil)
	c.cella.RestartFailed(true)
	c.drive("Make a file.", bash("toolu_1", "echo one > f.txt"), said("Made."))
	name := cella.SandboxName(c.s.ID)
	before, _ := c.cella.Sandbox(name)
	if !c.cella.SetFailed(name, "OOMKilled") {
		t.Fatal("the session has no sandbox")
	}
	c.drive("Read it.", bash("toolu_2", "cat f.txt"), said("It says one."))
	after, ok := c.cella.Sandbox(name)
	if !ok || after.Status.ID != before.Status.ID || after.Status.Phase != "Running" {
		t.Fatalf("the sandbox after the failure is %+v, was %s", after.Status, before.Status.ID)
	}
	if n := c.cella.Count(cellastub.OpDelete); n != 0 {
		t.Fatalf("%d deletes of a sandbox the core starts again", n)
	}
	if got := c.results(); len(got) != 2 || !strings.Contains(got[1], "one") {
		t.Fatalf("the results are %q", got)
	}
	if got := c.reasons(); !slices.Equal(got, []string{session.MachineAttached, session.MachineRestarted + ":" + session.MachineOutOfMemory}) {
		t.Fatalf("session.machine reasons %v", got)
	}
	if n := len(c.events(session.TypeSessionError)); n != 0 {
		t.Fatalf("%d session.error events\n%s", n, c.dump())
	}
	if !c.lastSaid(prompts.Render(prompts.TranscriptMachineRestarted, prompts.Data{"OutOfMemory": true})) {
		t.Fatal("the model does not read that the machine restarted with its files")
	}
}

// TestAFailedSandboxAnOlderCoreDoesNotStartIsReplacedAndSaid: on a core
// that refuses to start a failed sandbox, the sandbox is replaced, which
// removes the session's files. The log records the machine as replaced,
// never as a handoff, with a session.error machine_lost in a person's
// words, and the model reads that the files are gone before it meets an
// empty directory (spec 009).
func TestAFailedSandboxAnOlderCoreDoesNotStartIsReplacedAndSaid(t *testing.T) {
	c := newCloud(t, nil)
	c.drive("Make a file.", bash("toolu_1", "echo one > f.txt"), said("Made."))
	name := cella.SandboxName(c.s.ID)
	before, _ := c.cella.Sandbox(name)
	c.cella.SetFailed(name, "OOMKilled")
	c.drive("Read it.", bash("toolu_2", "ls"), said("The files are gone."))
	after, ok := c.cella.Sandbox(name)
	if !ok || after.Status.ID == before.Status.ID || c.cella.Count(cellastub.OpDelete) != 1 {
		t.Fatalf("the sandbox after the failure is %s, was %s, after %d deletes", after.Status.ID, before.Status.ID, c.cella.Count(cellastub.OpDelete))
	}
	if got := c.reasons(); !slices.Equal(got, []string{session.MachineAttached, session.MachineReplaced}) {
		t.Fatalf("session.machine reasons %v", got)
	}
	errs := c.events(session.TypeSessionError)
	var lost session.SessionError
	if len(errs) != 1 || errs[0].Decode(&lost) != nil || lost.Code != machine.CodeLost || lost.Message != runner.MessageMachineLost {
		t.Fatalf("session.error events %s", c.dump())
	}
	if !c.lastSaid(prompts.Text(prompts.TranscriptMachineReplaced)) {
		t.Fatal("the model does not read that the files are gone")
	}
}

// TestACommandKilledWithItsSandboxSaysSo: a command running when its
// sandbox is killed for memory gets a result that says it did not
// finish, that the machine ran out of memory, and that the machine runs
// again with its files, never that it did not start. The same sandbox
// runs the next command with the files there, the restart is recorded
// between the killed command and its result, and no error says the
// network could not be read (spec 009).
func TestACommandKilledWithItsSandboxSaysSo(t *testing.T) {
	c := newCloud(t, nil)
	c.cella.RestartFailed(true)
	c.drive("Make a file.", bash("toolu_1", "echo one > f.txt"), said("Made."))
	name := cella.SandboxName(c.s.ID)
	before, _ := c.cella.Sandbox(name)
	started := filepath.Join(c.cella.Workspace(name), "started")
	killed := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				killed <- c.cella.Kill(name, "OOMKilled")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		killed <- false
	}()
	c.drive("Install it.", bash("toolu_2", "echo installing; touch started; sleep 1; echo done"), bash("toolu_3", "cat f.txt"), said("Installed in smaller steps."))
	if !<-killed {
		t.Fatal("the sandbox was never killed under the command")
	}
	got := c.resultTexts()
	want := "installing\n" + prompts.Render(prompts.BashMachineStopped, prompts.Data{"OutOfMemory": true, "Restarted": true, "Lost": false, "Error": ""})
	if len(got) != 3 || got[1] != want || strings.Contains(got[1], "did not start") || !strings.Contains(got[2], "one") {
		t.Fatalf("the results are %q, want the second %q", got, want)
	}
	after, _ := c.cella.Sandbox(name)
	if after.Status.ID != before.Status.ID || c.cella.Count(cellastub.OpDelete) != 0 {
		t.Fatalf("the sandbox is %s, was %s, after %d deletes", after.Status.ID, before.Status.ID, c.cella.Count(cellastub.OpDelete))
	}
	if got := c.reasons(); !slices.Equal(got, []string{session.MachineAttached, session.MachineRestarted + ":" + session.MachineOutOfMemory}) {
		t.Fatalf("session.machine reasons %v", got)
	}
	machines, uses, results := c.events(session.TypeSessionMachine), c.events(session.TypeAgentToolUse), c.events(session.TypeToolResult)
	if uses[1].Seq >= machines[1].Seq || machines[1].Seq >= results[1].Seq {
		t.Fatalf("the restart at %d, the command at %d, its result at %d", machines[1].Seq, uses[1].Seq, results[1].Seq)
	}
	if n := len(c.events(session.TypeSessionError)); n != 0 {
		t.Fatalf("%d session.error events\n%s", n, c.dump())
	}
}

// TestACommandKilledOnACoreThatStartsNoFailedSandbox: on a core that
// refuses the start, the killed command's result says the machine could
// not be started again, the next command's failure is the machine's loss
// and no error of the network, nothing is deleted while the turn runs,
// and the next turn's machine is recorded as replaced with machine_lost
// (spec 009).
func TestACommandKilledOnACoreThatStartsNoFailedSandbox(t *testing.T) {
	c := newCloudOf(t, builderAgent, func(s *session.Session) {
		// A session on an allowlist reads the connections each command
		// made, which is where a lost machine was taken for a network
		// that could not be read.
		s.Network = &session.Network{Mode: session.NetworkAllowlist, Hosts: []string{"example.com"}}
	})
	c.drive("Make a file.", bash("toolu_1", "echo one > f.txt"), said("Made."))
	name := cella.SandboxName(c.s.ID)
	started := filepath.Join(c.cella.Workspace(name), "started")
	killed := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				killed <- c.cella.Kill(name, "OOMKilled")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		killed <- false
	}()
	c.drive("Install it.", bash("toolu_2", "echo installing; touch started; sleep 1"), bash("toolu_3", "cat f.txt"), said("The machine is lost."))
	if !<-killed {
		t.Fatal("the sandbox was never killed under the command")
	}
	got := c.resultTexts()
	want := "installing\n" + prompts.Render(prompts.BashMachineStopped, prompts.Data{"OutOfMemory": true, "Restarted": false, "Lost": true, "Error": ""})
	if len(got) != 3 || got[1] != want || !strings.Contains(got[2], "the session's machine is gone") {
		t.Fatalf("the results are %q, want the second %q", got, want)
	}
	if n := c.cella.Count(cellastub.OpDelete); n != 0 {
		t.Fatalf("%d deletes while the turn ran", n)
	}
	if n := len(c.events(session.TypeSessionError)); n != 0 {
		t.Fatalf("%d session.error events for a lost machine's calls\n%s", n, c.dump())
	}
	c.drive("Go on.", bash("toolu_4", "ls"), said("The files are gone."))
	if got := c.reasons(); !slices.Equal(got, []string{session.MachineAttached, session.MachineReplaced}) {
		t.Fatalf("session.machine reasons %v", got)
	}
	var lost session.SessionError
	if errs := c.events(session.TypeSessionError); len(errs) != 1 || errs[0].Decode(&lost) != nil || lost.Code != machine.CodeLost {
		t.Fatalf("session.error events %s", c.dump())
	}
}

// writer is an agent that writes, edits and reads files and runs
// commands in a Cella sandbox.
const writerAgent = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: writer
spec:
  model: {name: anthropic/claude-haiku-4.5}
  tools: [bash, write, edit, read]
  machine: {kind: cella}
`

// tool is a reply that calls a tool with input.
func tool(t *testing.T, id, name string, input map[string]any) luxstub.Reply {
	t.Helper()
	args, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: haiku, Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: name, Args: args}}}, StopReason: ir.StopToolUse}}
}

// TestAReplacedSandboxIsRebuiltFromTheLog: a hosted session with no
// repository whose sandbox failed on a core that does not start it again
// gets a new sandbox with the files its log holds: what it wrote and
// edited, and the file its answer linked, which a command had made and
// files.kept stored, sent to the sandbox as one archive. The file only a
// command made is not back. The machine is recorded as restored from the
// log, with no machine_lost, and the model reads what is back and what
// is not before its first command (spec 065).
func TestAReplacedSandboxIsRebuiltFromTheLog(t *testing.T) {
	c := newCloudOf(t, writerAgent, nil)
	c.drive("Build the app.",
		tool(t, "toolu_1", "write", map[string]any{"path": "src/app.js", "content": "console.log('hello');\n"}),
		tool(t, "toolu_2", "edit", map[string]any{"path": "src/app.js", "old_string": "hello", "new_string": "tide tables"}),
		tool(t, "toolu_3", "bash", map[string]any{"command": "mkdir -p out && printf 'tide report\\n' > out/report.txt && echo generated > out/data.bin"}),
		said("The app is built.\n\n[The report](out/report.txt)"),
	)
	name := cella.SandboxName(c.s.ID)
	before, _ := c.cella.Sandbox(name)
	c.cella.SetFailed(name, "OOMKilled")
	c.drive("Show me the app.", bash("toolu_4", "cat src/app.js out/report.txt; ls out"), said("The app and the report are back; the data is not."))
	after, _ := c.cella.Sandbox(name)
	if after.Status.ID == before.Status.ID || c.cella.Count(cellastub.OpDelete) != 1 {
		t.Fatalf("the sandbox is %s, was %s, after %d deletes", after.Status.ID, before.Status.ID, c.cella.Count(cellastub.OpDelete))
	}
	got := c.results()
	if last := got[len(got)-1]; !strings.Contains(last, "console.log('tide tables');\ntide report\nreport.txt\n") || strings.Contains(last, "data.bin") {
		t.Fatalf("the new sandbox's files read %q", last)
	}
	machines := c.events(session.TypeSessionMachine)
	var rebuilt session.SessionMachine
	if len(machines) != 2 || machines[1].Decode(&rebuilt) != nil || rebuilt.Reason != session.MachineRestored || rebuilt.Restore == nil {
		t.Fatalf("session.machine events %s", c.dump())
	}
	if r := rebuilt.Restore; r.Source != session.RestoreFromLog || r.Files != 2 || r.Unverified != 0 || r.Omitted != 0 || r.Commands != 1 ||
		!slices.Equal(r.Paths, []string{"out/report.txt", "src/app.js"}) {
		t.Fatalf("the restore record %+v", r)
	}
	if n := len(c.events(session.TypeSessionError)); n != 0 {
		t.Fatalf("%d session.error events\n%s", n, c.dump())
	}
	// The record, and so the note, precede the result of the command
	// that opened the sandbox.
	results := c.events(session.TypeToolResult)
	if machines[1].Seq >= results[len(results)-1].Seq || !c.lastSaid("was rebuilt from this conversation's record") || !c.lastSaid("- out/report.txt\n- src/app.js\n") {
		t.Fatal("the model does not read what is back before it goes on")
	}
}
