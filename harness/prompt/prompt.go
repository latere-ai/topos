// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package prompt holds the harness prompts of spec 011. Every released
// version stays embedded, so a replay rebuilds an old request from the
// prompt_version its model.request recorded.
package prompt

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed harness-v*.md compact-v*.md
var files embed.FS

// Current is the version new requests use.
const Current = 1

// Version is the prompt_version of a harness prompt version.
func Version(n int) string { return fmt.Sprintf("harness/%d", n) }

// Options are the facts the prompt's conditional sections depend on.
type Options struct {
	// Host is true on the person's own computer, false in a disposable
	// sandbox.
	Host bool
	// Threads is true when the agent has subagents.
	Threads bool
	// Memory is true when a memory store is attached.
	Memory bool
	// Git is true when the working directory is in a repository.
	Git bool
}

const (
	hostText = "This is the person's own computer. Files you delete or overwrite outside version control are gone, and commands run with the person's own account. Prefer reversible changes, and ask before a command that deletes data, rewrites history, or reaches another system."

	sandboxText = "This is a disposable sandbox made for this session. Its files are yours to change, and it holds no credential; what leaves it goes through the session's network rules."

	threadsText = "\n\n# Threads\n\n`spawn` starts a thread that runs another agent on a task you give it, on this same machine, and returns its final answer. `message` sends a message to a running thread. `advisor` asks a stronger model to review your work so far. Give a thread a complete task: it does not see this conversation."

	memoryText = "\n\n# Memory\n\nMemory is files in the directories the memory notes name. Read them with the file tools at the start of a task, and write what later sessions should know as you learn it."

	gitText = "\n\n# Git\n\nCommit your work on the session's branch with a message that says what changed and why. Never push to a protected branch, and never rewrite a commit you did not make in this session."
)

// Render returns the text of harness prompt version n.
func Render(n int, o Options) (string, error) {
	b, err := files.ReadFile(fmt.Sprintf("harness-v%d.md", n))
	if err != nil {
		return "", fmt.Errorf("prompt: no harness prompt version %d", n)
	}
	machine := sandboxText
	if o.Host {
		machine = hostText
	}
	cond := func(on bool, text string) string {
		if on {
			return text
		}
		return ""
	}
	r := strings.NewReplacer(
		"{{machine}}", machine,
		"{{threads}}", cond(o.Threads, threadsText),
		"{{memory}}", cond(o.Memory, memoryText),
		"{{git}}", cond(o.Git, gitText),
	)
	return strings.TrimSpace(r.Replace(string(b))), nil
}

// CompactCurrent is the compaction prompt version new compactions use.
const CompactCurrent = 1

// Compact returns the text of compaction prompt version n (spec 010).
func Compact(n int) (string, error) {
	b, err := files.ReadFile(fmt.Sprintf("compact-v%d.md", n))
	if err != nil {
		return "", fmt.Errorf("prompt: no compaction prompt version %d", n)
	}
	return strings.TrimSpace(string(b)), nil
}
