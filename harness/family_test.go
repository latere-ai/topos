// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// TestFamilyChangeReportsLoss: a session that moves from an Anthropic
// model to an OpenAI Responses one and back sends each request in the
// current family's encoding. What that family cannot carry, the other
// family's thinking and its reasoning item, is dropped from the bytes
// sent and named in the model.request's loss, while each family's own
// blocks come back to it intact.
func TestFamilyChangeReportsLoss(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	messages := e.h
	cfg := e.cfg
	cfg.Connection = models.Connection{BaseURL: e.stub.URL() + "/openai", Model: model, Family: models.FamilyOpenAI, Dialect: ir.DialectOpenAIResponses}
	responses, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	item := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"weigh it"}],"encrypted_content":"enc-1"}`)
	e.stub.Script(model,
		reply(ir.StopEndTurn, ir.Block{Type: ir.BlockThinking, Text: "plan", Signature: "sig-1"}, ir.Block{Type: ir.BlockRedactedThinking, Redacted: "red-1"}, text("Planned.")),
		reply(ir.StopEndTurn, ir.Block{Type: ir.BlockOpaque, Opaque: &ir.Opaque{Dialect: ir.DialectOpenAIResponses, Kind: "reasoning", Raw: item}}, text("Weighed.")),
		reply(ir.StopEndTurn, text("Done.")),
	)
	for i, h := range []*Harness{messages, responses, messages} {
		e.h = h
		e.send(ctx, "Next.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("turn %d: %+v", i+1, out)
		}
	}
	var loss [][]string
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var mr session.ModelRequest
		if err := ev.Decode(&mr); err != nil {
			t.Fatal(err)
		}
		loss = append(loss, mr.Loss)
	}
	// Responses has no cache breakpoints either, which its loss also
	// names; the content lost is thinking one way and the reasoning item
	// and its summary the other.
	if len(loss) != 3 || len(loss[0]) != 0 || !slices.Contains(loss[1], "thinking") || slices.Contains(loss[1], "opaque") || !slices.Contains(loss[2], "thinking") || !slices.Contains(loss[2], "opaque") {
		t.Fatalf("the requests' loss %q", loss)
	}
	reqs := e.stub.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	toResponses, backToMessages := string(reqs[1].Body), string(reqs[2].Body)
	if strings.Contains(toResponses, "sig-1") || strings.Contains(toResponses, "red-1") {
		t.Fatalf("the Responses request carries Anthropic thinking: %s", toResponses)
	}
	if strings.Contains(backToMessages, "enc-1") || strings.Contains(backToMessages, "weigh it") {
		t.Fatalf("the Messages request carries the reasoning item: %s", backToMessages)
	}
	if !strings.Contains(backToMessages, `"signature":"sig-1"`) || !strings.Contains(backToMessages, `"data":"red-1"`) {
		t.Fatalf("the Messages request lost its own thinking: %s", backToMessages)
	}
}
