// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// skill and todo stand in for session.Skill and tools.Todo, which this
// package cannot import: a template reads the fields by name.
type skill struct{ Name, Description, Path string }

type todo struct{ ID, Content, Status string }

// repository stands in for the harness's line of a session's repository.
type repository struct{ URL, Ref, Branch, Dir string }

// attachment stands in for session.Attachment.
type attachment struct {
	Path, MediaType string
	Size            int64
}

// textCase is one text rendered with representative data. want is the
// exact text, written as the expression that produced it before the text
// moved into its file; wantSHA pins a long document by the SHA-256 of
// its text instead.
type textCase struct {
	name    Name
	data    Data
	want    string
	wantSHA string
}

var textCases = func() []textCase {
	const p = "/work/app/main.go"
	sender := func(name, subject string) Data { return Data{"Name": name, "Subject": subject} }
	transcript := "The conversation so far:\n" +
		fmt.Sprintf("\n%s: %s\n", "Person", "Fix it.") +
		fmt.Sprintf("\n%s: %s\n", "Agent", "Reading.") +
		fmt.Sprintf("\nAgent called %s with %s\n", "read", `{"path":"a.go"}`) +
		fmt.Sprintf("\nThe call returned:\n%s\n", "package a") +
		fmt.Sprintf("\nThe call returned:\n%s\n", "xxxx"+"\n[cut]")
	entries := []Data{
		{"Kind": "text", "Agent": false, "Text": "Fix it."},
		{"Kind": "text", "Agent": true, "Text": "Reading."},
		{"Kind": "call", "Name": "read", "Args": `{"path":"a.go"}`},
		{"Kind": "result", "Text": "package a", "Cut": false},
		{"Kind": "result", "Text": "xxxx", "Cut": true},
	}
	contextData := func(cella bool, env string, git bool, commits []string) Data {
		return Data{
			"Workdir": "/work/app", "OS": "linux", "Arch": "arm64", "Cella": cella, "Environment": env,
			"Date": "2026-09-27", "Git": git, "Branch": "main", "Head": "1a2b3c4", "Modified": 2, "Untracked": 1, "Commits": commits,
		}
	}
	page := func(ctype string, truncated bool) Data {
		return Data{"URL": "https://example.com/final", "Status": 404, "ContentType": ctype, "Body": "no such page\n", "Truncated": truncated, "Max": 10}
	}
	return []textCase{
		{name: Compaction, wantSHA: "8d62d8134b413672d87d57e05c20f135933fa3f0095199280438e3170f879bf5"},

		{name: AdvisorInstructions, want: "You advise another agent. You see the conversation it has had so far and the question it asks. You act on nothing and have no tools: read what it did, say what is wrong or missing, and what it should do next, briefly and concretely."},
		{name: AdvisorTranscript, data: Data{"Entries": entries}, want: transcript},
		{name: AdvisorTranscript, data: Data{"Entries": []Data(nil)}, want: "The conversation so far:\n"},
		{name: AdvisorRequest, data: Data{"Transcript": transcript, "Question": "Is it right?"}, want: transcript + "\n\nThe question: " + "Is it right?"},
		{name: AdvisorRequest, data: Data{"Transcript": transcript, "Question": ""}, want: transcript + "\n\nReview the work so far."},

		{name: ToolRead, wantSHA: "78568219c234ed1777a6fa6de80bdbc8984e4d80ae2faf7b280c8159dd126df8"},
		{name: ToolWrite, wantSHA: "57efeb49ca78c1b5d8147183989ad3f539fd5dfacc1b08d7d5aa58fae6f73bf2"},
		{name: "tools/write-v1", wantSHA: "1b7f34af9f13f7c04c8baf8fa8213be0eb86e68d0fecb59d5a2944c55c93956f"},
		{name: ToolEdit, wantSHA: "3e3d91cd2333c7d13bfa2853bc4067232209b5e6ce48db68bc5c945ff8027622"},
		{name: "tools/edit-v1", wantSHA: "ab8d937843592cffe0a52d6b52e80b7faa2736cfa728321f4f37d728cb91a96c"},
		{name: ToolBash, wantSHA: "64a815c0147fa31ee847eb6d258b307f97c0951756a60e4fbd83449036eea364"},
		{name: ToolGrep, wantSHA: "8d58c89479ca66083587ec0d96c2f9e336dc6cc1287ceed85895ac3fbc2eab97"},
		{name: ToolGlob, wantSHA: "a52b678c89e92fb2d9d977a598e0850e84af774923efb10e42dedf09f2abeb70"},
		{name: ToolWebFetch, wantSHA: "4b672af878d18ae215331e1e1fdc57a0b188e5a1577012d1ab15088327370619"},
		{name: ToolTodo, wantSHA: "2eeebc6268388102bf6ba67519e6b902c3b0f6b7c25278343e963413120b24b5"},
		{name: ToolSpawn, want: "Start a thread that runs one of your subagents on a task, on this same machine, and return its final answer. The thread does not see this conversation: give it a complete task. Several spawn calls in one step run their threads at once. Send the thread more work later with message."},
		{name: ToolMessage, want: "Send a message to a thread you spawned and return its answer. Set end to true to end the thread after this turn."},
		{name: ToolAdvisor, want: "Ask a stronger model to review your work so far. It sees this conversation and your question, acts on nothing, and answers with advice. Use it before a hard decision or when you are stuck."},

		{name: "harness/machine-host-v1", want: "This is the person's own computer. Files you delete or overwrite outside version control are gone, and commands run with the person's own account. Prefer reversible changes, and ask before a command that deletes data, rewrites history, or reaches another system."},
		{name: "harness/machine-sandbox-v1", want: "This is a disposable sandbox made for this session. Its files are yours to change, and it holds no credential; what leaves it goes through the session's network rules."},
		{name: "harness/threads-v1", want: strings.TrimPrefix("\n\n# Threads\n\n`spawn` starts a thread that runs another agent on a task you give it, on this same machine, and returns its final answer. `message` sends a message to a running thread. `advisor` asks a stronger model to review your work so far. Give a thread a complete task: it does not see this conversation.", "\n\n")},
		{name: "harness/memory-v1", want: strings.TrimPrefix("\n\n# Memory\n\nMemory is files in the directories the memory notes name. Read them with the file tools at the start of a task, and write what later sessions should know as you learn it.", "\n\n")},
		{name: "harness/git-v1", want: strings.TrimPrefix("\n\n# Git\n\nCommit your work on the session's branch with a message that says what changed and why. Never push to a protected branch, and never rewrite a commit you did not make in this session.", "\n\n")},

		{name: TranscriptTruncated, want: "Your previous response was cut off at the output limit. Continue from where it stopped."},
		{name: TranscriptSummary, data: Data{"Summary": "The parser was fixed."}, want: "Summary of the conversation so far:" + "\n\n" + "The parser was fixed."},
		{name: TranscriptCleared, want: "[cleared to save context; run the tool again if the result is needed]"},
		{name: TranscriptSender, data: sender("Ada", "usr_ada"), want: "Message from " + cmp.Or("Ada", "usr_ada", "someone") + ":"},
		{name: TranscriptSender, data: sender("", "usr_ada"), want: "Message from " + cmp.Or("", "usr_ada", "someone") + ":"},
		{name: TranscriptSender, data: sender("", ""), want: "Message from " + cmp.Or("", "", "someone") + ":"},
		{name: TranscriptInterrupt, data: sender("Ada", "usr_ada"), want: cmp.Or("Ada", "usr_ada", "someone") + " interrupted the previous turn."},
		{name: TranscriptInterrupt, data: sender("", ""), want: cmp.Or("", "", "someone") + " interrupted the previous turn."},
		{name: TranscriptThreadMessage, data: Data{"FromName": "reviewer", "From": "evt_1"}, want: "Message from thread " + cmp.Or("reviewer", "evt_1", "session") + ":"},
		{name: TranscriptThreadMessage, data: Data{"FromName": "", "From": ""}, want: "Message from thread " + cmp.Or("", "", "session") + ":"},
		{name: TranscriptRewound, data: Data{"Turn": 3}, want: "The working directory was restored to its state at the end of turn " + strconv.Itoa(3) + "."},

		{name: CallDenied, data: Data{"Note": ""}, want: "A person denied this call."},
		{name: CallDenied, data: Data{"Note": "not that"}, want: "A person denied this call." + " Their note: " + "not that"},
		{name: CallUnknownTool, data: Data{"Name": "grep"}, want: "No tool named " + "grep" + "."},
		{name: CallUnknownEffect, want: "The runner stopped while this call ran. Its effects are unknown; inspect the machine before repeating it."},
		{name: CallCanceled, want: "The call was canceled before it finished."},
		{name: CallFailed, data: Data{"Error": "disk full"}, want: "The tool failed: " + "disk full"},
		{name: CallMachineUnavailable, data: Data{"Error": "no sandbox"}, want: "The machine this tool acts on could not be started, so the call did not run: " + "no sandbox"},
		{name: CallPlanMode, want: "Plan mode: only read-only tools run."},
		{name: CallAboveBlock, want: "above the block threshold"},

		{name: ThreadIdle, data: Data{"Thread": "evt_7"}, want: "Thread " + "evt_7" + " is idle; send it more work with message."},
		{name: ThreadStopped, data: Data{"Thread": "evt_7", "Reason": "error", "Detail": "model_error"}, want: fmt.Sprintf("Thread %s stopped: %s %s", "evt_7", "error", "model_error")},
		{name: AdvisorStopped, data: Data{"Reason": "turn_limit", "Detail": ""}, want: fmt.Sprintf("The advisor stopped: %s %s", "turn_limit", "")},
		{name: ThreadBranch, data: Data{"Branch": "agents/reviewer/ses_1.evt_7", "Commit": "1a2b3c4"}, want: fmt.Sprintf("Its work is on branch %s at %s; merge it with git.", "agents/reviewer/ses_1.evt_7", "1a2b3c4")},
		{name: ThreadUncommitted, want: "The thread works from the last commit: your uncommitted changes are not in its worktree."},
		{name: ThreadCommitFailed, data: Data{"Branch": "agents/r/b", "Output": "fatal: no"}, want: "The thread's work could not be committed to " + "agents/r/b" + ": " + "fatal: no"},
		{name: ThreadUnknownSubagent, data: Data{"Agent": `no"body`}, want: fmt.Sprintf("no subagent named %q", `no"body`)},
		{name: ThreadDepthExceeded, data: Data{"Depth": 2}, want: fmt.Sprintf("a thread at depth %d may not spawn", 2)},
		{name: ThreadNoWorktrees, want: "this machine keeps no worktrees; spawn with isolation shared"},
		{name: ThreadTooMany, data: Data{"Max": 8}, want: fmt.Sprintf("the session already runs %d threads", 8)},
		{name: ThreadNotSpawned, data: Data{"Thread": "evt_9"}, want: fmt.Sprintf("no thread %s that this thread spawned", "evt_9")},
		{name: ThreadEnded, data: Data{"Thread": "evt_9"}, want: fmt.Sprintf("thread %s has ended", "evt_9")},
		{name: ThreadSubagentGone, data: Data{"Thread": "evt_9", "Agent": "reviewer"}, want: fmt.Sprintf("thread %s runs %q, which is no longer a subagent", "evt_9", "reviewer")},

		{name: RegistryUnknownTool, data: Data{"Name": "nope", "Available": "read, write"}, want: fmt.Sprintf("No tool named %s. Available tools: %s.", "nope", "read, write")},
		{name: RegistryUnknownTool, data: Data{"Name": "nope", "Available": ""}, want: fmt.Sprintf("No tool named %s. Available tools: %s.", "nope", "")},
		{name: RegistryInvalidInput, data: Data{"Tool": "read", "Problems": "/path: expected string, got number"}, want: fmt.Sprintf("The input does not match the schema of %s:\n%s", "read", "/path: expected string, got number")},
		{name: RegistryInvalidJSON, data: Data{"Tool": "bash", "Problem": "unexpected EOF", "Excerpt": `{"command":"rm a`}, want: fmt.Sprintf("The arguments of %s were not valid JSON (%s). They began:\n%s\nCall %s again with its arguments as one JSON object that matches its schema.", "bash", "unexpected EOF", `{"command":"rm a`, "bash")},
		{name: OutputSpilled, data: Data{"Omitted": 73182, "Path": "/spill/tool-toolu_1.txt"}, want: strings.Trim(fmt.Sprintf("\n[... %d bytes omitted; the full output is in %s ...]\n", 73182, "/spill/tool-toolu_1.txt"), "\n")},

		{name: FileChanged, data: Data{"Path": p}, want: p + " changed since it was last read; read it again before writing."},
		{name: FileUnread, data: Data{"Path": p}, want: p + " exists and this thread has not read it; read it before writing to it."},
		{name: FileOutside, data: Data{"Path": p}, want: p + " is outside the working directory."},
		{name: FileDenied, data: Data{"Path": p}, want: p + " is on the credential deny-list; the tools do not open it."},
		{name: FileNotFound, data: Data{"Path": p}, want: p + " does not exist."},
		{name: FilePermission, data: Data{"Path": p}, want: p + " is not accessible: permission denied."},
		{name: FileError, data: Data{"Path": p, "Error": "boom"}, want: fmt.Sprintf("%s: %v.", p, errors.New("boom"))},
		{name: FileDirectory, data: Data{"Path": p}, want: p + " is a directory; write and edit change files."},

		{name: ReadDirectory, data: Data{"Path": p}, want: p + " is a directory; use glob to list the files in it."},
		{name: ReadBinary, data: Data{"Path": p}, want: p + " is a binary file; read shows text files and PNG, JPEG, GIF and WebP images. Inspect it with bash."},
		{name: ReadEmpty, data: Data{"Path": p}, want: p + " is empty."},
		{name: ReadPastEnd, data: Data{"Path": p, "Lines": "1 line", "Offset": 5}, want: fmt.Sprintf("%s has %s; offset %d is past its end.", p, "1 line", 5)},
		{name: ReadLineCut, data: Data{"Max": 2000}, want: strings.TrimPrefix(fmt.Sprintf(" [... line cut at %d characters]", 2000), " ")},
		{name: ReadMoreLines, data: Data{"From": 2, "To": 3, "Total": 10, "Next": 4}, want: strings.TrimSuffix(fmt.Sprintf("[lines %d to %d of %d; read on with offset %d]\n", 2, 3, 10, 4), "\n")},
		{name: ReadImageTooLarge, data: Data{"Path": p, "Size": int64(5242898), "Max": 5}, want: fmt.Sprintf("%s is an image of %d bytes, over the %d MiB limit of read.", p, int64(5242898), 5)},

		{name: WriteDone, data: Data{"Existed": false, "Path": p, "Bytes": "1 byte", "Lines": "1 line"}, want: fmt.Sprintf("%s %s (%s, %s).", "Created", p, "1 byte", "1 line")},
		{name: WriteDone, data: Data{"Existed": true, "Path": p, "Bytes": "3 bytes", "Lines": "0 lines"}, want: fmt.Sprintf("%s %s (%s, %s).", "Wrote", p, "3 bytes", "0 lines")},

		{name: EditEmptyOld, want: "old_string is empty; give the exact text to replace, or use write to create a file."},
		{name: EditSame, want: "old_string and new_string are the same; there is nothing to change."},
		{name: EditMissing, data: Data{"Path": p}, want: p + " does not exist; use write to create it."},
		{name: EditBinary, data: Data{"Path": p}, want: p + " is a binary file; edit changes text files."},
		{name: EditNotFound, data: Data{"Path": p}, want: fmt.Sprintf("old_string does not occur in %s. Read the file again and copy the text exactly, whitespace and indentation included.", p)},
		{name: EditAmbiguous, data: Data{"Count": 3, "Path": p}, want: fmt.Sprintf("old_string occurs %d times in %s. Add surrounding lines to make it unique, or set replace_all to replace every occurrence.", 3, p)},
		{name: EditDoneOne, data: Data{"Path": p, "Line": 4}, want: fmt.Sprintf("Edited %s: replaced 1 occurrence at line %d.", p, 4)},
		{name: EditDoneMany, data: Data{"Path": p, "Count": 3, "Line": 4}, want: fmt.Sprintf("Edited %s: replaced %d occurrences, the first at line %d.", p, 3, 4)},

		{name: BashEmpty, want: "The command is empty."},
		{name: BashDirGone, data: Data{"Dir": "/work/gone", "Workdir": "/work"}, want: strings.TrimSuffix(fmt.Sprintf("[%s no longer exists; the command ran in the working directory %s]\n", "/work/gone", "/work"), "\n")},
		{name: BashNotStarted, data: Data{"Error": "no shell"}, want: fmt.Sprintf("The command did not start: %v.", errors.New("no shell"))},
		{name: BashBackground, data: Data{"PID": 4242, "Log": "/spill/jobs/job-1.log"}, want: fmt.Sprintf("%sStarted in the background as pid %d; its output goes to %s. Read the log with read, and stop the job with bash: kill -TERM -%d.", "", 4242, "/spill/jobs/job-1.log", 4242)},
		{name: BashNoOutput, want: strings.TrimSuffix("(no output)\n", "\n")},
		{name: BashTimeout, data: Data{"Timeout": (200 * time.Millisecond).String()}, want: fmt.Sprintf("The command passed its timeout of %s and was killed with its process group.", 200*time.Millisecond)},
		{name: BashCanceled, want: "The command was canceled and killed with its process group."},
		{name: BashExitCode, data: Data{"Code": 3}, want: fmt.Sprintf("Exit code: %d", 3)},

		{name: GrepNoMatches, want: "No matches."},
		{name: GrepMore, data: Data{"Limit": 250}, want: strings.TrimSuffix(fmt.Sprintf("[... more results past head_limit %d; narrow the pattern, the path or the glob, or raise head_limit]\n", 250), "\n")},
		{name: GlobNoMatches, want: "No files match."},
		{name: GlobMore, data: Data{"Limit": 1000}, want: strings.TrimSuffix(fmt.Sprintf("[... more than %d paths match; narrow the pattern or the path]\n", 1000), "\n")},
		{name: SearchFailed, data: Data{"Error": "bad mode"}, want: "The search failed: " + "bad mode" + "."},

		{name: TodoTooMany, data: Data{"Count": 101, "Max": 100}, want: fmt.Sprintf("The list has %d items; it holds at most %d.", 101, 100)},
		{name: TodoNoID, data: Data{"Index": 2}, want: fmt.Sprintf("Item %d has no id.", 2)},
		{name: TodoDuplicate, data: Data{"ID": "a"}, want: fmt.Sprintf("The id %q is used twice; each item needs its own.", "a")},
		{name: TodoNoContent, data: Data{"ID": "a"}, want: fmt.Sprintf("Item %q has no content.", "a")},
		{name: TodoBadStatus, data: Data{"ID": "a", "Status": "done"}, want: fmt.Sprintf("Item %q has the status %q; a status is pending, in_progress or completed.", "a", "done")},
		{name: TodoEmpty, want: "The todo list is empty."},
		{name: TodoList, data: Data{"Done": 1, "Total": 2, "Items": []todo{{"a", "first", "completed"}, {"b", "second", "pending"}}},
			want: fmt.Sprintf("The todo list, %d of %d completed:\n", 1, 2) + fmt.Sprintf("[%s] %s: %s\n", "completed", "a", "first") + fmt.Sprintf("[%s] %s: %s\n", "pending", "b", "second")},

		{name: FetchNotURL, data: Data{"URL": "ftp://x"}, want: fmt.Sprintf("%q is not an http or https URL.", "ftp://x")},
		{name: FetchUnavailable, want: "Web fetch is not available on this machine."},
		{name: FetchCanceled, data: Data{"URL": "https://example.com/a"}, want: fmt.Sprintf("The fetch of %s was canceled.", "https://example.com/a")},
		{name: FetchTimeout, data: Data{"URL": "https://example.com/a", "Timeout": (30 * time.Second).String()}, want: fmt.Sprintf("The fetch of %s passed its timeout of %s.", "https://example.com/a", 30*time.Second)},
		{name: FetchFailed, data: Data{"URL": "https://example.com/a", "Error": "refused"}, want: fmt.Sprintf("The fetch of %s failed: %s.", "https://example.com/a", "refused")},
		{name: FetchNotText, data: Data{"URL": "https://e/x", "ContentType": "image/png"}, want: fmt.Sprintf("%s returned %s, which is not text; web_fetch returns text and HTML pages.", "https://e/x", cmp.Or("image/png", "no content type"))},
		{name: FetchNotText, data: Data{"URL": "https://e/x", "ContentType": ""}, want: fmt.Sprintf("%s returned %s, which is not text; web_fetch returns text and HTML pages.", "https://e/x", cmp.Or("", "no content type"))},
		{name: FetchPage, data: page("text/plain", true), want: fmt.Sprintf("URL: %s\nStatus: %d\n", "https://example.com/final", 404) + fmt.Sprintf("Content-Type: %s\n", "text/plain") + "\n" + "no such page\n" + fmt.Sprintf("[the body passed %d MiB and was cut there]\n", 10)},
		{name: FetchPage, data: page("", false), want: fmt.Sprintf("URL: %s\nStatus: %d\n", "https://example.com/final", 404) + "\n" + "no such page\n"},

		{name: ContextBlock, data: contextData(false, "", false, nil), want: "<context>\n" + fmt.Sprintf("Working directory: %s\n", "/work/app") + fmt.Sprintf("Platform: %s/%s\n", "linux", "arm64") +
			fmt.Sprintf("Machine: %s\n", "host") + fmt.Sprintf("Date: %s\n", "2026-09-27") + "</context>"},
		{name: ContextBlock, data: contextData(true, "gpu", true, []string{"1a2b3c4 parse: reject empty input", "0f0f0f0 init"}), want: "<context>\n" + fmt.Sprintf("Working directory: %s\n", "/work/app") + fmt.Sprintf("Platform: %s/%s\n", "linux", "arm64") +
			fmt.Sprintf("Machine: %s\n", "Cella sandbox"+" ("+"gpu"+")") + fmt.Sprintf("Date: %s\n", "2026-09-27") +
			fmt.Sprintf("Git: branch %s at %s, %s modified, %s untracked\n", "main", "1a2b3c4", strconv.Itoa(2), strconv.Itoa(1)) +
			"Recent commits:\n" + "- " + "1a2b3c4 parse: reject empty input" + "\n" + "- " + "0f0f0f0 init" + "\n" + "</context>"},
		{name: ContextBlock, data: contextData(true, "", true, nil), want: "<context>\n" + fmt.Sprintf("Working directory: %s\n", "/work/app") + fmt.Sprintf("Platform: %s/%s\n", "linux", "arm64") +
			fmt.Sprintf("Machine: %s\n", "Cella sandbox") + fmt.Sprintf("Date: %s\n", "2026-09-27") +
			fmt.Sprintf("Git: branch %s at %s, %s modified, %s untracked\n", "main", "1a2b3c4", strconv.Itoa(2), strconv.Itoa(1)) + "</context>"},
		{name: TranscriptAttachments, data: Data{"Attachments": []attachment{{"attachments/sales.csv", "text/csv", 1204}, {"attachments/notes-2.md", "text/markdown", 9}}},
			want: "Attached files, in the working directory:\n" + "- attachments/sales.csv (text/csv, 1204 bytes)\n" + "- attachments/notes-2.md (text/markdown, 9 bytes)\n"},
		{name: TranscriptImageUnseen, want: "[An image is attached here, but this model does not take images, so it cannot see it.]"},
		{name: ContextRepositories, data: Data{"Repositories": []repository{
			{"https://git.example/acme/web.git", "main", "agents/coder/ses_1", ""},
			{"https://git.example/acme/api.git", "", "agents/coder/ses_1", "api"},
		}}, want: "<context>\n" + "Repositories, cloned the first time a file or command tool runs:\n" +
			"- https://git.example/acme/web.git at main, on branch agents/coder/ses_1, into the working directory\n" +
			"- https://git.example/acme/api.git, on branch agents/coder/ses_1, into api/ in the working directory\n" + "</context>"},
		{name: ContextInstructions, data: Data{"Path": `/w/AGENTS "x".md`, "Body": "Be terse."}, want: fmt.Sprintf("<instructions path=%q>\n%s\n</instructions>", `/w/AGENTS "x".md`, "Be terse.")},
		{name: InstructionCut, want: strings.TrimPrefix("\n[the file is longer than 64 KiB and was cut here]", "\n")},
		{name: InstructionsTotalCut, want: strings.TrimPrefix("\n[the instruction files pass 256 KiB together and were cut here]", "\n")},
		{name: ContextSkills, data: Data{"Skills": []skill{{"one", "Does one.", "/s/one/SKILL.md"}, {"two", "Does two: yes.", "/s/two/SKILL.md"}}},
			want: "<skills>\n" + fmt.Sprintf("- name: %s\n  description: %s\n  path: %s\n", "one", "Does one.", "/s/one/SKILL.md") + fmt.Sprintf("- name: %s\n  description: %s\n  path: %s\n", "two", "Does two: yes.", "/s/two/SKILL.md") + "</skills>"},
		{name: ContextSkills, data: Data{"Skills": []skill(nil)}, want: "<skills>\n" + "</skills>"},
		{name: ContextMemory, data: Data{"Name": "notes", "ReadOnly": true, "Path": "/mem/notes", "Description": "Team notes."}, want: fmt.Sprintf("Memory store %s (%s) is at %s", "notes", "read-only", "/mem/notes") + ": " + "Team notes."},
		{name: ContextMemory, data: Data{"Name": "scratch", "ReadOnly": false, "Path": "/mem/s", "Description": ""}, want: fmt.Sprintf("Memory store %s (%s) is at %s", "scratch", "read-write", "/mem/s")},
	}
}()

