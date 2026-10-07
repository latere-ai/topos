// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// TestTheAllowsContextAndTheAttachmentsBlock: the context an allow
// attached follows the initiator's instructions, a block per part with
// the first saying what they are, and then the attachments block names
// each attached app's checkout, name, address and slug, every request of
// every turn the same bytes, before the machine's context; the
// repositories block before a machine names no repository with an app.
func TestTheAllowsContextAndTheAttachmentsBlock(t *testing.T) {
	ctx := t.Context()
	app := &session.ResourceApp{Slug: "tide-tables", Name: "Tide tables", URL: "https://tide-tables.apps.example.com"}
	parts := []session.ContextPart{{Title: "Project", Text: "Tide tables for the harbor club.\n"}, {Title: "Memory", Text: "- ent_01: The club's color is navy."}}
	e := setupSession(t, func(c *Config) { c.Instructions = "Be terse." }, func(s *session.Session) {
		s.Instructions = "Call me Ada."
		s.Context = parts
		s.Resources = []session.Resource{
			{Type: session.ResourceRepository, URL: "https://git.example.com/acme/web.git"},
			{Type: session.ResourceRepository, URL: "https://git.example.com/r/7f3c.git", App: app, Attached: true},
		}
	})
	e.stub.Script(model, reply(ir.StopEndTurn, text("one")), reply(ir.StopEndTurn, text("two")))
	e.send(ctx, "First.")
	e.turn(ctx)
	attached, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Machine: session.AttachedMachine{Kind: "cella"}, Reason: "attached", Context: "<context>\nWorkdir: /work\n</context>"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, attached)
	e.send(ctx, "Second.")
	e.turn(ctx)
	texts, _ := e.systemTexts()
	if len(texts) != 2 {
		t.Fatalf("%d requests", len(texts))
	}
	header := []string{
		"Be terse.",
		prompts.Render(prompts.ContextInitiator, prompts.Data{"Text": "Call me Ada."}),
		prompts.Render(prompts.ContextAttached, prompts.Data{"First": true, "Title": "Project", "Text": "Tide tables for the harbor club."}),
		prompts.Render(prompts.ContextAttached, prompts.Data{"First": false, "Title": "Memory", "Text": "- ent_01: The club's color is navy."}),
		"<attachments>\n" + "Apps, each a checkout of its source in the working directory, on your branch " + session.Branch(e.s) + ":\n" +
			`- tide-tables/ : Tide tables, published at https://tide-tables.apps.example.com (publish with app "tide-tables")` + "\n" + "</attachments>",
	}
	for i, blocks := range texts {
		if len(blocks) != len(header)+2 || !slices.Equal(blocks[1:len(header)+1], header) {
			t.Fatalf("request %d: system %q", i, blocks)
		}
	}
	if before := texts[0][len(header)+1]; !strings.Contains(before, "acme/web.git") || strings.Contains(before, "7f3c") {
		t.Fatalf("the repositories block before a machine: %q", before)
	}
	if after := texts[1][len(header)+1]; after != "<context>\nWorkdir: /work\n</context>" {
		t.Fatalf("the machine's context: %q", after)
	}
	if !strings.HasPrefix(header[2], "The installation that runs this session") || strings.HasPrefix(header[3], "The installation") {
		t.Fatalf("the first part opens with what the parts are, and only the first: %q, %q", header[2], header[3])
	}
}

// TestHeaderBlocksOfASubagent: a subagent's thread reads the attachments
// block and neither the initiator's instructions nor the attached
// context, which speak to the session's own agent; a session with no app
// renders no attachments block, and one whose apps alone are its
// repositories renders no repositories block.
func TestHeaderBlocksOfASubagent(t *testing.T) {
	app := &session.ResourceApp{Slug: "tide", Name: "Tide", URL: "https://tide.apps.example.com"}
	s := session.Session{ID: "ses_1", Agent: session.AgentRef{Name: "assistant"}, Instructions: "Call me Ada.", Context: []session.ContextPart{{Title: "Project", Text: "P."}},
		Resources: []session.Resource{{Type: session.ResourceRepository, URL: "https://git.example.com/r/1.git", App: app, Attached: true}}}
	own, sub := headerBlocks(s, true), headerBlocks(s, false)
	if len(own) != 3 || len(sub) != 1 || sub[0] != own[2] || !strings.HasPrefix(sub[0], "<attachments>") || !strings.Contains(sub[0], "- tide/ : Tide") {
		t.Fatalf("own %q, subagent %q", own, sub)
	}
	if got := headerBlocks(session.Session{ID: "ses_2"}, true); got != nil {
		t.Fatalf("a bare session's header blocks %q", got)
	}
	if got := withRepositories(nil, s); got != nil {
		t.Fatalf("a session whose repositories are apps alone named %v", got)
	}
}
