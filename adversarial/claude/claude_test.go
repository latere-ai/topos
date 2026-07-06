package claude

import (
	"strings"
	"testing"

	"latere.ai/x/topos/adversarial/internal/agent"
)

// TestWithProposerReadOnly locks the read-only denylist: the proposer must be
// barred from every file-mutating tool (and Bash, which can write).
func TestWithProposerReadOnly(t *testing.T) {
	var p agent.ClaudeProposer
	WithProposerReadOnly()(&p)

	got := strings.Join(p.DisallowedTools, ",")
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash"} {
		if !strings.Contains(got, tool) {
			t.Errorf("read-only denylist %q missing %q", got, tool)
		}
	}
}
