package adversarial

import (
	"fmt"
	"strings"
)

// assemblePrompt is the internal implementation of AssemblePrompt.
// Mirrors agent.AssemblePrompt from internal/agent but works on the
// public CriticInput type so external Critic implementations can call
// it without importing any internal/ package.
func assemblePrompt(in CriticInput) string {
	var b strings.Builder
	b.WriteString(in.SystemPrompt)
	b.WriteString("\n\n")
	b.WriteString(criticDirectives)
	b.WriteString("\n\n# Task\n\n")
	b.WriteString(in.TaskContext)
	b.WriteString("\n\n# Diff\n\n```diff\n")
	b.WriteString(in.DiffPatch)
	b.WriteString("\n```\n")
	if len(in.PriorRoundFiles) > 0 {
		b.WriteString("\n# Prior rounds\n\n")
		for _, r := range in.PriorRoundFiles {
			fmt.Fprintf(&b, "- @%s - round %d %s\n", r.Path, r.Round, r.Role)
		}
	}
	return b.String()
}

// criticDirectives is the immutable output-discipline block appended to
// every critic system prompt. Kept in sync with internal/agent.directives.
const criticDirectives = `Critical output rules:
1. Your entire reply MUST be the markdown attack document and nothing
   else. No preamble like "I'll review this" or "Let me start by". No
   trailing summary. Just the document.
2. Do NOT run shell commands, search the file tree, or otherwise
   investigate beyond the diff, task context, and prior round files
   provided above. Reading the files listed under "# Prior rounds" is
   expected (round 3+ requires it); reading anything else is not.
   The reproduction in each attack is what proves the bug; you do not
   need to verify it with a tool.
3. If you decide there is nothing to attack, emit an empty document
   that still has the top header and "aspect:" line, then stop.
4. The very first non-blank line of your reply MUST be the top header
   "# Critic <i> - round <n> attacks".`
