// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// systemTexts are the system blocks of every request the stub received,
// and whether each request's last system block carries the cache hint.
func (e *env) systemTexts() ([][]string, []bool) {
	e.t.Helper()
	var texts [][]string
	var cached []bool
	for _, r := range e.stub.Requests() {
		var blocks []string
		for _, b := range r.Request.System {
			blocks = append(blocks, b.Text)
		}
		texts = append(texts, blocks)
		n := len(r.Request.System)
		cached = append(cached, n > 0 && r.Request.System[n-1].CacheHint)
	}
	return texts, cached
}

// TestInitiatorInstructionsInThePrefix: the initiator's instructions the
// session's header records are rendered in their wrapper after the agent's
// instructions and before the machine's context, in the system prompt a
// provider caches, the same bytes on every request of every turn (spec
// 053).
func TestInitiatorInstructionsInThePrefix(t *testing.T) {
	ctx := t.Context()
	const standing = "Call me Ada. I work on the parser; answer in short paragraphs."
	e := setupSession(t, func(c *Config) { c.Instructions = "Be terse." }, func(s *session.Session) { s.Instructions = standing })
	attached, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Machine: session.AttachedMachine{Kind: "cella"}, Reason: "attached", Context: "<context>\nWorkdir: /work\n</context>"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, attached)
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("one")),
		reply(ir.StopEndTurn, text("two")),
	)
	e.send(ctx, "First.")
	e.turn(ctx)
	e.send(ctx, "Second.")
	e.turn(ctx)
	texts, cached := e.systemTexts()
	if len(texts) != 3 {
		t.Fatalf("%d requests", len(texts))
	}
	wrapper := prompts.Render(prompts.ContextInitiator, prompts.Data{"Text": standing})
	for i, blocks := range texts {
		if len(blocks) != 4 || blocks[1] != "Be terse." || blocks[2] != wrapper || blocks[3] != "<context>\nWorkdir: /work\n</context>" || !cached[i] {
			t.Fatalf("request %d: system %q, cached %v", i, blocks, cached[i])
		}
	}
}

// TestNoInitiatorInstructions: a session whose header records none
// renders no wrapper.
func TestNoInitiatorInstructions(t *testing.T) {
	ctx := t.Context()
	e := setupSession(t, func(c *Config) { c.Instructions = "Be terse." }, nil)
	e.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	e.send(ctx, "Hello.")
	e.turn(ctx)
	texts, _ := e.systemTexts()
	if len(texts) != 1 || len(texts[0]) != 2 || texts[0][1] != "Be terse." {
		t.Fatalf("system %q", texts)
	}
}
