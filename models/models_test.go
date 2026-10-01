// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

func i64(v int64) *int64 { return &v }

func TestConnectionValidate(t *testing.T) {
	good := Connection{BaseURL: "https://lux.example.com/anthropic", Model: "claude-opus-5", Family: FamilyAnthropic}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if good.EffectiveDialect() != ir.DialectAnthropicMessages || good.Scripted() {
		t.Fatal("dialect or scripted")
	}
	for name, c := range map[string]Connection{
		"zero":        {},
		"no model":    {BaseURL: "https://lux.example.com"},
		"blank model": {BaseURL: "https://lux.example.com", Model: "  "},
		"no host":     {BaseURL: "https://", Model: "m"},
		"ftp":         {BaseURL: "ftp://lux.example.com", Model: "m"},
		"bad url":     {BaseURL: "http://[::1", Model: "m"},
		"family":      {BaseURL: "https://lux.example.com", Model: "m", Family: "gemini"},
		"dialect":     {BaseURL: "https://lux.example.com", Model: "m", Dialect: "lux"},
	} {
		if err := c.Validate(); err == nil {
			t.Fatalf("%s: valid", name)
		}
	}
	s := Connection{BaseURL: "scripted:testdata/steps.yaml", Model: "m"}
	if s.Validate() != nil || !s.Scripted() {
		t.Fatal("a scripted connection")
	}
	for fam, d := range map[string]ir.Dialect{FamilyAnthropic: ir.DialectAnthropicMessages, FamilyOpenAI: ir.DialectOpenAIResponses, FamilyOther: ir.DialectOpenAIChat, "": ir.DialectOpenAIChat} {
		if DefaultDialect(fam) != d {
			t.Fatalf("DefaultDialect(%q) = %s", fam, DefaultDialect(fam))
		}
	}
	if (Connection{Family: FamilyOpenAI, Dialect: ir.DialectOpenAIChat}).EffectiveDialect() != ir.DialectOpenAIChat {
		t.Fatal("an explicit dialect lost to the family default")
	}
}

func TestRetryable(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{&HTTPError{Status: 429}, true},
		{&HTTPError{Status: 529}, true},
		{&HTTPError{Status: 503}, true},
		{&HTTPError{Status: 400}, false},
		{&HTTPError{Status: 401}, false},
		{&HTTPError{Status: 413}, false},
		{&StreamError{Err: errors.New("anthropic: upstream error (overloaded_error): busy")}, true},
		{&StreamError{Err: errors.New("rate_limit_exceeded")}, true},
		{&StreamError{Err: errors.New("invalid_request_error")}, false},
		{&TransportError{Err: errors.New("connection reset")}, true},
		{ErrIncomplete, true},
		{fmt.Errorf("wrapped: %w", &HTTPError{Status: 502}), true},
		{&EncodeError{Err: errors.New("bad")}, false},
		{errors.New("other"), false},
	} {
		if got := Retryable(c.err); got != c.want {
			t.Fatalf("Retryable(%v) = %v", c.err, got)
		}
	}
	if RetryAfter(&HTTPError{Status: 429, RetryAfter: time.Second}) != time.Second || RetryAfter(errors.New("x")) != 0 {
		t.Fatal("RetryAfter")
	}
}

func TestErrorMessages(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&HTTPError{Status: 429, Type: "rate_limit_error", Message: "slow down"}, "HTTP 429: rate_limit_error: slow down"},
		{&HTTPError{Status: 500}, "HTTP 500"},
		{&StreamError{Err: errors.New("x")}, "the stream failed: x"},
		{&TransportError{Err: errors.New("reset")}, "transport: reset"},
		{&EncodeError{Err: errors.New("bad")}, "encode the request: bad"},
		{&Coded{Code: CodeUnknown, Message: "m"}, "model_unknown: m"},
	} {
		if !strings.Contains(c.err.Error(), c.want) {
			t.Fatalf("%q does not contain %q", c.err.Error(), c.want)
		}
	}
	inner := errors.New("inner")
	if !errors.Is(&StreamError{Err: inner}, inner) || !errors.Is(&TransportError{Err: inner}, inner) || !errors.Is(&EncodeError{Err: inner}, inner) {
		t.Fatal("an error does not unwrap")
	}
}

