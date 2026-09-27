// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package scripted

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
)

const script = `
steps:
  - thinking: plan first
    text: Listing the files.
    tool_calls:
      - name: bash
        input: {command: ls}
      - name: read
    usage: {input_tokens: 100, output_tokens: 20, cost_usd_micro: 7}
  - fail: {status: 529, retry_after: 2, times: 2, type: overloaded_error}
    text: after the failures
  - fail: {cut: true}
    text: after a cut
  - expect: {tool_result_contains: main.go, system_contains: agent, messages: 3}
    text: All done.
  - stop: max_tokens
    text: cut off
`

func conn(path string) models.Connection {
	return models.Connection{BaseURL: "scripted:" + path, Model: "scripted-model"}
}

func drain(t *testing.T, s models.Stream) (models.Result, error) {
	t.Helper()
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			return s.Result(), nil
		}
		if err != nil {
			return models.Result{}, err
		}
	}
}

func request(system string, results ...string) ir.Request {
	r := ir.Request{System: []ir.Block{{Type: ir.BlockText, Text: system}}, Messages: []ir.Message{{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "go"}}}}}
	for _, res := range results {
		r.Messages = append(r.Messages,
			ir.Message{Role: ir.RoleAssistant, Blocks: []ir.Block{{Type: ir.BlockText, Text: "..."}}},
			ir.Message{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "call_1_1", Blocks: []ir.Block{{Type: ir.BlockText, Text: res}}}}}},
		)
	}
	return r
}

func TestAScriptPlaysStepByStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{}
	ctx := t.Context()

	s, err := m.Stream(ctx, models.Request{IR: request("You are an agent."), Connection: conn(path), Capture: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	b := res.Message.Blocks
	if len(b) != 4 || b[0].Type != ir.BlockThinking || b[0].Signature != "scripted" || b[1].Text != "Listing the files." ||
		b[2].ToolUse.ID != "call_1_1" || string(b[2].ToolUse.Args) != `{"command":"ls"}` || b[3].ToolUse.ID != "call_1_2" || string(b[3].ToolUse.Args) != `{}` {
		t.Fatalf("blocks %+v", b)
	}
	if res.StopReason != ir.StopToolUse || res.Usage.InputTokens != 100 || res.Usage.CostUSDMicro == nil || *res.Usage.CostUSDMicro != 7 || res.Codec != Codec || res.RequestSHA256 == "" || len(res.RequestBytes) == 0 || len(res.RawResponse) == 0 {
		t.Fatalf("result %+v", res)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		_, err := m.Stream(ctx, models.Request{IR: request("x"), Connection: conn(path)})
		var he *models.HTTPError
		if !errors.As(err, &he) || he.Status != 529 || he.RetryAfter != 2*time.Second || !models.Retryable(err) {
			t.Fatalf("an injected failure: %v", err)
		}
	}
	s, err = m.Stream(ctx, models.Request{IR: request("x"), Connection: conn(path)})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := drain(t, s); err != nil || res.Message.Blocks[0].Text != "after the failures" {
		t.Fatalf("after the failures: %+v, %v", res, err)
	}

	s, err = m.Stream(ctx, models.Request{IR: request("x"), Connection: conn(path)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(t, s); !errors.Is(err, models.ErrIncomplete) {
		t.Fatalf("a cut stream: %v", err)
	}
	s, err = m.Stream(ctx, models.Request{IR: request("x"), Connection: conn(path)})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := drain(t, s); err != nil || res.Message.Blocks[0].Text != "after a cut" {
		t.Fatalf("after a cut: %+v, %v", res, err)
	}

	var ee *ExpectationError
	for _, r := range []ir.Request{request("You are an agent.", "nothing here"), request("wrong system", "main.go"), request("You are an agent.")} {
		if _, err := m.Stream(ctx, models.Request{IR: r, Connection: conn(path)}); !errors.As(err, &ee) || ee.Step != 4 || !strings.Contains(err.Error(), "the request was") {
			t.Fatalf("a failed expectation: %v", err)
		}
	}
	s, err = m.Stream(ctx, models.Request{IR: request("You are an agent.", "main.go"), Connection: conn(path)})
	if err != nil {
		t.Fatalf("a met expectation: %v", err)
	}
	if res, err := drain(t, s); err != nil || res.StopReason != ir.StopEndTurn {
		t.Fatalf("step 4: %+v, %v", res, err)
	}
	s, err = m.Stream(ctx, models.Request{IR: request("x"), Connection: conn(path)})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := drain(t, s); err != nil || res.StopReason != ir.StopMaxTokens {
		t.Fatalf("step 5: %+v, %v", res, err)
	}
	if m.Remaining(path) != 0 || m.Remaining("other") != -1 {
		t.Fatalf("remaining %d", m.Remaining(path))
	}
	if _, err := m.Stream(ctx, models.Request{IR: request("x"), Connection: conn(path)}); !errors.Is(err, ErrExhausted) {
		t.Fatalf("past the end: %v", err)
	}
}

func TestScriptsAreRefusedWhenMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"yaml":      "steps: [",
		"stop":      "steps:\n  - stop: later\n",
		"call name": "steps:\n  - tool_calls: [{input: {a: 1}}]\n",
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("%s: parsed", name)
		}
	}
	m := &Model{Load: func(string) ([]byte, error) { return nil, os.ErrNotExist }}
	if _, err := m.Stream(t.Context(), models.Request{Connection: conn("missing.yaml")}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing script: %v", err)
	}
	bad := &Model{Load: func(string) ([]byte, error) { return []byte("steps: ["), nil }}
	if _, err := bad.Stream(t.Context(), models.Request{Connection: conn("bad.yaml")}); err == nil {
		t.Fatal("a malformed script played")
	}
	if _, err := m.Stream(t.Context(), models.Request{Connection: models.Connection{BaseURL: "https://lux.example.com", Model: "m"}}); err == nil {
		t.Fatal("played a connection that is not scripted")
	}
}
