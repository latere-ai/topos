// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package tools is the tool contract of spec 008 and the registry the
// harness validates calls against. Tools act only through the session's
// machine, so the package dials nothing.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// Definition is what the model sees of a tool.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Effect is what a tool can change; it feeds the risk features of spec
// 012.
type Effect string

// Effects.
const (
	EffectNone     Effect = "none"
	EffectRead     Effect = "read"
	EffectWrite    Effect = "write"
	EffectExternal Effect = "external"
)

// Properties say how the harness schedules and scores a tool.
type Properties struct {
	// Parallel lets the tool run beside other parallel calls of a step.
	Parallel bool
	// Repeatable marks a built-in a resumed runner may run again.
	Repeatable bool
	Effect     Effect
	// Client tools are executed by the client, not the runner.
	Client bool
	// OutputLimit caps the result text in bytes; zero is DefaultOutputLimit.
	OutputLimit int
}

// Tool is one tool.
type Tool interface {
	Definition() Definition
	Properties() Properties
	Run(ctx context.Context, c Call) (Result, error)
}

// Call is one call of a tool.
type Call struct {
	ID      string
	Input   json.RawMessage
	Machine machine.Machine
	// State is the thread's tool state as its log records it.
	State State
}

// Outcomes of a call (spec 008).
const (
	OutcomeOK            = "ok"
	OutcomeError         = "error"
	OutcomeTimeout       = "timeout"
	OutcomeUnknownTool   = "unknown_tool"
	OutcomeInvalidInput  = "invalid_input"
	OutcomeDenied        = "denied"
	OutcomeBlocked       = "blocked"
	OutcomeCanceled      = "canceled"
	OutcomeUnknownEffect = "unknown_effect"
)

// Result is a finished call.
type Result struct {
	Content []lux.Block
	Outcome string
	// Meta is recorded on the tool.result for later calls of the thread.
	Meta  *Meta
	Spill *session.Spill
}

// IsError reports whether the outcome is one the model sees as an error.
func (r Result) IsError() bool { return r.Outcome != OutcomeOK && r.Outcome != "" }

// Text returns a result of one text block.
func Text(outcome, text string) Result {
	return Result{Outcome: outcome, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}
}

