// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/test/stubs/luxstub"
)

func i64(v int64) *int64 { return &v }

func request(model string) ir.Request {
	max := int64(1024)
	return ir.Request{
		Model:     model,
		System:    []ir.Block{{Type: ir.BlockText, Text: "You are a builder."}},
		Messages:  []ir.Message{{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "List the files."}}}},
		Tools:     []ir.Tool{{Name: "bash", Description: "Run a command.", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}},
		MaxTokens: &max,
	}
}

func reply() ir.Response {
	return ir.Response{
		Model: "m",
		Blocks: []ir.Block{
			{Type: ir.BlockText, Text: "Listing."},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "bash", Args: json.RawMessage(`{"command":"ls"}`)}},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 120, OutputTokens: 30, CacheReadInputTokens: i64(40)},
	}
}

func drain(t *testing.T, s models.Stream) (models.Result, []ir.Event, error) {
	t.Helper()
	var evs []ir.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return s.Result(), evs, nil
		}
		if err != nil {
			return models.Result{}, evs, err
		}
		evs = append(evs, ev)
	}
}

func TestEveryDialectRoundTripsThroughTheStub(t *testing.T) {
	for _, c := range []struct {
		dialect ir.Dialect
		door    string
		auth    func(t *testing.T, r luxstub.Recorded)
	}{
		{ir.DialectAnthropicMessages, "/anthropic", func(t *testing.T, r luxstub.Recorded) {
			if r.Header.Get("x-api-key") != "sk-test" || r.Header.Get("anthropic-version") != AnthropicVersion || r.Header.Get("Authorization") != "" {
				t.Fatalf("messages headers %v", r.Header)
			}
		}},
		{ir.DialectOpenAIResponses, "/openai", func(t *testing.T, r luxstub.Recorded) {
			if r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("x-api-key") != "" {
				t.Fatalf("responses headers %v", r.Header)
			}
		}},
		{ir.DialectOpenAIChat, "/openai", func(t *testing.T, r luxstub.Recorded) {
			if r.Header.Get("Authorization") != "Bearer sk-test" {
				t.Fatalf("chat headers %v", r.Header)
			}
		}},
	} {
		t.Run(string(c.dialect), func(t *testing.T) {
			stub := luxstub.New(t)
			stub.Script("builder-model", luxstub.Reply{Response: reply(), Expect: func(r *ir.Request) error {
				if len(r.Tools) != 1 || r.Tools[0].Name != "bash" {
					return errors.New("the bash tool did not arrive")
				}
				return nil
			}})
			m := &Model{}
			conn := models.Connection{BaseURL: stub.URL() + c.door, Model: "builder-model", Dialect: c.dialect, Credential: "sk-test"}
			s, err := m.Stream(t.Context(), models.Request{IR: request("ignored"), Connection: conn, Capture: true})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			res, evs, err := drain(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) == 0 || res.StopReason != ir.StopToolUse {
				t.Fatalf("%d events, stop %q", len(evs), res.StopReason)
			}
			var text, call bool
			for _, b := range res.Message.Blocks {
				text = text || (b.Type == ir.BlockText && b.Text == "Listing.")
				call = call || (b.Type == ir.BlockToolUse && b.ToolUse.ID == "toolu_1" && string(b.ToolUse.Args) == `{"command":"ls"}`)
			}
			if !text || !call || res.Message.Role != ir.RoleAssistant {
				t.Fatalf("message %+v", res.Message)
			}
			if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 30 {
				t.Fatalf("usage %+v", res.Usage)
			}
			recs := stub.Requests()
			if len(recs) != 1 || recs[0].Dialect != c.dialect || recs[0].Model != "builder-model" {
				t.Fatalf("recorded %+v", recs)
			}
			c.auth(t, recs[0])
			sum := sha256.Sum256(recs[0].Body)
			if res.RequestSHA256 != hex.EncodeToString(sum[:]) || res.RequestSize != int64(len(recs[0].Body)) || string(res.RequestBytes) != string(recs[0].Body) {
				t.Fatal("the recorded request hash is not the hash of the bytes sent")
			}
			if len(res.RawResponse) == 0 || !strings.HasPrefix(res.Codec, string(c.dialect)+"@") {
				t.Fatalf("raw %d bytes, codec %q", len(res.RawResponse), res.Codec)
			}
			if _, _, err := drain(t, s); err != nil {
				t.Fatalf("Next after the end: %v", err)
			}
		})
	}
}

