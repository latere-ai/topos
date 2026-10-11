// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
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
