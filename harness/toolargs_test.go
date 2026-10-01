// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// chatCall is one tool_calls entry of a Chat Completions stream chunk:
// the header when id is set, else an arguments fragment.
type chatCall struct {
	index          int
	id, name, args string
}

// chatChunk is one Chat Completions SSE frame carrying tool call deltas.
func chatChunk(t *testing.T, calls ...chatCall) string {
	t.Helper()
	var tcs []map[string]any
	for _, c := range calls {
		tc := map[string]any{"index": c.index, "function": map[string]any{"arguments": c.args}}
		if c.id != "" {
			tc["id"], tc["type"] = c.id, "function"
			tc["function"].(map[string]any)["name"] = c.name
		}
		tcs = append(tcs, tc)
	}
	return chatFrame(t, map[string]any{"role": "assistant", "content": nil, "tool_calls": tcs}, nil)
}

// chatFrame is one Chat Completions SSE frame with a delta and a
// finish_reason, nil for none.
func chatFrame(t *testing.T, delta map[string]any, finish any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(b) + "\n\n"
}

// chatEnd is the tail of a Chat Completions stream: the finish reason,
// the usage and [DONE].
func chatEnd(t *testing.T, finish string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": model, "choices": []any{},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110},
	})
	if err != nil {
		t.Fatal(err)
	}
	return chatFrame(t, map[string]any{"content": ""}, finish) + "data: " + string(b) + "\n\n" + "data: [DONE]\n\n"
}

// chatSetup is the harness over Lux's OpenAI door speaking Chat
// Completions, with need, a tool whose command is required, beside the
// usual echo and bash.
func chatSetup(t *testing.T) (*env, *fakeTool) {
	t.Helper()
	need := &fakeTool{name: "need", props: tools.Properties{Effect: tools.EffectRead},
		schema: `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`}
	e := setup(t, func(c *Config) {
		c.Connection = models.Connection{BaseURL: strings.TrimSuffix(c.Connection.BaseURL, "/anthropic") + "/openai", Model: model, Family: models.FamilyOther, Dialect: ir.DialectOpenAIChat}
		if err := c.Tools.AddBuiltin(need); err != nil {
			t.Fatal(err)
		}
	})
	return e, need
}

// answeredWithAnError checks that the step's response is in the log, its
// broken call held with the input {}, the raw stream kept, and the call
// answered with an invalid_input result naming want, without an
// agent.tool_use or a session.error, and that the turn ended end_turn.
func answeredWithAnError(t *testing.T, e *env, out Outcome, id, want string) {
	t.Helper()
	ctx := t.Context()
	if errs := e.events(ctx, session.TypeSessionError); len(errs) != 0 {
		t.Fatalf("a session.error: %s", errs[0].Payload)
	}
	if out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	msgs := e.events(ctx, session.TypeAgentMessage)
	if len(msgs) == 0 {
		t.Fatal("no agent.message")
	}
	var first session.AgentMessage
	if err := msgs[0].Decode(&first); err != nil {
		t.Fatal(err)
	}
	var logged bool
	for _, b := range first.Message.Blocks {
		if b.ToolUse != nil && b.ToolUse.ID == id {
			logged = string(b.ToolUse.Args) == `{}`
		}
	}
	if !logged {
		t.Fatalf("the call %s is not in the message with the input {}: %+v", id, first.Message.Blocks)
	}
	var mr session.ModelRequest
	if err := e.events(ctx, session.TypeModelRequest)[0].Decode(&mr); err != nil {
		t.Fatal(err)
	}
	if mr.Outcome != "ok" || mr.ResponseBlob == "" {
		t.Fatalf("model.request %+v", mr)
	}
	for _, ev := range e.events(ctx, session.TypeAgentToolUse) {
		var u session.AgentToolUse
		if err := ev.Decode(&u); err != nil {
			t.Fatal(err)
		}
		if u.ToolUseID == id {
			t.Fatalf("the broken call %s has an agent.tool_use", id)
		}
	}
	var res *session.ToolResult
	for _, ev := range e.events(ctx, session.TypeToolResult) {
		var r session.ToolResult
		if err := ev.Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.ToolUseID == id {
			res = &r
		}
	}
	if res == nil || !res.IsError || res.Outcome != tools.OutcomeInvalidInput || len(res.Content) != 1 || !strings.Contains(res.Content[0].Text, want) {
		t.Fatalf("result %+v, want invalid_input naming %q", res, want)
	}
}

// sawTheError is the next step's Expect: the result of call id came
// back to the model holding want. Chat Completions has no error flag on
// a tool message, so the text alone says it failed.
func sawTheError(id, want string) func(*ir.Request) error {
	return func(r *ir.Request) error {
		for _, m := range r.Messages {
			for _, b := range m.Blocks {
				if b.ToolResult == nil || b.ToolResult.ToolUseID != id {
					continue
				}
				if len(b.ToolResult.Blocks) == 0 || !strings.Contains(b.ToolResult.Blocks[0].Text, want) {
					return fmt.Errorf("the result of %s is %+v", id, b.ToolResult)
				}
				return nil
			}
		}
		return fmt.Errorf("no result for %s in the next request", id)
	}
}