// TestEveryTextRendersItsCurrentBytes renders every text with
// representative data and pins the result: a change of wording shows here
// as a test diff, and belongs in a new version of the file.
func TestEveryTextRendersItsCurrentBytes(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range textCases {
		covered[string(c.name)] = true
		got, err := Execute(c.name, c.data)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.wantSHA != "" {
			if sum := sha256.Sum256([]byte(got)); hex.EncodeToString(sum[:]) != c.wantSHA {
				t.Errorf("%s changed:\n%s", c.name, got)
			}
		} else if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if _, static := texts.static[string(c.name)]; static && Text(c.name) != got {
			t.Errorf("%s: Text %q, Execute %q", c.name, Text(c.name), got)
		}
		keys := slices.Sorted(maps.Keys(c.data))
		if uses := slices.Sorted(maps.Keys(fields(t, texts, string(c.name)))); !slices.Equal(keys, uses) {
			t.Errorf("%s is rendered with %v and uses %v", c.name, keys, uses)
		}
	}
	for _, n := range names(texts) {
		if !covered[n] && !strings.HasPrefix(n, "harness/harness-v") {
			t.Errorf("%s has no case", n)
		}
	}
}

// TestTheHarnessPromptRendersItsCurrentBytes pins harness prompt version
// 1 in each of its sixteen combinations of sections, by the SHA-256 of
// the text it rendered before it moved into this package.
func TestTheHarnessPromptRendersItsCurrentBytes(t *testing.T) {
	want := []string{
		"8ab4952a767c587c306989ef1afe50eac905e018e676abbaf724ca1b142af5d8",
		"e9c3db42f2452256369c4fb1b80c7c6cf68be6db3911646f181cb7fba9ac6630",
		"2cdb35ec8737a776eeccdbebbda8c4275070fa0e3442522ae477462e979f1a2a",
		"5aa3d41eebebf13b4e4a73af9f6902d73993b3e9f689c84a709b6e46e2d2e5ee",
		"9a846f222c2b02e08b73430c0acbc49da00221040fd8775c815c40c186fee589",
		"30c374b65d0aa63339bfde4feb9f83e716bcdc97c5801e1da0301beab33029d4",
		"5f477d3af59d1b01991af19eb8cf3cc23e790bd97999b3712c11f433d6eb263a",
		"60b7a24353e39a8ea1e079defd5af931cb70632dce2eb43fb1847f96751880dc",
		"fc24a9f32595c6266e1b5884f79775f95da3b1329c25e8aed108afb5ac40d3fe",
		"b32ad4a920b1fd5d44c8e14777205b61d7f13cf47da39ebfc0296bc7048ec006",
		"8a5f71047c613bdd77583d38b97997ed23e12d67384a222a6c937dc951d2a90b",
		"46e5240d29589595bcd2fa05d4ea4a309ab5a72019019025376b9400758d0198",
		"43a703d5e498f57a58f1f868545a12ac5533d7209821f3678697e80ea585731a",
		"3996753f8e76c8d33aa1d021e64dbc2e5b7ade9bf0a4a6e3959459c53edfdf19",
		"36b5f849ff200c0a82d57e6833bfc63e845561fc333d752e223e6451b4670ec6",
		"8b50d2daa0751c36a8debd0ba402629893fca2ad3395e91b664478a4c310c59a",
	}
	for i, sum := range want {
		o := HarnessOptions{Host: i&1 != 0, Threads: i&2 != 0, Memory: i&4 != 0, Git: i&8 != 0}
		got, err := Harness(1, o)
		if err != nil {
			t.Fatal(err)
		}
		if s := sha256.Sum256([]byte(got)); hex.EncodeToString(s[:]) != sum {
			t.Errorf("harness prompt 1 with %+v changed:\n%s", o, got)
		}
	}
	if uses := slices.Sorted(maps.Keys(fields(t, texts, "harness/harness-v1"))); !slices.Equal(uses, []string{"Git", "Host", "Memory", "Threads"}) {
		t.Errorf("harness prompt 1 uses %v", uses)
	}
}

