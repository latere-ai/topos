// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
)

// TestToolTextStatesTheLimitsItEnforces: a tool's description is
// versioned text sent to the model byte for byte, so it keeps its numbers
// written out; this holds each number to the constant the tool enforces,
// and a changed limit fails here until a new version of the text says it.
func TestToolTextStatesTheLimitsItEnforces(t *testing.T) {
	itoa := strconv.Itoa
	for _, c := range []struct {
		name   prompts.Name
		limits []string
	}{
		{prompts.ToolBash, []string{itoa(BashDefaultTimeoutMS), itoa(BashMaxTimeoutMS), itoa(int(BashServerGrace.Seconds())) + " seconds"}},
		{prompts.ToolTodo, []string{itoa(TodoLimit) + " items"}},
		{prompts.ToolRead, []string{itoa(ReadDefaultLimit), itoa(ReadMaxLineChars) + " characters", itoa(ReadMaxImage>>20) + " MiB"}},
		{prompts.ToolGlob, []string{itoa(machine.GlobLimit)}},
		{prompts.ToolGrep, []string{itoa(machine.DefaultHeadLimit)}},
		{prompts.ToolWebFetch, []string{itoa(machine.FetchRedirects), itoa(machine.FetchMaxBody>>20) + " MiB", itoa(int(machine.FetchTimeout.Seconds())) + " seconds"}},
	} {
		text := prompts.Text(c.name)
		for _, want := range c.limits {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not state %q, the limit its tool enforces", c.name, want)
			}
		}
	}
	for schema, n := range map[string]int{bashSchema: BashMaxTimeoutMS, todoSchema: TodoLimit, readSchema: ReadDefaultLimit} {
		if !strings.Contains(schema, strconv.Itoa(n)) {
			t.Errorf("a schema does not state %d", n)
		}
	}
}
