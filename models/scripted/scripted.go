// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package scripted is the scripted model of spec 026: a connection
// "scripted:<path>" plays the YAML script at path, one step per request.
// Its responses go through the Lux wire encoding like a real model's, so
// a scripted session writes a v1 log. It is for tests and dials nothing.
package scripted

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
)

// Codec is the codec a scripted model.request records.
const Codec = "scripted@1"

// Step is one scripted response.
type Step struct {
	Text      string     `yaml:"text"`
	Thinking  string     `yaml:"thinking"`
	ToolCalls []ToolCall `yaml:"tool_calls"`
	Stop      string     `yaml:"stop"`
	Usage     Usage      `yaml:"usage"`
	Fail      *Fail      `yaml:"fail"`
	Expect    *Expect    `yaml:"expect"`
}

// ToolCall is one call a step makes.
type ToolCall struct {
	Name  string         `yaml:"name"`
	Input map[string]any `yaml:"input"`
}

// Usage is a step's token counts.
type Usage struct {
	InputTokens  int64  `yaml:"input_tokens"`
	OutputTokens int64  `yaml:"output_tokens"`
	CostUSDMicro *int64 `yaml:"cost_usd_micro"`
}

// Fail injects a failure before a step's response: an HTTP error the
// given number of times, or a stream cut short.
type Fail struct {
	Status     int    `yaml:"status"`
	RetryAfter int    `yaml:"retry_after"`
	Times      int    `yaml:"times"`
	Cut        bool   `yaml:"cut"`
	Type       string `yaml:"type"`
}

// Expect asserts on the request a step answers.
type Expect struct {
	ToolResultContains string `yaml:"tool_result_contains"`
	SystemContains     string `yaml:"system_contains"`
	Messages           int    `yaml:"messages"`
}

// Script is a list of steps.
type Script struct {
	Steps []Step `yaml:"steps"`
}

// Parse reads a script.
func Parse(b []byte) (Script, error) {
	var s Script
	if err := yaml.Unmarshal(b, &s); err != nil {
		return Script{}, fmt.Errorf("scripted: %w", err)
	}
	for i, st := range s.Steps {
		switch st.Stop {
		case "", "end_turn", "tool_use", "max_tokens", "refusal":
		default:
			return Script{}, fmt.Errorf("scripted: step %d: stop %q is not end_turn, tool_use, max_tokens or refusal", i+1, st.Stop)
		}
		for j, c := range st.ToolCalls {
			if c.Name == "" {
				return Script{}, fmt.Errorf("scripted: step %d call %d has no name", i+1, j+1)
			}
		}
	}
	return s, nil
}

// ErrExhausted is a request after the script's last step.
var ErrExhausted = errors.New("scripted: the script has no step left")

// ExpectationError is a request that failed a step's expect.
type ExpectationError struct {
	Step    int
	Problem string
	Request string
}

func (e *ExpectationError) Error() string {
	return fmt.Sprintf("scripted: step %d: %s\nthe request was:\n%s", e.Step, e.Problem, e.Request)
}

// Model plays scripts. One Model keeps each script's position, so the
// requests of a session advance through its steps.
type Model struct {
	// Load reads a script's bytes; nil reads the file.
	Load func(path string) ([]byte, error)

	mu      sync.Mutex
	scripts map[string]*player
}

type player struct {
	script Script
	next   int
	failed int
	seq    int
}

// Remaining reports how many steps of the script at path are unplayed.
func (m *Model) Remaining(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.scripts[path]
	if !ok {
		return -1
	}
	return len(p.script.Steps) - p.next
}

func (m *Model) player(path string) (*player, error) {
	if m.scripts == nil {
		m.scripts = map[string]*player{}
	}
	if p, ok := m.scripts[path]; ok {
		return p, nil
	}
	load := m.Load
	if load == nil {
		load = os.ReadFile
	}
	b, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("scripted: read %s: %w", path, err)
	}
	s, err := Parse(b)
	if err != nil {
		return nil, err
	}
	p := &player{script: s}
	m.scripts[path] = p
	return p, nil
}

// Stream plays the next step of the connection's script.
func (m *Model) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	path, ok := strings.CutPrefix(req.Connection.BaseURL, models.SchemeScripted+":")
	if !ok {
		return nil, fmt.Errorf("scripted: %q is not a scripted connection", req.Connection.BaseURL)
	}
	body, err := json.Marshal(req.IR)
	if err != nil {
		return nil, fmt.Errorf("scripted: encode the request: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.player(path)
	if err != nil {
		return nil, err
	}
	if p.next >= len(p.script.Steps) {
		return nil, ErrExhausted
	}
	st := p.script.Steps[p.next]
	n := p.next + 1
	if f := st.Fail; f != nil && !f.Cut && p.failed < max(f.Times, 1) {
		p.failed++
		return nil, &models.HTTPError{Status: f.Status, Type: f.Type, RetryAfter: time.Duration(f.RetryAfter) * time.Second}
	}
	if err := check(st.Expect, &req.IR, n); err != nil {
		return nil, err
	}
	cut := st.Fail != nil && st.Fail.Cut && p.failed < max(st.Fail.Times, 1)
	if cut {
		p.failed++
	} else {
		p.next++
		p.failed = 0
	}
	p.seq++
	resp := response(st, p.seq, req.Connection.Model)
	sum := sha256.Sum256(body)
	res := models.Result{RequestSHA256: hex.EncodeToString(sum[:]), RequestSize: int64(len(body)), Codec: Codec}
	if req.Capture {
		res.RequestBytes = body
	}
	return &stream{events: events(resp), resp: resp, res: res, cut: cut}, nil
}

// check applies a step's expect to the request.
func check(e *Expect, r *ir.Request, step int) error {
	if e == nil {
		return nil
	}
	fail := func(problem string) error {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return &ExpectationError{Step: step, Problem: problem}
		}
		return &ExpectationError{Step: step, Problem: problem, Request: string(b)}
	}
	if e.Messages > 0 && len(r.Messages) != e.Messages {
		return fail(fmt.Sprintf("want %d messages, got %d", e.Messages, len(r.Messages)))
	}
	if e.SystemContains != "" {
		found := false
		for _, b := range r.System {
			found = found || strings.Contains(b.Text, e.SystemContains)
		}
		if !found {
			return fail(fmt.Sprintf("no system part contains %q", e.SystemContains))
		}
	}
	if e.ToolResultContains != "" {
		found := false
		for _, m := range r.Messages {
			for _, b := range m.Blocks {
				if b.ToolResult == nil {
					continue
				}
				for _, in := range b.ToolResult.Blocks {
					found = found || strings.Contains(in.Text, e.ToolResultContains)
				}
			}
		}
		if !found {
			return fail(fmt.Sprintf("no tool result contains %q", e.ToolResultContains))
		}
	}
	return nil
}

