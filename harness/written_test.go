// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
)

// chatTools are the tools a chat agent is offered: the built-ins, todo and
// the question tool, and no tool named plan.
var chatTools = []string{"read", "grep", "glob", "edit", "write", "bash", "web_fetch", "todo", "question"}

// questionAsMarkup is an answer that wrote its plan and its question to the
// person as markup instead of calling the question tool, in the shape an
// open model served without a tool call wrote it: a plan element, then a
// question element that nests the questions, each with a header, the
// question and options whose attributes are the tool's input fields.
const questionAsMarkup = "Building a to-do app is a great project. Here is how I would start.\n\n" +
	"<plan> 1. **Define the Tech Stack**: pick the platform and the storage. 2. **Model the data**: projects, areas and tasks. </plan>\n\n" +
	"<question> <questions> <question> <header>Tech Stack</header> " +
	"<question>What technologies would you like to use?</question> <options> " +
	`<option label="Web (React + Tailwind)" description="Recommended. Runs anywhere." recommended="true"></option> ` +
	`<option label="Native (Swift)" description="Feels at home on one platform."></option> ` +
	"</options> </question> </questions> </question>\n"

func answered(stop ir.StopReason, blocks ...lux.Block) models.Result {
	return models.Result{Message: lux.Message{Role: ir.RoleAssistant, Blocks: blocks}, StopReason: stop}
}

func said(s string) lux.Block { return lux.Block{Type: ir.BlockText, Text: s} }

