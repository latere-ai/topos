// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestAClaudeSessionSeesItsImage: a hosted session on a Claude model, on
// an installation without TOPOS_MODELS_KEY whose runner reads the door
// with the session's own key, and whose door lists the model without its
// modalities, as Lux lists a discovered Model, sends a message's image
// to the model as an image block: the embedded catalog says the model
// takes images.
func TestAClaudeSessionSeesItsImage(t *testing.T) {
	const claude = "anthropic/claude-sonnet-4-5"
	stub := luxstub.New(t)
	stub.Models(bridge.Model{Name: claude, ContextWindow: 1_000_000, MaxOutputTokens: 64_000})
	stub.Script(claude, luxstub.Reply{Response: ir.Response{Model: claude, Blocks: []ir.Block{{Type: ir.BlockText, Text: "A chart."}}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}}})
	st := session.NewMemoryStore()
	var asked v1.Machine
	h, err := Harness(Options{Store: st, ModelsURL: stub.URL() + "/anthropic", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t, st, strings.Replace(reviewer, "model: {name: anthropic/claude-haiku-4.5}", "model: {name: "+claude+"}", 1))
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nchart"))
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{
		{Type: ir.BlockText, Text: "What does the chart show?"},
		{Type: ir.BlockImage, Image: &lux.Image{MediaType: "image/png", Data: png}},
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{msg}
	session.Stamp(s.ID, 0, evs)
	if _, err := st.Append(t.Context(), s.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Options{Store: st, Harness: h, ID: "run_images", Kind: runner.KindServe,
		Credentials: func(string, session.Lease) runner.Credentials { return &issued{life: time.Hour} }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Drive(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	reqs := stub.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	var image bool
	for _, b := range reqs[0].Request.Messages[0].Blocks {
		if b.Type == ir.BlockImage && b.Image != nil && b.Image.Data == png {
			image = true
		}
		if b.Type == ir.BlockText && strings.Contains(b.Text, "cannot see") {
			t.Fatalf("the model got the note in the image's place: %q", b.Text)
		}
	}
	if !image {
		t.Fatalf("the request carried no image: %+v", reqs[0].Request.Messages[0].Blocks)
	}
	if listed := stub.Listed(); len(listed) == 0 || !bytes.HasPrefix([]byte(listed[0].Get("X-Api-Key")), []byte("lux-session-")) {
		t.Fatalf("the door's list was not read with the session's key: %v", listed)
	}
}