// Meta is a tool's record for later calls of its thread.
type Meta struct {
	// Path and SHA256 are a file a file tool read or wrote, and the hash
	// of its content then. An empty SHA256 is a file that does not exist.
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Dir is the directory bash ended in.
	Dir      string `json:"dir,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// Todos is the thread's todo list after a todo call. omitzero keeps
	// an emptied list, [], apart from a meta that has no list.
	Todos []Todo `json:"todos,omitzero"`
}

// Todo is one entry of a thread's list.
type Todo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// State is what a thread's tools know from its log.
type State struct {
	// Hashes is the last recorded hash of each path, "" for a path last
	// seen absent.
	Hashes map[string]string
	// Dir is bash's persistent directory; empty is the working directory.
	Dir string
	// DirSeq is the sequence of the result that reported Dir, which
	// tells a fork's directory from the one its copied log names, a
	// directory of its parent's machine.
	DirSeq uint64
	Todos  []Todo
}

// StateOf folds the tool.result metas of one thread, in sequence order.
func StateOf(events []session.Event, thread string) State {
	st := State{Hashes: map[string]string{}}
	for _, e := range events {
		if e.Type != session.TypeToolResult || e.Thread != thread || e.Redacted() {
			continue
		}
		var p session.ToolResult
		if e.Decode(&p) != nil || len(p.Meta) == 0 {
			continue
		}
		var m Meta
		if json.Unmarshal(p.Meta, &m) != nil {
			continue
		}
		if m.Path != "" {
			st.Hashes[m.Path] = m.SHA256
		}
		if m.Dir != "" {
			st.Dir, st.DirSeq = m.Dir, e.Seq
		}
		if m.Todos != nil {
			st.Todos = m.Todos
		}
	}
	return st
}

// Output caps of spec 008.
const (
	DefaultOutputLimit = 32 << 10
	spillHead          = 20 << 10
	spillTail          = 10 << 10
)

// Cap keeps text within limit. Past it, the whole text goes to a spill
// file in the machine's spill directory, and the result carries its head,
// a line naming the file, and its tail.
func Cap(ctx context.Context, m machine.Machine, id, text string, limit int) (string, *session.Spill, error) {
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	if len(text) <= limit {
		return text, nil, nil
	}
	// A machine opened on demand has a spill directory once it is open.
	if err := machine.Open(ctx, m); err != nil {
		return "", nil, fmt.Errorf("tools: open the machine for the spill file: %w", err)
	}
	p := path.Join(m.SpillDir(), "tool-"+spillName(id)+".txt")
	if err := m.WriteFile(ctx, p, strings.NewReader(text), 0o600); err != nil {
		return "", nil, fmt.Errorf("tools: write the spill file: %w", err)
	}
	// The cut points move to character boundaries, so neither part ends
	// or starts inside a UTF-8 sequence.
	head := min(spillHead, limit*5/8)
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	from := len(text) - min(spillTail, limit*5/16)
	for from < len(text) && !utf8.RuneStart(text[from]) {
		from++
	}
	omitted := from - head
	line := prompts.Render(prompts.OutputSpilled, prompts.Data{"Omitted": omitted, "Path": p})
	capped := text[:head] + "\n" + line + "\n" + text[from:]
	return capped, &session.Spill{Path: p, Bytes: int64(len(text))}, nil
}

// spillName makes a tool_use id safe as a file name: a character other
// than a letter, a digit, _ or - becomes _, so an id cannot name a path
// outside the spill directory.
func spillName(id string) string {
	b := []byte(id)
	for i, c := range b {
		if !nameChar(rune(c)) {
			b[i] = '_'
		}
	}
	if len(b) == 0 {
		return "call"
	}
	return string(b)
}

// Registry is the tools of one thread, in the order the model sees them.
type Registry struct {
	tools   []Tool
	byName  map[string]int
	schemas map[string]*Schema
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]int{}, schemas: map[string]*Schema{}}
}

// ErrRepeatable is a tool that is not built in and claims Repeatable.
var ErrRepeatable = errors.New("tools: only a built-in tool may be repeatable")

// Add registers a tool from outside the built-in set: an MCP server's,
// an embedder's, or a client tool. It may not be repeatable.
func (r *Registry) Add(t Tool) error {
	if t.Properties().Repeatable {
		return fmt.Errorf("%w: %s", ErrRepeatable, t.Definition().Name)
	}
	return r.add(t)
}

// AddBuiltin registers a built-in tool.
func (r *Registry) AddBuiltin(t Tool) error { return r.add(t) }

var namePattern = func(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, c := range n {
		if !nameChar(c) {
			return false
		}
	}
	return true
}

// nameChar reports whether c may appear in a tool name: a letter, a
// digit, _ or -.
func nameChar(c rune) bool {
	return c == '_' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (r *Registry) add(t Tool) error {
	d := t.Definition()
	if !namePattern(d.Name) {
		return fmt.Errorf("tools: %q is not a tool name: letters, digits, _ and -, at most 64", d.Name)
	}
	if _, ok := r.byName[d.Name]; ok {
		return fmt.Errorf("tools: %s is registered twice", d.Name)
	}
	s, err := CompileSchema(d.InputSchema)
	if err != nil {
		return fmt.Errorf("tools: the schema of %s: %w", d.Name, err)
	}
	r.byName[d.Name] = len(r.tools)
	r.tools = append(r.tools, t)
	r.schemas[d.Name] = s
	return nil
}

// Subset returns a registry of the named tools, in this registry's
// order; names it does not hold are ignored. It is a thread's registry
// after the narrowing of spec 013.
func (r *Registry) Subset(names []string) *Registry {
	out := NewRegistry()
	for _, t := range r.tools {
		n := t.Definition().Name
		if !slices.Contains(names, n) {
			continue
		}
		out.byName[n] = len(out.tools)
		out.tools = append(out.tools, t)
		out.schemas[n] = r.schemas[n]
	}
	return out
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	i, ok := r.byName[name]
	if !ok {
		return nil, false
	}
	return r.tools[i], true
}

// Names are the registered names in order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.tools))
	for i, t := range r.tools {
		out[i] = t.Definition().Name
	}
	return out
}

// Definitions are the definitions the model sees, in order.
func (r *Registry) Definitions() []Definition {
	out := make([]Definition, len(r.tools))
	for i, t := range r.tools {
		out[i] = t.Definition()
	}
	return out
}

// Validate checks a call before it is scored (spec 005): a known name and
// an input that matches the tool's schema. A failing call gets the
// result the model sees instead of running.
func (r *Registry) Validate(name string, input json.RawMessage) (Tool, *Result) {
	t, ok := r.Get(name)
	if !ok {
		res := Text(OutcomeUnknownTool, prompts.Render(prompts.RegistryUnknownTool, prompts.Data{"Name": name, "Available": strings.Join(r.Names(), ", ")}))
		return nil, &res
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if len(bytes.TrimSpace(input)) == 0 {
		v = map[string]any{}
	} else if err := dec.Decode(&v); err != nil || !atEOF(dec) {
		res := Text(OutcomeInvalidInput, invalidJSON(name, input, err))
		return nil, &res
	}
	if _, isObj := v.(map[string]any); !isObj {
		res := Text(OutcomeInvalidInput, invalidInput(name, "/: the input is not an object"))
		return nil, &res
	}
	if probs := r.schemas[name].Validate(v); len(probs) > 0 {
		res := Text(OutcomeInvalidInput, invalidInput(name, strings.Join(probs[:min(len(probs), 5)], "\n")))
		return nil, &res
	}
	return t, nil
}

// invalidInput is the result text of an input that does not match the
// schema of tool: problems are the validator's lines, one per problem.
func invalidInput(tool, problems string) string {
	return prompts.Render(prompts.RegistryInvalidInput, prompts.Data{"Tool": tool, "Problems": problems})
}

// InvalidJSONExcerpt is how many characters of arguments that are not
// valid JSON the call's result quotes: enough for the model to see where
// its text went wrong, bounded because such text can run to the model's
// whole output limit.
const InvalidJSONExcerpt = 200

// invalidJSON is the result text of a call whose arguments are not one
// JSON value: what is wrong, and the start of the text the model sent,
// so the model can send the call again. err is the decoder's error, nil
// when the text holds more after its first value.
func invalidJSON(tool string, input []byte, err error) string {
	problem := "there is more text after the first JSON value"
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		problem = "the text ends before the JSON value does"
	case err != nil:
		problem = err.Error()
	}
	return prompts.Render(prompts.RegistryInvalidJSON, prompts.Data{"Tool": tool, "Problem": problem, "Excerpt": excerpt(input)})
}

// CutInput is the answer to a call whose arguments the response's output
// limit, limit tokens, cut off before they ended: the call did not run,
// and the model is asked for shorter arguments, since more room only
// lets a call that runs away inside an argument run further.
func CutInput(tool string, input []byte, limit int64) Result {
	return Text(OutcomeInvalidInput, prompts.Render(prompts.RegistryCutInput, prompts.Data{"Tool": tool, "Limit": limit, "Excerpt": excerpt(input)}))
}

// excerpt is the start of a call's arguments a result quotes, at most
// InvalidJSONExcerpt characters.
func excerpt(input []byte) string {
	s := string(input)
	if utf8.RuneCountInString(s) > InvalidJSONExcerpt {
		s = string([]rune(s)[:InvalidJSONExcerpt])
	}
	return s
}

// atEOF reports whether dec has nothing left but whitespace, so an input
// with data after its object is refused.
func atEOF(dec *json.Decoder) bool {
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// sortedKeys is the keys of m in order, for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