// TestBrokenArgumentsAreAnsweredAndTheModelRetries is the turn the bug
// ended: a model whose call's arguments break off inside a string. The
// step is logged, the call is answered with the text it sent, and the
// model's next call, with valid arguments, runs.
func TestBrokenArgumentsAreAnsweredAndTheModelRetries(t *testing.T) {
	e, _ := chatSetup(t)
	ctx := t.Context()
	const broken = `{"text":"rm hello.cc hello.ccc'} }]}]} }]}>}?  the files have been removed`
	want := "The arguments of echo were not valid JSON (the text ends before the JSON value does). They began:\n" + broken
	e.stub.Script(model,
		luxstub.Reply{Raw: chatChunk(t, chatCall{index: 0, id: "call_broken", name: "echo"}) +
			chatChunk(t, chatCall{index: 0, args: broken[:30]}) + chatChunk(t, chatCall{index: 0, args: broken[30:]}) + chatEnd(t, "tool_calls")},
		luxstub.Reply{Expect: sawTheError("call_broken", want), Raw: chatChunk(t, chatCall{index: 0, id: "call_fixed", name: "echo", args: `{"text":"rm hello.cc"}`}) + chatEnd(t, "tool_calls")},
		luxstub.Reply{Raw: chatFrame(t, map[string]any{"role": "assistant", "content": "Removed."}, nil) + chatEnd(t, "stop")},
	)
	e.send(ctx, "can you remove all the files except readme and hello.c?")
	out := e.turn(ctx)
	answeredWithAnError(t, e, out, "call_broken", want)
	if got := e.echo.ran(); len(got) != 1 || got[0] != "call_fixed" {
		t.Fatalf("echo ran %v, want only the fixed call", got)
	}
	if n := len(e.events(ctx, session.TypeModelRequest)); n != 3 {
		t.Fatalf("%d model requests, want 3", n)
	}
}

// TestEmptyArgumentsAreTheEmptyObject: a call that streams no arguments
// is the input {}, which the tool's schema then judges; a tool that
// requires a property answers it with what is missing.
func TestEmptyArgumentsAreTheEmptyObject(t *testing.T) {
	e, need := chatSetup(t)
	ctx := t.Context()
	want := `/: missing required property "command"`
	e.stub.Script(model,
		luxstub.Reply{Raw: chatChunk(t, chatCall{index: 0, id: "call_empty", name: "need"}) + chatChunk(t, chatCall{index: 0, args: ""}) + chatEnd(t, "tool_calls")},
		luxstub.Reply{Expect: sawTheError("call_empty", want), Raw: chatChunk(t, chatCall{index: 0, id: "call_full", name: "need", args: `{"command":"ls"}`}) + chatEnd(t, "tool_calls")},
		luxstub.Reply{Raw: chatFrame(t, map[string]any{"role": "assistant", "content": "Listed."}, nil) + chatEnd(t, "stop")},
	)
	e.send(ctx, "List the files.")
	answeredWithAnError(t, e, e.turn(ctx), "call_empty", want)
	if got := need.ran(); len(got) != 1 || got[0] != "call_full" {
		t.Fatalf("need ran %v, want only the full call", got)
	}
}

// TestInterleavedParallelCallsAreReassembled: parallel calls whose
// headers come first and whose arguments interleave reach the tools
// with their whole arguments, whichever way the decoder closes their
// blocks.
func TestInterleavedParallelCallsAreReassembled(t *testing.T) {
	e, _ := chatSetup(t)
	ctx := t.Context()
	e.stub.Script(model,
		luxstub.Reply{Raw: chatChunk(t, chatCall{index: 0, id: "call_a", name: "echo"}) +
			chatChunk(t, chatCall{index: 1, id: "call_b", name: "echo"}) +
			chatChunk(t, chatCall{index: 0, args: `{"text":`}) +
			chatChunk(t, chatCall{index: 1, args: `{"text":`}) +
			chatChunk(t, chatCall{index: 0, args: `"a"}`}, chatCall{index: 1, args: `"b"}`}) +
			chatEnd(t, "tool_calls")},
		luxstub.Reply{Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			var got []string
			for _, b := range last.Blocks {
				if b.ToolResult != nil && !b.ToolResult.IsError {
					got = append(got, b.ToolResult.ToolUseID+"="+b.ToolResult.Blocks[0].Text)
				}
			}
			if fmt.Sprint(got) != "[call_a=echo a call_b=echo b]" {
				return fmt.Errorf("results %v", got)
			}
			return nil
		}, Raw: chatFrame(t, map[string]any{"role": "assistant", "content": "Both echoed."}, nil) + chatEnd(t, "stop")},
	)
	e.send(ctx, "Echo a and b.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if errs := e.events(ctx, session.TypeSessionError); len(errs) != 0 {
		t.Fatalf("a session.error: %s", errs[0].Payload)
	}
	var uses []string
	for _, ev := range e.events(ctx, session.TypeAgentToolUse) {
		var u session.AgentToolUse
		if err := ev.Decode(&u); err != nil {
			t.Fatal(err)
		}
		uses = append(uses, u.ToolUseID+"="+string(u.Input))
	}
	if fmt.Sprint(uses) != `[call_a={"text":"a"} call_b={"text":"b"}]` {
		t.Fatalf("tool uses %v", uses)
	}
}

