// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// texts are the text blocks of a transcript's messages, each led by its
// message's role.
func texts(tr session.Transcript) []string {
	var out []string
	for _, m := range tr.Messages {
		for _, b := range m.Blocks {
			switch {
			case b.Type == ir.BlockText:
				out = append(out, string(m.Role)+": "+b.Text)
			case b.ToolResult != nil:
				out = append(out, string(m.Role)+": result of "+b.ToolResult.ToolUseID)
			}
		}
	}
	return out
}

// TestAMachineThatCameBackOrWasReplacedIsSaid: a session.machine with
// reason restarted reads in the session's own thread as a machine that
// stopped and was started again with its files, for memory when the
// record says so, after the result of the call it was appended beside;
// one with reason replaced reads as a machine that was lost with its
// files. The first attachment and a handoff read as nothing, a thread's
// transcript holds neither note, and the system parts are the latest
// record's whatever its reason (spec 009).
func TestAMachineThatCameBackOrWasReplacedIsSaid(t *testing.T) {
	b := &logb{t: t}
	machine := func(reason, cause, id string) {
		b.add(session.TypeSessionMachine, "", 1, session.SessionMachine{
			Machine: session.AttachedMachine{Kind: "cella", ID: id, Workdir: "/workspace"}, Reason: reason, Cause: cause,
			Context: "<context>\nWorking directory: /workspace\n</context>",
		})
	}
	b.user(ada, "Build the app.", 1)
	machine(session.MachineAttached, "", "sbx_1")
	b.assistant("", 1, false, use("toolu_1", "bash"))
	b.toolUse("", 1, "toolu_1")
	machine(session.MachineRestarted, session.MachineOutOfMemory, "sbx_1")
	b.result("", 1, "toolu_1", "The command did not finish.")
	sub := b.thread("", "Review it.", 1)
	b.assistant(sub, 1, false, txt("Reviewed."))
	b.assistant("", 1, false, txt("Installed."))
	b.user(ada, "Go on.", 2)
	machine(session.MachineRestarted, "", "sbx_1")
	b.assistant("", 2, false, txt("Going on."))
	b.user(ada, "And now?", 3)
	machine(session.MachineHandoff, "", "sbx_2")
	machine(session.MachineReplaced, "", "sbx_3")
	b.assistant("", 3, false, txt("The files are gone."))

	tr, err := session.Fold(b.events, "")
	if err != nil {
		t.Fatal(err)
	}
	const (
		memory   = "user: The session's machine stopped because it ran out of memory and was started again. The files in the working directory are kept. Every process that was running on it, background jobs and servers included, is gone, so start again what you still need."
		noCause  = "user: The session's machine stopped and was started again. The files in the working directory are kept. Every process that was running on it, background jobs and servers included, is gone, so start again what you still need."
		replaced = "user: The session's machine was lost and replaced with a new, empty one. The files that commands and file tools put in the working directory before this point are gone, and so is every process that was running. Tell the person plainly that the files are gone before you go on."
	)
	want := []string{
		"user: Build the app.",
		"user: result of toolu_1", memory,
		"assistant: Installed.",
		"user: Go on.", noCause,
		"assistant: Going on.",
		"user: And now?", replaced,
		"assistant: The files are gone.",
	}
	if got := texts(tr); !slices.Equal(got, want) {
		t.Fatalf("the transcript reads\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The note after a call's result is in the result's own message, so
	// the call and its result stay adjacent.
	if m := tr.Messages[2]; m.Role != ir.RoleUser || len(m.Blocks) != 2 || m.Blocks[0].ToolResult == nil {
		t.Fatalf("the message after the call is %+v", m)
	}
	if len(tr.System) != 1 || tr.System[0].Kind != session.PartContext || tr.System[0].Context == "" {
		t.Fatalf("the system parts are %+v", tr.System)
	}
	thread, err := session.Fold(b.events, sub)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(thread); !slices.Equal(got, []string{"user: Review it.", "assistant: Reviewed."}) {
		t.Fatalf("a thread's transcript reads %q", got)
	}
}