// response builds a step's IR response; tool call ids are
// call_<request>_<n>.
func response(st Step, seq int, model string) ir.Response {
	r := ir.Response{ID: fmt.Sprintf("msg_scripted_%d", seq), Model: model}
	if st.Thinking != "" {
		r.Blocks = append(r.Blocks, ir.Block{Type: ir.BlockThinking, Text: st.Thinking, Signature: "scripted"})
	}
	if st.Text != "" {
		r.Blocks = append(r.Blocks, ir.Block{Type: ir.BlockText, Text: st.Text})
	}
	for i, c := range st.ToolCalls {
		args, err := json.Marshal(c.Input)
		if err != nil || c.Input == nil {
			args = []byte(`{}`)
		}
		r.Blocks = append(r.Blocks, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: fmt.Sprintf("call_%d_%d", seq, i+1), Name: c.Name, Args: args}})
	}
	switch {
	case st.Stop != "":
		r.StopReason = ir.StopReason(st.Stop)
	case len(st.ToolCalls) > 0:
		r.StopReason = ir.StopToolUse
	default:
		r.StopReason = ir.StopEndTurn
	}
	r.Usage = ir.Usage{InputTokens: st.Usage.InputTokens, OutputTokens: st.Usage.OutputTokens, CostUSDMicro: st.Usage.CostUSDMicro}
	return r
}

// events is the IR stream of a whole response.
func events(r ir.Response) []ir.Event {
	start := ir.Usage{InputTokens: r.Usage.InputTokens}
	evs := []ir.Event{{Type: ir.EventMessageStart, ID: r.ID, Model: r.Model, Usage: &start}}
	for i, b := range r.Blocks {
		head := ir.Block{Type: b.Type}
		if b.ToolUse != nil {
			head.ToolUse = &ir.ToolUse{ID: b.ToolUse.ID, Name: b.ToolUse.Name}
		}
		evs = append(evs, ir.Event{Type: ir.EventBlockStart, Index: i, Block: &head})
		switch b.Type {
		case ir.BlockText:
			evs = append(evs, ir.Event{Type: ir.EventTextDelta, Index: i, Delta: b.Text})
		case ir.BlockThinking:
			evs = append(evs, ir.Event{Type: ir.EventThinkingDelta, Index: i, Delta: b.Text}, ir.Event{Type: ir.EventSignatureDelta, Index: i, Delta: b.Signature})
		case ir.BlockToolUse:
			evs = append(evs, ir.Event{Type: ir.EventArgsDelta, Index: i, Delta: string(b.ToolUse.Args)})
		}
		evs = append(evs, ir.Event{Type: ir.EventBlockStop, Index: i})
	}
	u := r.Usage
	return append(evs, ir.Event{Type: ir.EventMessageDelta, StopReason: r.StopReason, Usage: &u}, ir.Event{Type: ir.EventMessageStop})
}

type stream struct {
	events []ir.Event
	i      int
	resp   ir.Response
	res    models.Result
	cut    bool
	acc    models.Accumulator
	done   bool
}

func (s *stream) Next() (ir.Event, error) {
	if s.done {
		return ir.Event{}, io.EOF
	}
	if s.cut && s.i >= len(s.events)-2 {
		return ir.Event{}, models.ErrIncomplete
	}
	if s.i == len(s.events) {
		msg, usage, err := models.LuxMessage(s.acc.Response())
		if err != nil {
			return ir.Event{}, err
		}
		raw, err := json.Marshal(lux.Response{ID: s.resp.ID, Model: s.resp.Model, Blocks: msg.Blocks, StopReason: s.resp.StopReason, Usage: usage})
		if err != nil {
			return ir.Event{}, fmt.Errorf("scripted: encode the response: %w", err)
		}
		s.res.Message, s.res.Usage, s.res.StopReason, s.res.RawResponse = msg, usage, s.resp.StopReason, raw
		s.done = true
		return ir.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	if err := s.acc.Add(ev); err != nil {
		return ir.Event{}, err
	}
	return ev, nil
}

func (s *stream) Result() models.Result { return s.res }
func (s *stream) Close() error          { return nil }

var _ models.Model = (*Model)(nil)