// TestACallNeverStoppedIsClosedAtTheMessagesEnd: a stream that ends its
// message without stopping a call's block still delivers the call, with
// the arguments it streamed.
func TestACallNeverStoppedIsClosedAtTheMessagesEnd(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	frame := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("event: %s\ndata: %s\n\n", v["type"], b)
	}
	e.stub.Script(model,
		luxstub.Reply{Raw: frame(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "model": model, "usage": map[string]any{"input_tokens": 100}}}) +
			frame(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_open", "name": "echo", "input": map[string]any{}}}) +
			frame(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"text":`}}) +
			frame(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `"open"}`}}) +
			frame(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 10}}) +
			frame(map[string]any{"type": "message_stop"})},
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.send(ctx, "Echo open.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if errs := e.events(ctx, session.TypeSessionError); len(errs) != 0 {
		t.Fatalf("a session.error: %s", errs[0].Payload)
	}
	uses := e.events(ctx, session.TypeAgentToolUse)
	var u session.AgentToolUse
	if len(uses) != 1 || uses[0].Decode(&u) != nil || string(u.Input) != `{"text":"open"}` {
		t.Fatalf("tool uses %d, %+v", len(uses), u)
	}
	if got := e.echo.ran(); len(got) != 1 {
		t.Fatalf("echo ran %v", got)
	}
}

// TestTheCapturedRunawayNeverFailsTheTurn replays the stream a provider
// sent for the bug: a model that ran to its output limit inside a
// string argument, reported as finish_reason tool_calls with the native
// reason max_output_tokens. Whether the codec reads that as a call to
// answer or as a stop at the output limit to send again, the turn goes
// on to the model's next call and never fails.
func TestTheCapturedRunawayNeverFailsTheTurn(t *testing.T) {
	raw, err := os.ReadFile("testdata/runaway-arguments.sse")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := chatSetup(t)
	ctx := t.Context()
	e.stub.Script(model,
		luxstub.Reply{Raw: string(raw)},
		luxstub.Reply{Raw: chatChunk(t, chatCall{index: 0, id: "call_fixed", name: "echo", args: `{"text":"rm hello.cc hello.ccc hello.cccc"}`}) + chatEnd(t, "tool_calls")},
		luxstub.Reply{Raw: chatFrame(t, map[string]any{"role": "assistant", "content": "Removed."}, nil) + chatEnd(t, "stop")},
	)
	e.send(ctx, "can you remove all the files except readme and hello.c?")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if errs := e.events(ctx, session.TypeSessionError); len(errs) != 0 {
		t.Fatalf("a session.error: %s", errs[0].Payload)
	}
	if got := e.write.ran(); len(got) != 0 {
		t.Fatalf("the runaway bash call ran: %v", got)
	}
	if got := e.echo.ran(); len(got) != 1 || got[0] != "call_fixed" {
		t.Fatalf("echo ran %v, want the next call", got)
	}
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var mr session.ModelRequest
		if err := ev.Decode(&mr); err != nil {
			t.Fatal(err)
		}
		if mr.ResponseBlob == "" {
			t.Fatalf("model.request %d keeps no response", ev.Seq)
		}
	}
}

// TestAFailedStreamKeepsWhatItReceived: a stream that fails part way,
// here on a chunk that is not JSON, ends the turn with model_error, and
// its model.request keeps the bytes received as its response blob.
func TestAFailedStreamKeepsWhatItReceived(t *testing.T) {
	e, _ := chatSetup(t)
	ctx := t.Context()
	raw := chatChunk(t, chatCall{index: 0, id: "call_1", name: "echo", args: `{"text":"a"}`}) + "data: {not json\n\n"
	e.stub.Script(model, luxstub.Reply{Raw: raw})
	e.send(ctx, "Echo a.")
	if out := e.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeModelError {
		t.Fatalf("outcome %+v", out)
	}
	var mr session.ModelRequest
	if err := e.events(ctx, session.TypeModelRequest)[0].Decode(&mr); err != nil {
		t.Fatal(err)
	}
	if mr.Outcome != "error" || mr.ResponseBlob == "" {
		t.Fatalf("model.request %+v", mr)
	}
	rc, err := e.store.Blob(ctx, e.s.ID, mr.ResponseBlob)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil || string(got) != raw {
		t.Fatalf("response blob %q, %v", got, err)
	}
}
