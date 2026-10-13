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

// TestARebuiltWorkingDirectoryIsSaid: a session.machine with reason
// restored whose restore names the log reads, in the session's own
// thread, as a working directory rebuilt from the conversation's record,
// with the files that are back, the ones that may be out of date, the
// ones that are missing, how many more of each the record counts than it
// names, and how many commands ran since. A machine a checkpoint
// restored holds every file and reads as nothing (spec 065).
func TestARebuiltWorkingDirectoryIsSaid(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "Go on.", 1)
	b.add(session.TypeSessionMachine, "", 1, session.SessionMachine{
		Machine: session.AttachedMachine{Kind: "cella", ID: "sbx_2", Workdir: "/workspace"}, Reason: session.MachineRestored,
		Checkpoint: &session.CheckpointRef{Commit: "1a2b3c4"},
	})
	b.add(session.TypeSessionMachine, "", 1, session.SessionMachine{
		Machine: session.AttachedMachine{Kind: "cella", ID: "sbx_3", Workdir: "/workspace"}, Reason: session.MachineRestored,
		Restore: &session.Restore{
			Source: session.RestoreFromLog, Files: 5, Bytes: 1200, Unverified: 3, Omitted: 1, Commands: 2,
			Paths: []string{"app.js", "package.json"}, UnverifiedPaths: []string{"package.json"},
			OmittedFiles: []session.OmittedFile{{Path: "rows.csv", Reason: session.OmittedUnknown}},
		},
	})
	sub := b.thread("", "Review it.", 1)
	tr, err := session.Fold(b.events, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user: Go on.", "user: The working directory is on a new machine and was rebuilt from this conversation's record: what the write and edit tools wrote, the files an answer showed or linked, and files that were read in full. 5 files are back:\n" +
		"- app.js\n- package.json\n- and 3 more\n" +
		"A command changed these after the record last held them, so they may be out of date:\n- package.json\n- and 2 more\n" +
		"These were there and could not be rebuilt, so they are missing:\n- rows.csv\n" +
		"Nothing that only a command made is back: generated files, downloads, installed packages and build output are missing, and so is every process that was running. 2 commands ran after the first of these files was made. Regenerate what you need before you rely on it, and tell the person what is back and what is not."}
	if got := texts(tr); !slices.Equal(got, want) {
		t.Fatalf("the transcript reads\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	thread, err := session.Fold(b.events, sub)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(thread); !slices.Equal(got, []string{"user: Review it."}) {
		t.Fatalf("a thread's transcript reads %q", got)
	}
}

// TestARestoreFromTheCopyIsSaid: a machine restored from the copy the
// session keeps in its folder reads, in the session's own thread, what
// the copy gave and when it was saved, what the log added, what the copy
// left out and does not keep, and that dependencies are reinstalled from
// manifests; one restored from the copy only after it could not be
// written reads as a rebuild from the log (spec 073).
func TestARestoreFromTheCopyIsSaid(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "Go on.", 1)
	b.add(session.TypeSessionMachine, "", 1, session.SessionMachine{
		Machine: session.AttachedMachine{Kind: "cella", ID: "sbx_2", Workdir: "/workspace"}, Reason: session.MachineRestored,
		Restore: &session.Restore{
			Source: session.RestoreFromFolder, Files: 1, Bytes: 20, Commands: 1, Paths: []string{"notes.md"},
			Copy: &session.RestoreCopy{Files: 412, Bytes: 18234011, Turn: 7, Skipped: 1, SkippedFiles: []session.OmittedFile{{Path: "data/raw.parquet", Reason: session.WorkTooLarge}}},
		},
	})
	tr, err := session.Fold(b.events, "")
	if err != nil {
		t.Fatal(err)
	}
	got := texts(tr)
	if len(got) != 2 {
		t.Fatalf("the transcript reads %q", got)
	}
	for _, want := range []string{
		"was restored from the copy this conversation keeps of it, as it was saved at the end of turn 7: 412 files.",
		"Then this was brought up to date from this conversation's record",
		"- notes.md\n",
		"These were too large for the copy, or past what it holds, so they are missing:\n- data/raw.parquet\n",
		"(node_modules, bower_components, .venv,",
		"files larger than 32 MiB",
		"1 command ran after the copy was saved",
		"Reinstall dependencies from the project's manifests",
	} {
		if !strings.Contains(got[1], want) {
			t.Errorf("the note does not say %q:\n%s", want, got[1])
		}
	}
	if strings.Contains(got[1], ".git,") {
		t.Errorf("the note names .git among the dependencies:\n%s", got[1])
	}
	b.add(session.TypeSessionMachine, "", 2, session.SessionMachine{
		Machine: session.AttachedMachine{Kind: "cella", ID: "sbx_3", Workdir: "/workspace"}, Reason: session.MachineRestored,
		Restore: &session.Restore{Source: session.RestoreFromLog, Files: 1, Paths: []string{"a.txt"}, Copy: &session.RestoreCopy{Failed: "the folder is away"}},
	})
	tr, err = session.Fold(b.events, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(tr); len(got) != 3 || !strings.Contains(got[2], "was rebuilt from this conversation's record") || strings.Contains(got[2], "the folder is away") {
		t.Fatalf("a copy that could not be written reads %q", got)
	}
}