// TestNoTextHasAnEmDash keeps the texts to the house style.
func TestNoTextHasAnEmDash(t *testing.T) {
	for n, raw := range raws(t, files) {
		if strings.ContainsRune(raw, '\u2014') {
			t.Errorf("%s holds an em dash", n)
		}
	}
}

// TestAMissingKeyFailsLoudly renders texts with the wrong inputs: a key a
// template uses and the data lacks, a name no file holds, and a template
// asked for as a static text.
func TestAMissingKeyFailsLoudly(t *testing.T) {
	if _, err := Execute(FileChanged, Data{}); err == nil || !strings.Contains(err.Error(), `"Path"`) {
		t.Fatalf("a missing key rendered: %v", err)
	}
	if _, err := Execute(FileChanged, nil); err == nil {
		t.Fatal("a template rendered with no data")
	}
	if _, err := Execute("results/nothing-v1", nil); err == nil {
		t.Fatal("a name no file holds rendered")
	}
	if got := Render(FileChanged, Data{"Path": "a"}); got != "a changed since it was last read; read it again before writing." {
		t.Fatalf("Render %q", got)
	}
	for name, f := range map[string]func(){
		"render a missing key":        func() { Render(FileChanged, Data{}) },
		"a template as a static text": func() { Text(FileChanged) },
		"an unknown static text":      func() { Text("results/nothing-v1") },
	} {
		if !panics(f) {
			t.Errorf("%s: no panic", name)
		}
	}
	if _, err := Harness(0, HarnessOptions{}); err == nil {
		t.Fatal("harness prompt version 0 rendered")
	}
}

// TestRenderingIsConcurrentAndRepeatable renders every case from several
// goroutines at once: the same inputs give the same bytes, and the shared
// templates hold under the race detector.
func TestRenderingIsConcurrentAndRepeatable(t *testing.T) {
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			for _, c := range textCases {
				a, err := Execute(c.name, c.data)
				if err != nil {
					errs <- err
					return
				}
				b, err := Execute(c.name, c.data)
				if err != nil {
					errs <- err
					return
				}
				if a != b {
					errs <- fmt.Errorf("%s rendered %q, then %q", c.name, a, b)
					return
				}
			}
			errs <- nil
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}