// TestWrittenCallReadsMarkupNamedAfterAnOfferedTool holds the detection
// of spec 062 to its two shapes, outside code, on a response that ended on
// its own and called no tool, and to nothing else.
func TestWrittenCallReadsMarkupNamedAfterAnOfferedTool(t *testing.T) {
	withPlan := append([]string{"plan"}, chatTools...)
	for _, c := range []struct {
		name    string
		res     models.Result
		offered []string
		want    string
	}{
		{"the question written as nested markup", answered(ir.StopEndTurn, said(questionAsMarkup)), chatTools, "question"},
		{"the same answer ended by a stop sequence", answered(ir.StopStopSequence, said(questionAsMarkup)), chatTools, "question"},
		{"a plan element where no tool is named plan", answered(ir.StopEndTurn, said("<plan>1. Model the data.</plan>")), chatTools, ""},
		{"a plan element where a tool is named plan", answered(ir.StopEndTurn, said("<plan>1. Model the data.</plan>")), withPlan, "plan"},
		{"the first call written is named", answered(ir.StopEndTurn, said("Step one.\n<todo><item>a</item></todo>\n<question>Which?</question>")), chatTools, "todo"},
		{"a self-closing element", answered(ir.StopEndTurn, said(`Saving the list. <todo items="a, b"/>`)), chatTools, "todo"},
		{"a self-closing element with a space", answered(ir.StopEndTurn, said(`Saving the list. <todo />`)), chatTools, "todo"},
		{"attributes on the element", answered(ir.StopEndTurn, said(`<bash command="ls -la"></bash>`)), chatTools, "bash"},
		{"a closing tag with space", answered(ir.StopEndTurn, said("<read>main.go</read >")), chatTools, "read"},
		{"the markup split over two text blocks", answered(ir.StopEndTurn, said("<question>Which stack?"), said("</question>")), chatTools, "question"},
		{"a Hermes tool_call", answered(ir.StopEndTurn, said("Let me ask.\n<tool_call>\n{\"name\": \"question\", \"arguments\": {\"questions\": []}}\n</tool_call>")), chatTools, "question"},
		{"a tool_call with no closing tag", answered(ir.StopEndTurn, said(`<tool_call>{"name":"question","arguments":{}}`)), chatTools, "question"},
		{"an invoke naming the tool", answered(ir.StopEndTurn, said(`<function_calls><invoke name="question"><parameter name="questions">[]</parameter></invoke></function_calls>`)), chatTools, "question"},
		{"Llama's function form", answered(ir.StopEndTurn, said(`<function=web_fetch>{"url": "https://example.com"}</function>`)), chatTools, "web_fetch"},
		{"a function_call in JSON", answered(ir.StopEndTurn, said(`<function_call>{"type":"function","function":{"name":"grep","arguments":"{}"}}</function_call>`)), chatTools, "grep"},
		{"a tool_call naming a tool not offered", answered(ir.StopEndTurn, said(`<tool_call>{"name": "plan", "arguments": {}}</tool_call>`)), chatTools, ""},
		{"a wrapper naming nothing", answered(ir.StopEndTurn, said("<tool_call>\nnothing here\n</tool_call>")), chatTools, ""},

		{"prose that names tools", answered(ir.StopEndTurn, said("I can ask you a question with the question tool, read the files, or run bash to build it. Shall I write a todo list first?")), chatTools, ""},
		{"an element opened and never closed", answered(ir.StopEndTurn, said("I will use <question> to ask you when we start.")), chatTools, ""},
		{"a tag that only shares a prefix", answered(ir.StopEndTurn, said("<questions>Which?</questions> <reader>x</reader>")), chatTools, ""},
		{"markup in a fenced code block", answered(ir.StopEndTurn, said("The tool's input looks like this:\n\n```xml\n<question>\n  <header>Stack</header>\n</question>\n```\n\nThat is all.")), chatTools, ""},
		{"markup in a tilde fence", answered(ir.StopEndTurn, said("~~~\n<question>Which?</question>\n~~~\n")), chatTools, ""},
		{"markup in a fence nested in a list", answered(ir.StopEndTurn, said("1. Write the element:\n\n    ```html\n    <todo>a</todo>\n    ```\n2. Done.")), chatTools, ""},
		{"markup in a fence never closed", answered(ir.StopEndTurn, said("```\n<question>Which?</question>\n")), chatTools, ""},
		{"a closing fence shorter than its opening", answered(ir.StopEndTurn, said("````\n```\n<question>Which?</question>\n````\n")), chatTools, ""},
		{"markup in an inline code span", answered(ir.StopEndTurn, said("Write `<question>Which?</question>` to ask, or ``<todo/>``.")), chatTools, ""},
		{"markup after a fence closes", answered(ir.StopEndTurn, said("```\ncode\n```\n<question>Which?</question>")), chatTools, "question"},
		{"markup after a code span closes", answered(ir.StopEndTurn, said("Run `ls` first.\n<bash>ls</bash>")), chatTools, "bash"},
		{"a backtick that nothing closes", answered(ir.StopEndTurn, said("A stray ` here.\n<question>Which?</question>")), chatTools, "question"},
		{"a code span closed only past a blank line", answered(ir.StopEndTurn, said("A stray ` here.\n\n<question>Which?</question>\n\nand ` there.")), chatTools, "question"},
		{"a line of three backticks with more on it", answered(ir.StopEndTurn, said("```x``` then <question>Which?</question>")), chatTools, "question"},

		{"a response that calls a tool", answered(ir.StopToolUse, said(questionAsMarkup), lux.Block{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_1", Name: "read", Args: []byte(`{}`)}}), chatTools, ""},
		{"a response cut at the output limit", answered(ir.StopMaxTokens, said(questionAsMarkup)), chatTools, ""},
		{"a refusal", answered(ir.StopRefusal, said(questionAsMarkup)), chatTools, ""},
		{"markup in thinking alone", answered(ir.StopEndTurn, lux.Block{Type: ir.BlockThinking, Text: "<question>Which?</question>"}, said("Here is my answer.")), chatTools, ""},
		{"a request that offered no tools", answered(ir.StopEndTurn, said(questionAsMarkup)), nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := writtenCall(c.res, c.offered); got != c.want {
				t.Fatalf("writtenCall = %q, want %q", got, c.want)
			}
		})
	}
}
