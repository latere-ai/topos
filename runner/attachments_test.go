// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// attachment stores body as a blob of the fixture's session and returns
// the attachment the message whose event id is message names it by.
func (f *fixture) attachment(ctx context.Context, message, name, body string) session.Attachment {
	f.t.Helper()
	d, err := f.store.PutBlob(ctx, f.s.ID, bytes.NewReader([]byte(body)))
	if err != nil {
		f.t.Fatal(err)
	}
	return session.Attachment{Name: name, MediaType: "text/plain", Size: int64(len(body)), Blob: d, Path: session.AttachmentPath(message, name)}
}

// poster is a tool that sends a message with a file while the turn runs.
type poster struct {
	f *fixture
	a func(ctx context.Context) session.Attachment
}

func (poster) Definition() tools.Definition {
	return tools.Definition{Name: "post", Description: "sends a message with a file", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (poster) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectRead} }
func (p poster) Run(ctx context.Context, _ tools.Call) (tools.Result, error) {
	p.f.send(ctx, session.TypeUserMessage, session.UserMessage{Sender: p.f.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And this one."}}, Attachments: []session.Attachment{p.a(ctx)}})
	return tools.Text(tools.OutcomeOK, "posted"), nil
}

// delivered are the paths of the fixture session's attachments.delivered
// events, one list per event.
func (f *fixture) delivered(ctx context.Context) [][]string {
	f.t.Helper()
	var out [][]string
	for _, e := range f.events(ctx, session.TypeAttachmentsDelivered) {
		var p session.AttachmentsDelivered
		if err := e.Decode(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p.Paths)
	}
	return out
}

// TestAMessagesFilesReachTheMachine: a file a message carries is written
// under attachments/ when a tool first opens the machine, before that
// tool runs, and the model is told its path; a file sent while the
// machine is open is written before the next step's request; a later
// drive writes no file twice, and the working directory's repository
// excludes the directory.
func TestAMessagesFilesReachTheMachine(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	work := filepath.Join(filepath.Dir(f.work), "fresh")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "--quiet", "-b", "main")
	var opens atomic.Int32
	f.onDemand(work, &opens)
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1}, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: machine.KindCella}, t0)
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
	base := f.r.o.Harness
	f.r.o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		c, err := base(ctx, s)
		if err != nil {
			return c, err
		}
		return c, c.Tools.AddBuiltin(poster{f: f, a: func(ctx context.Context) session.Attachment {
			return f.attachment(ctx, "evt_later", "later.txt", "sent mid-turn\n")
		}})
	}
	notes := f.attachment(ctx, "evt_notes", "notes.txt", "Remember the milk.\n")
	f.send(ctx, session.TypeUserMessage, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Read my notes."}}, Attachments: []session.Attachment{notes}})
	f.stub.Script(model,
		reply(toolUse("toolu_1", "bash", `{"command":"cat attachments/evt_notes/notes.txt"}`)),
		reply(toolUse("toolu_2", "post", `{}`)),
		reply(toolUse("toolu_3", "bash", `{"command":"cat attachments/evt_later/later.txt && git status --porcelain"}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "Read both."}),
	)
	if _, err := f.r.Drive(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if first := f.stub.Requests()[0].Request.Messages[0]; !strings.Contains(first.Blocks[len(first.Blocks)-1].Text, "- attachments/evt_notes/notes.txt (text/plain, 19 bytes)") {
		t.Fatalf("the model was not told the file's path: %+v", first.Blocks)
	}
	var results []string
	for _, e := range f.events(ctx, session.TypeToolResult) {
		var p session.ToolResult
		if err := e.Decode(&p); err != nil || p.IsError {
			t.Fatalf("a call failed: %+v, %v", p, err)
		}
		results = append(results, p.Content[0].Text)
	}
	if len(results) != 3 || !strings.Contains(results[0], "Remember the milk.") || !strings.Contains(results[2], "sent mid-turn") {
		t.Fatalf("the calls read %q", results)
	}
	if strings.Contains(results[2], "attachments") {
		t.Fatalf("git sees the attachments: %q", results[2])
	}
	if got := f.delivered(ctx); len(got) != 2 || !slices.Equal(got[0], []string{"attachments/evt_notes/notes.txt"}) || !slices.Equal(got[1], []string{"attachments/evt_later/later.txt"}) {
		t.Fatalf("attachments.delivered %v", got)
	}
	f.stub.Script(model, reply(toolUse("toolu_4", "bash", `{"command":"ls attachments"}`)), reply(ir.Block{Type: ir.BlockText, Text: "Listed."}))
	f.message(ctx, "List them.")
	if _, err := f.r.Drive(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.delivered(ctx); len(got) != 2 || opens.Load() != 2 {
		t.Fatalf("a later drive wrote %v with %d opens", got, opens.Load())
	}
}
