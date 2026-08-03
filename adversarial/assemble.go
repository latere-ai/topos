package adversarial

import (
	"fmt"
	"strings"

	"latere.ai/x/topos/adversarial/internal/agent"
)

// assemblePrompt is the internal implementation of AssemblePrompt.
// Mirrors agent.AssemblePrompt from internal/agent but works on the
// public CriticInput type so external Critic implementations can call
// it without importing any internal/ package.
func assemblePrompt(in CriticInput) string {
	var b strings.Builder
	b.WriteString(in.SystemPrompt)
	b.WriteString("\n\n")
	b.WriteString(agent.Directives)
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
