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

// HarnessDevice is the harness prompt version a session on a person's own
// computer uses (spec 080): the first with a machine section for one.
// The instruction tier ran it against version 1 on 2026-10-19, at commit
// 7dc5386 on anthropic/claude-haiku-5.5, 15 runs of each of the 13
// instruction tasks per version: version 1 passed 179 of 195 runs and
// version 2, rendered for a device, 190 of 195, for 0.52 USD in all (spec
// 080's Outcome). Every other session stays on HarnessCurrent; version 2
// renders byte for byte as version 1 on any machine but a device.
const HarnessDevice = 2

// HarnessVersion is the prompt_version a model.request records for
// harness prompt version n.
func HarnessVersion(n int) string { return fmt.Sprintf("harness/%d", n) }

// HarnessOptions are the facts the harness prompt's conditional sections
// depend on.
type HarnessOptions struct {
	// Host is true on the person's own computer, false in a disposable
	// sandbox.
	Host bool
	// Device is true on a person's own computer reached through the Cella
	// environment it serves (spec 080), which version 2 and later say.
	Device bool
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
		"Host": o.Host, "Device": o.Device, "Threads": o.Threads, "Memory": o.Memory, "Git": o.Git,
	})
	if err != nil {
		return "", fmt.Errorf("prompts: no harness prompt version %d: %w", n, err)
	}
	return strings.TrimSpace(s), nil
}