func TestPrices(t *testing.T) {
	for in, want := range map[string]Price{"0": 0, "1.25": 1_250_000, "15": 15_000_000, "0.000001": 1, "0.5": 500_000} {
		got, err := ParsePrice(in)
		if err != nil || got != want {
			t.Fatalf("ParsePrice(%q) = %d, %v", in, got, err)
		}
		if back, _ := ParsePrice(got.String()); back != got {
			t.Fatalf("%q does not round-trip through %q", in, got.String())
		}
	}
	for _, bad := range []string{"", "-1", "1.1234567", "x", "1.x", ".5"} {
		if _, err := ParsePrice(bad); err == nil {
			t.Fatalf("ParsePrice(%q) accepted", bad)
		}
	}
	var p Price
	if err := json.Unmarshal([]byte(`"2.5"`), &p); err != nil || p != 2_500_000 {
		t.Fatalf("unmarshal: %d, %v", p, err)
	}
	if err := json.Unmarshal([]byte(`2.5`), &p); err == nil {
		t.Fatal("a price as a number")
	}
	if err := json.Unmarshal([]byte(`"nope"`), &p); err == nil {
		t.Fatal("a price that is not a decimal")
	}
	b, err := json.Marshal(Price(1_250_000))
	if err != nil || string(b) != `"1.25"` {
		t.Fatalf("marshal %s, %v", b, err)
	}
}

func price(s string) *Price {
	p, err := ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return &p
}

func TestCost(t *testing.T) {
	opus := Entry{Name: "opus", Pricing: &Pricing{Input: price("5"), Output: price("25"), CacheRead: price("0.5")}}
	cost, src, err := Cost(UsageFigures{Input: 1000, Output: 200, CacheRead: i64(10_000), CacheWrite: i64(2000)}, opus)
	// 1000*5 + 200*25 + 10000*0.5 + 2000*6.25 = 5000 + 5000 + 5000 + 12500 micro-USD
	if err != nil || src != CostCatalog || cost != 27_500 {
		t.Fatalf("catalog cost %d %s %v", cost, src, err)
	}
	cost, src, err = Cost(UsageFigures{Input: 1, ReportedCostUSDMicro: i64(42)}, Entry{})
	if err != nil || src != CostProvider || cost != 42 {
		t.Fatalf("provider cost %d %s %v", cost, src, err)
	}
	if _, _, err := Cost(UsageFigures{Input: 1}, Entry{}); !errors.Is(err, ErrUnpriced) {
		t.Fatalf("unpriced: %v", err)
	}
	cost, _, err = Cost(UsageFigures{Input: 1, CacheRead: i64(3)}, Entry{Pricing: &Pricing{Input: price("0.1"), Output: price("0.1")}})
	if err != nil || cost != 1 {
		t.Fatalf("a cache read with no price costs %d, %v; want the input rate rounded up", cost, err)
	}
	est, err := EstimateInput(100_000, opus)
	if err != nil || est != 500_000 {
		t.Fatalf("estimate %d, %v", est, err)
	}
	if _, err := EstimateInput(1, Entry{}); !errors.Is(err, ErrUnpriced) {
		t.Fatal("an estimate of an unpriced model")
	}
	u := FromLux(lux.Usage{InputTokens: 3, OutputTokens: 4, CacheReadInputTokens: i64(5), CostUSDMicro: i64(6)})
	if u.Input != 3 || u.Output != 4 || *u.CacheRead != 5 || u.CacheWrite != nil || *u.ReportedCostUSDMicro != 6 {
		t.Fatalf("FromLux %+v", u)
	}
}