// TestABrokenCallStillEndsTheStream: a Chat Completions stream whose
// call's arguments break off inside a string, reported as tool_calls,
// ends as a result: the call holds the input {}, its text is in
// InvalidArgs, and the raw stream is kept.
func TestABrokenCallStillEndsTheStream(t *testing.T) {
	const broken = `{"command":"rm hello.cc hello.ccc'} }]}`
	chunk := func(delta string) string {
		return `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}` + "\n\n"
	}
	args, err := json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	raw := chunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}`) +
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":`+string(args)+`}}]}`) +
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	stub := luxstub.New(t)
	stub.Script("m", luxstub.Reply{Raw: raw})
	s, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: models.Connection{BaseURL: stub.URL() + "/openai", Model: "m", Dialect: ir.DialectOpenAIChat}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, _, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Message.Blocks) != 1 || string(res.Message.Blocks[0].ToolUse.Args) != `{}` || res.StopReason != ir.StopToolUse {
		t.Fatalf("message %+v, stop %s", res.Message.Blocks, res.StopReason)
	}
	if res.InvalidArgs["call_1"] != broken || string(res.RawResponse) != raw {
		t.Fatalf("invalid %q, raw %d bytes", res.InvalidArgs, len(res.RawResponse))
	}
}

func TestErrorsAreClassifiedForRetry(t *testing.T) {
	conn := func(stub *luxstub.Server) models.Connection {
		return models.Connection{BaseURL: stub.URL() + "/anthropic", Model: "m", Dialect: ir.DialectAnthropicMessages}
	}
	t.Run("429 with retry-after", func(t *testing.T) {
		stub := luxstub.New(t)
		stub.Script("m", luxstub.Reply{Response: reply(), Fail: &luxstub.Failure{Status: 429, RetryAfter: "3"}})
		_, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: conn(stub)})
		var he *models.HTTPError
		if !errors.As(err, &he) || he.Status != 429 || he.Type != "overloaded_error" || !models.Retryable(err) || models.RetryAfter(err) != 3*time.Second {
			t.Fatalf("429: %v", err)
		}
	})
	t.Run("400 is final", func(t *testing.T) {
		stub := luxstub.New(t)
		stub.Script("m", luxstub.Reply{Response: reply(), Fail: &luxstub.Failure{Status: 400, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`}})
		_, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: conn(stub)})
		var he *models.HTTPError
		if !errors.As(err, &he) || he.Type != "invalid_request_error" || he.Message != "bad" || models.Retryable(err) {
			t.Fatalf("400: %v", err)
		}
	})
	t.Run("a gateway's detail", func(t *testing.T) {
		stub := luxstub.New(t)
		const sent = `upstream status 429: {"error":{"message":"Rate limit exceeded","code":429}}`
		stub.Script("m", luxstub.Reply{Response: reply(), Fail: &luxstub.Failure{Status: 502, Detail: sent,
			Body: `{"type":"error","error":{"type":"upstream_error","message":"The provider returned an error."}}`}})
		_, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: conn(stub)})
		if !models.Down(err) || models.GatewayDetail(err) != sent || models.Described(err) != err.Error()+" ("+sent+")" {
			t.Fatalf("a gateway's detail: %v, %q", err, models.GatewayDetail(err))
		}
		if long := strings.Repeat("é", models.MaxDetail); len(detail(long)) != models.MaxDetail || !utf8.ValidString(detail(long)) || detail("short") != "short" {
			t.Fatal("a detail past models.MaxDetail is not cut on a character's boundary")
		}
		if models.Described(errors.New("other")) != "other" {
			t.Fatal("an error with no detail is described as more")
		}
	})
	t.Run("retry-after as a date", func(t *testing.T) {
		now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
		if got := retryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got != 90*time.Second {
			t.Fatalf("date retry-after %s", got)
		}
		if retryAfter("soon", now) != 0 || retryAfter("", now) != 0 {
			t.Fatal("an unreadable retry-after was honored")
		}
	})
	t.Run("a cut stream", func(t *testing.T) {
		stub := luxstub.New(t)
		stub.Script("m", luxstub.Reply{Response: reply(), Fail: &luxstub.Failure{Cut: true}})
		s, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: conn(stub)})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, _, err := drain(t, s); !models.Retryable(err) {
			t.Fatalf("a cut stream: %v, want a retryable error", err)
		}
		if _, err := s.Next(); err == nil {
			t.Fatal("Next after a failure succeeded")
		}
	})
	t.Run("a cut stream in every dialect", func(t *testing.T) {
		for d, door := range map[ir.Dialect]string{ir.DialectOpenAIResponses: "/openai", ir.DialectOpenAIChat: "/openai", ir.DialectAnthropicMessages: "/anthropic"} {
			stub := luxstub.New(t)
			stub.Script("m", luxstub.Reply{Response: reply(), Fail: &luxstub.Failure{Cut: true}})
			s, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: models.Connection{BaseURL: stub.URL() + door, Model: "m", Dialect: d}})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = drain(t, s)
			if !errors.Is(err, models.ErrIncomplete) {
				t.Fatalf("%s: a cut stream read as %v", d, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("an error event inside the stream", func(t *testing.T) {
		for typ, retry := range map[string]bool{"overloaded_error": true, "invalid_request_error": false} {
			stub := luxstub.New(t)
			ev := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"" + typ + "\",\"message\":\"x\"}}\n\n"
			stub.Script("m", luxstub.Reply{Response: reply(), Fail: &luxstub.Failure{Event: ev}})
			s, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: conn(stub)})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = drain(t, s)
			var se *models.StreamError
			if !errors.As(err, &se) || models.Retryable(err) != retry {
				t.Fatalf("%s: %v, retryable %v", typ, err, models.Retryable(err))
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("an unreachable server", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: models.Connection{BaseURL: "http://" + addr, Model: "m"}})
		var te *models.TransportError
		if !errors.As(err, &te) || !models.Retryable(err) {
			t.Fatalf("refused connection: %v", err)
		}
	})
	t.Run("a canceled request", func(t *testing.T) {
		stub := luxstub.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := (&Model{}).Stream(ctx, models.Request{IR: request("m"), Connection: conn(stub)})
		if !errors.Is(err, context.Canceled) || models.Retryable(err) {
			t.Fatalf("canceled: %v", err)
		}
	})
}

func TestStreamRefusesWhatItCannotSend(t *testing.T) {
	m := &Model{}
	for name, conn := range map[string]models.Connection{
		"no base url": {Model: "m"},
		"scripted":    {BaseURL: "scripted:testdata/x.yaml", Model: "m"},
		"no model":    {BaseURL: "https://lux.example.com"},
	} {
		if _, err := m.Stream(t.Context(), models.Request{IR: request("m"), Connection: conn}); err == nil {
			t.Fatalf("%s: sent", name)
		}
	}
	if _, _, err := codec("gemini"); err == nil {
		t.Fatal("a codec for an unknown dialect")
	}
	bad := request("m")
	bad.ToolChoice = &ir.ToolChoice{Mode: "sometimes"}
	stub := luxstub.New(t)
	_, err := m.Stream(t.Context(), models.Request{IR: bad, Connection: models.Connection{BaseURL: stub.URL() + "/anthropic", Model: "m", Dialect: ir.DialectAnthropicMessages}})
	var ee *models.EncodeError
	if !errors.As(err, &ee) || models.Retryable(err) {
		t.Fatalf("an unencodable request: %v", err)
	}
}

func TestCodecVersionNamesTheDialect(t *testing.T) {
	if v := CodecVersion(ir.DialectOpenAIChat); !strings.HasPrefix(v, "openai-chat@") || len(v) <= len("openai-chat@") {
		t.Fatalf("codec version %q", v)
	}
}
