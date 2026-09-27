// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxstub

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
)

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

const chatBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestTheStubAnswersAsScripted(t *testing.T) {
	s := New(t)
	if code, _ := post(t, s.URL()+"/nowhere", chatBody); code != http.StatusNotFound {
		t.Fatalf("an unknown door: %d", code)
	}
	if code, _ := post(t, s.URL()+PathChat, `{`); code != http.StatusBadRequest {
		t.Fatalf("an undecodable body: %d", code)
	}
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusNotFound || !strings.Contains(body, "no scripted reply") {
		t.Fatalf("no reply: %d %s", code, body)
	}
	s.Script("m",
		Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "one"}}}, Fail: &Failure{Status: 503, Times: 2}},
		Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "two"}}}, Expect: func(*ir.Request) error { return errors.New("wanted a tool result") }},
	)
	for range 2 {
		if code, _ := post(t, s.URL()+PathChat, chatBody); code != http.StatusServiceUnavailable {
			t.Fatalf("an injected failure: %d", code)
		}
	}
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusOK || !strings.Contains(body, "one") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("the reply after two failures: %d %s", code, body)
	}
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusBadRequest || !strings.Contains(body, "wanted a tool result") {
		t.Fatalf("a failed expectation: %d %s", code, body)
	}
	if n := len(s.Requests()); n != 5 {
		t.Fatalf("%d requests recorded", n)
	}
}

func TestEventsFollowTheGrammar(t *testing.T) {
	evs := Events(ir.Response{Blocks: []ir.Block{
		{Type: ir.BlockThinking, Text: "t", Signature: "s"},
		{Type: ir.BlockRedactedThinking, Redacted: "r"},
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "a", Name: "b", Args: []byte(`{}`)}},
	}})
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Type))
	}
	want := "message_start block_start thinking_delta signature_delta block_stop block_start block_stop block_start args_delta block_stop message_delta message_stop"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events %s", got)
	}
	if evs[0].ID != "msg_stub" || evs[len(evs)-2].StopReason != ir.StopEndTurn || evs[5].Block.Redacted != "r" {
		t.Fatalf("defaults %+v", evs)
	}
}

func TestAnInjectedErrorEventFollowsThePartialStream(t *testing.T) {
	s := New(t)
	s.Script("m", Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "partial"}}}, Fail: &Failure{Event: "event: error\ndata: {}\n\n"}})
	code, body := post(t, s.URL()+PathChat, chatBody)
	if code != http.StatusOK || !bytes.HasSuffix([]byte(body), []byte("event: error\ndata: {}\n\n")) || strings.Contains(body, "[DONE]") {
		t.Fatalf("%d %q", code, body)
	}
}