func TestEmbeddedCatalog(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) == 0 || c.Source == "" {
		t.Fatalf("catalog of %d entries from %q", len(c.Models), c.Source)
	}
	e, err := c.Resolve("claude-opus-5")
	if err != nil {
		t.Fatalf("resolve by alias: %v", err)
	}
	if e.Family != FamilyAnthropic || e.InputWindow <= 0 || e.MaxOutputTokens <= 0 || e.Pricing == nil {
		t.Fatalf("claude-opus-5 %+v", e)
	}
	free := 0
	for _, m := range c.Models {
		if strings.HasSuffix(m.Name, ":free") {
			free++
			if m.Pricing == nil || *m.Pricing.Input != 0 || m.Dialect != ir.DialectOpenAIChat {
				t.Fatalf("free model %+v", m)
			}
		}
	}
	if free == 0 {
		t.Fatal("no free development model")
	}
}

func TestResolveOverlaysSources(t *testing.T) {
	c := Catalog{Models: []Entry{{Name: "a", Aliases: []string{"a-1"}, Family: FamilyOther, InputWindow: 1000, Pricing: &Pricing{Input: price("1")}}}}
	if _, err := c.Resolve("a"); err == nil {
		t.Fatal("a model with no output limit resolved")
	}
	var coded *Coded
	_, err := c.Resolve("unknown")
	if !errors.As(err, &coded) || coded.Code != CodeUnknown {
		t.Fatalf("an unknown model: %v", err)
	}
	lux := Entry{MaxOutputTokens: 500, Pricing: &Pricing{Output: price("2")}, Supports: Supports{Images: true}}
	agent := Entry{Family: FamilyOpenAI, Dialect: ir.DialectOpenAIChat, InputWindow: 800, Pricing: &Pricing{Input: price("3"), CacheRead: price("0.3"), CacheWrite: price("4")}, Supports: Supports{Thinking: true, Effort: true, ParallelTools: true}}
	e, err := c.Resolve("a-1", lux, agent)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "a" || e.Family != FamilyOpenAI || e.Dialect != ir.DialectOpenAIChat || e.InputWindow != 800 || e.MaxOutputTokens != 500 {
		t.Fatalf("overlay %+v", e)
	}
	if *e.Pricing.Input != 3_000_000 || *e.Pricing.Output != 2_000_000 || *e.Pricing.CacheWrite != 4_000_000 || !e.Supports.Images || !e.Supports.Thinking {
		t.Fatalf("overlay pricing %+v supports %+v", e.Pricing, e.Supports)
	}
	if *c.Models[0].Pricing.Input != 1_000_000 {
		t.Fatal("Resolve changed the catalog's own entry")
	}
}

