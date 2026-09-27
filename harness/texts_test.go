// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"fmt"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// TestTheAdvisorReadsTheTranscriptAsText pins the transcript an advisor
// reads: who said what, each call with its input, each result, and
// nothing for a block with no content.
func TestTheAdvisorReadsTheTranscriptAsText(t *testing.T) {
	tr := session.Transcript{Messages: []lux.Message{
		{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockText, Text: "Fix the parser."}}},
		{Role: ir.RoleAssistant, Blocks: []lux.Block{
			{Type: ir.BlockThinking, Text: "unseen"},
			{Type: ir.BlockText, Text: "Reading it."},
			{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_1", Name: "read", Args: []byte(`{"path":"parse.go"}`)}},
			{Type: ir.BlockToolUse},
		}},
		{Role: ir.RoleUser, Blocks: []lux.Block{
			{Type: ir.BlockToolResult, ToolResult: &lux.ToolResult{ToolUseID: "toolu_1", Blocks: []lux.Block{{Type: ir.BlockText, Text: "package parse"}, {Type: ir.BlockText, Text: "func P() {}"}}}},
			{Type: ir.BlockToolResult},
		}},
	}}
	want := "The conversation so far:\n" +
		"\nPerson: Fix the parser.\n" +
		"\nAgent: Reading it.\n" +
		"\nAgent called read with {\"path\":\"parse.go\"}\n" +
		"\nThe call returned:\npackage parse\nfunc P() {}\n"
	if got := renderTranscript(tr); got != want {
		t.Fatalf("the advisor's transcript:\n%q\nwant\n%q", got, want)
	}
}

// TestSpawnRefusesAnUnknownSubagentAndTheDepthLimit pins the error results
// a spawn gives the model before it starts a thread.
func TestSpawnRefusesAnUnknownSubagentAndTheDepthLimit(t *testing.T) {
	top := &turn{h: &Harness{c: Config{Subagents: map[string]Subagent{"reviewer": {}}}}, sh: &shared{}}
	res, err := top.spawn(t.Context(), "toolu_1", "nobody", "Review.", "", nil)
	if want := CodeUnknownSubagent + ": " + fmt.Sprintf("no subagent named %q", "nobody"); err != nil || res.Outcome != tools.OutcomeError || res.Content[0].Text != want {
		t.Fatalf("an unknown subagent: %+v, %v; want %q", res, err, want)
	}
	deep := &turn{h: top.h, sh: top.sh, depth: DefaultMaxDepth}
	res, err = deep.spawn(t.Context(), "toolu_2", "reviewer", "Review.", "", nil)
	if want := CodeDepthExceeded + ": " + fmt.Sprintf("a thread at depth %d may not spawn", DefaultMaxDepth); err != nil || res.Outcome != tools.OutcomeError || res.Content[0].Text != want {
		t.Fatalf("past the depth limit: %+v, %v; want %q", res, err, want)
	}
}
