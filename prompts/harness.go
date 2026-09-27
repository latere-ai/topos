// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

import (
	"fmt"
	"strings"
)

// HarnessCurrent is the harness prompt version new requests use (spec
// 011).
const HarnessCurrent = 1

// HarnessVersion is the prompt_version a model.request records for
// harness prompt version n.
func HarnessVersion(n int) string { return fmt.Sprintf("harness/%d", n) }

// HarnessOptions are the facts the harness prompt's conditional sections
// depend on.
type HarnessOptions struct {
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

// Harness returns the text of harness prompt version n, the file
// harness/harness-v<n> with the sections that apply. Each version's file
// names the section versions it includes, so a released version renders
// the same text however later sections change. The version comes from
// configuration, so an unknown one is an error, not a panic.
func Harness(n int, o HarnessOptions) (string, error) {
	s, err := Execute(Name(fmt.Sprintf("harness/harness-v%d", n)), Data{
		"Host": o.Host, "Threads": o.Threads, "Memory": o.Memory, "Git": o.Git,
	})
	if err != nil {
		return "", fmt.Errorf("prompts: no harness prompt version %d: %w", n, err)
	}
	return strings.TrimSpace(s), nil
}