func TestAccumulator(t *testing.T) {
	var a Accumulator
	for _, ev := range []ir.Event{
		{Type: ir.EventMessageStart, ID: "msg_1", Model: "m", Usage: &ir.Usage{InputTokens: 10}},
		{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking}},
		{Type: ir.EventThinkingDelta, Index: 0, Delta: "plan "},
		{Type: ir.EventThinkingDelta, Index: 0, Delta: "it"},
		{Type: ir.EventSignatureDelta, Index: 0, Delta: "sig"},
		{Type: ir.EventBlockStop, Index: 0},
		{Type: ir.EventBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "bash"}}},
		{Type: ir.EventArgsDelta, Index: 1, Delta: `{"command":`},
		{Type: ir.EventArgsDelta, Index: 1, Delta: `"ls"}`},
		{Type: ir.EventBlockStop, Index: 1},
		{Type: ir.EventBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t2", Name: "pwd"}}},
		{Type: ir.EventBlockStop, Index: 2},
		{Type: ir.EventMessageDelta, StopReason: ir.StopToolUse, StopSequence: "", Usage: &ir.Usage{OutputTokens: 7, CacheReadInputTokens: i64(3), CostUSDMicro: i64(9), ReasoningTokens: 2}},
		{Type: ir.EventMessageStop},
	} {
		if err := a.Add(ev); err != nil {
			t.Fatalf("%s: %v", ev.Type, err)
		}
	}
	r := a.Response()
	if !a.Stopped() || r.ID != "msg_1" || r.StopReason != ir.StopToolUse || len(r.Blocks) != 3 {
		t.Fatalf("response %+v", r)
	}
	if r.Blocks[0].Text != "plan it" || r.Blocks[0].Signature != "sig" || string(r.Blocks[1].ToolUse.Args) != `{"command":"ls"}` || string(r.Blocks[2].ToolUse.Args) != `{}` {
		t.Fatalf("blocks %+v", r.Blocks)
	}
	if r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 7 || *r.Usage.CacheReadInputTokens != 3 || *r.Usage.CostUSDMicro != 9 || r.Usage.ReasoningTokens != 2 {
		t.Fatalf("usage %+v", r.Usage)
	}
	msg, usage, err := LuxMessage(r)
	if err != nil || msg.Role != ir.RoleAssistant || len(msg.Blocks) != 3 || usage.OutputTokens != 7 {
		t.Fatalf("lux message %+v %+v %v", msg, usage, err)
	}
	if err := a.Add(ir.Event{Type: ir.EventTextDelta}); err == nil {
		t.Fatal("an event after message_stop")
	}
	for name, evs := range map[string][]ir.Event{
		"start without a block":   {{Type: ir.EventBlockStart}},
		"delta to a closed block": {{Type: ir.EventTextDelta, Index: 4}},
		"stop of a closed block":  {{Type: ir.EventBlockStop, Index: 1}},
		"unknown event":           {{Type: "ping"}},
	} {
		var b Accumulator
		var err error
		for _, ev := range evs {
			if err = b.Add(ev); err != nil {
				break
			}
		}
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// TestAccumulatorSettlesEveryCallsArguments: whatever a stream sends as
// a call's arguments, the response encodes as Lux wire. Arguments that
// are not one JSON value are the input {} with their text kept by tool
// use ID; arguments for a block that already stopped are appended to it;
// a block the stream never stopped is closed at the message's end with
// what it received.
func TestAccumulatorSettlesEveryCallsArguments(t *testing.T) {
	start := func(i int, id string, head string) ir.Event {
		tu := &ir.ToolUse{ID: id, Name: "bash"}
		if head != "" {
			tu.Args = json.RawMessage(head)
		}
		return ir.Event{Type: ir.EventBlockStart, Index: i, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: tu}}
	}
	args := func(i int, d string) ir.Event { return ir.Event{Type: ir.EventArgsDelta, Index: i, Delta: d} }
	stop := func(i int) ir.Event { return ir.Event{Type: ir.EventBlockStop, Index: i} }
	end := []ir.Event{{Type: ir.EventMessageDelta, StopReason: ir.StopToolUse}, {Type: ir.EventMessageStop}}
	truncated := `{"command":"rm hello.cc hello.ccc'} }]}]} }]}>}?  the files have been removed`
	for _, c := range []struct {
		name    string
		events  []ir.Event
		want    []string
		invalid map[string]string
	}{
		{"empty", []ir.Event{start(0, "t1", ""), args(0, ""), stop(0)}, []string{`{}`}, nil},
		{"whitespace", []ir.Event{start(0, "t1", ""), args(0, " \n"), stop(0)}, []string{`{}`}, nil},
		{"truncated", []ir.Event{start(0, "t1", ""), args(0, truncated[:20]), args(0, truncated[20:]), stop(0)}, []string{`{}`}, map[string]string{"t1": truncated}},
		{"two values", []ir.Event{start(0, "t1", ""), args(0, `{"command":"ls"}{"command":"pwd"}`), stop(0)}, []string{`{}`}, map[string]string{"t1": `{"command":"ls"}{"command":"pwd"}`}},
		{"interleaved", []ir.Event{
			start(0, "t1", ""), stop(0), start(1, "t2", ""),
			args(0, `{"command":`), args(1, `{"command":`), args(0, `"ls"}`), args(1, `"pwd"}`), stop(1),
		}, []string{`{"command":"ls"}`, `{"command":"pwd"}`}, nil},
		{"interleaved past a truncation", []ir.Event{
			start(0, "t1", ""), args(0, `{"command":`), stop(0), start(1, "t2", ""), args(1, `{}`), args(0, `"ls"}`), stop(1),
		}, []string{`{"command":"ls"}`, `{}`}, nil},
		{"never stopped", []ir.Event{start(0, "t1", ""), args(0, `{"command":"ls"}`)}, []string{`{"command":"ls"}`}, nil},
		{"never stopped and truncated", []ir.Event{start(0, "t1", ""), args(0, `{"command":"l`)}, []string{`{}`}, map[string]string{"t1": `{"command":"l`}},
		{"arguments in the start", []ir.Event{start(0, "t1", `{"command":"ls"}`), stop(0)}, []string{`{"command":"ls"}`}, nil},
		{"deltas after an empty start", []ir.Event{start(0, "t1", `{}`), args(0, `{"command":"ls"}`), stop(0)}, []string{`{"command":"ls"}`}, nil},
		{"broken start", []ir.Event{start(0, "t1", `{"command`), stop(0)}, []string{`{}`}, map[string]string{"t1": `{"command`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var a Accumulator
			for _, ev := range append(append([]ir.Event{{Type: ir.EventMessageStart, ID: "msg_1"}}, c.events...), end...) {
				if err := a.Add(ev); err != nil {
					t.Fatalf("%s %d: %v", ev.Type, ev.Index, err)
				}
			}
			r := a.Response()
			var got []string
			for _, b := range r.Blocks {
				got = append(got, string(b.ToolUse.Args))
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("args %q, want %q", got, c.want)
			}
			if inv := a.InvalidArgs(); fmt.Sprint(inv) != fmt.Sprint(c.invalid) {
				t.Fatalf("invalid %q, want %q", inv, c.invalid)
			}
			msg, _, err := LuxMessage(r)
			if err != nil || len(msg.Blocks) != len(c.want) {
				t.Fatalf("lux message %+v: %v", msg, err)
			}
		})
	}
	// Only arguments reach a block that stopped: text for a stopped text
	// block, and arguments for a block never started, are still refused.
	for name, evs := range map[string][]ir.Event{
		"text after its stop":    {{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}, {Type: ir.EventBlockStop, Index: 0}, {Type: ir.EventTextDelta, Index: 0, Delta: "late"}},
		"arguments for a text":   {{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}, {Type: ir.EventBlockStop, Index: 0}, args(0, `{}`)},
		"arguments for no block": {args(3, `{}`)},
		"a call stopped twice":   {start(0, "t1", ""), stop(0), stop(0)},
	} {
		var a Accumulator
		var err error
		for _, ev := range evs {
			if err = a.Add(ev); err != nil {
				break
			}
		}
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestSpendRefusalsAreNotRetried(t *testing.T) {
	for typ, spent := range map[string]bool{"budget_exhausted": true, "spend_exceeded": true, "rate_limit_error": false} {
		err := fmt.Errorf("wrapped: %w", &HTTPError{Status: 429, Type: typ})
		code, ok := SpendRefused(err)
		if ok != spent || (ok && code != typ) || Retryable(err) == spent {
			t.Errorf("%s: SpendRefused %q %v, Retryable %v", typ, code, ok, Retryable(err))
		}
	}
	if _, ok := SpendRefused(errors.New("plain")); ok {
		t.Fatal("a plain error is a spend refusal")
	}
	core := fmt.Errorf("machine: %w", &SpendError{Core: "cella", Code: "spend_exceeded", Err: errors.New("the sandbox allowance is spent")})
	if code, ok := SpendRefused(core); !ok || code != "spend_exceeded" || Retryable(core) || !strings.Contains(core.Error(), "cella refused for spend (spend_exceeded)") {
		t.Fatalf("a core's refusal: %q %v, %v", code, ok, core)
	}
	if _, ok := SpendRefused(&SpendError{Core: "cella", Code: "quota", Err: errors.New("x")}); ok {
		t.Fatal("a core's refusal for another reason is a spend refusal")
	}
	if !errors.Is(core, errors.Unwrap(errors.Unwrap(core))) {
		t.Fatal("a core's refusal does not unwrap")
	}
}
