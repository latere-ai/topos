// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"latere.ai/x/topos/session"
)

// stub is a tool with a given definition and properties.
type stub struct {
	def   Definition
	props Properties
}

func (s stub) Definition() Definition { return s.def }
func (s stub) Properties() Properties { return s.props }
func (s stub) Run(context.Context, Call) (Result, error) {
	return Text(OutcomeOK, "ran"), nil
}

func TestOnlyBuiltinsAreRepeatable(t *testing.T) {
	r := NewRegistry()
	sync := stub{def: Definition{Name: "memory_sync"}, props: Properties{Repeatable: true}}
	if err := r.Add(sync); !errors.Is(err, ErrRepeatable) {
		t.Fatalf("a repeatable tool from outside the set: %v", err)
	}
	if err := r.AddBuiltin(sync); err != nil {
		t.Fatalf("a repeatable built-in: %v", err)
	}
	if err := r.Add(stub{def: Definition{Name: "mcp_tool"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Names(), []string{"memory_sync", "mcp_tool"}) || len(r.Definitions()) != 2 {
		t.Fatalf("names %v", r.Names())
	}
	if got, ok := r.Get("mcp_tool"); !ok || got.Definition().Name != "mcp_tool" {
		t.Fatal("get")
	}
	if _, ok := r.Get("absent"); ok {
		t.Fatal("got an absent tool")
	}
}

func TestRegistryRefuses(t *testing.T) {
	r := NewRegistry()
	for name, tool := range map[string]Tool{
		"empty name":   stub{def: Definition{Name: ""}},
		"bad name":     stub{def: Definition{Name: "has space"}},
		"long name":    stub{def: Definition{Name: strings.Repeat("a", 65)}},
		"bad schema":   stub{def: Definition{Name: "x", InputSchema: json.RawMessage(`{"type":"object","maxItems":3}`)}},
		"not a schema": stub{def: Definition{Name: "y", InputSchema: json.RawMessage(`[`)}},
	} {
		if err := r.Add(tool); err == nil {
			t.Fatalf("%s: registered", name)
		}
	}
	if err := r.Add(stub{def: Definition{Name: "Tool-1_a"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(stub{def: Definition{Name: "Tool-1_a"}}); err == nil {
		t.Fatal("registered twice")
	}
}

func TestValidate(t *testing.T) {
	r := NewRegistry()
	for _, tool := range Builtins() {
		if err := r.AddBuiltin(tool); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Add(stub{def: Definition{Name: "free"}}); err != nil {
		t.Fatal(err)
	}
	prefix := "The input does not match the schema of read:\n"
	notJSON := func(problem, excerpt string) string {
		return "The arguments of read were not valid JSON (" + problem + "). They began:\n" + excerpt +
			"\nCall read again with its arguments as one JSON object that matches its schema."
	}
	long := `{"path":"` + strings.Repeat("é", 2*InvalidJSONExcerpt)
	for _, c := range []struct {
		name, tool, input, outcome, want string
	}{
		{"unknown", "nope", `{}`, OutcomeUnknownTool, "No tool named nope. Available tools: read, write, edit, bash, grep, glob, web_fetch, todo, free."},
		{"not json", "read", `{"path":`, OutcomeInvalidInput, notJSON("the text ends before the JSON value does", `{"path":`)},
		{"trailing data", "read", `{"path":"a"} {"path":"b"}`, OutcomeInvalidInput, notJSON("there is more text after the first JSON value", `{"path":"a"} {"path":"b"}`)},
		{"trailing brace", "read", `{"path":"a"}}`, OutcomeInvalidInput, notJSON("there is more text after the first JSON value", `{"path":"a"}}`)},
		{"bad token", "read", `{"path":}`, OutcomeInvalidInput, notJSON("invalid character '}' looking for beginning of value", `{"path":}`)},
		// Text past the excerpt is cut on a character boundary.
		{"long", "read", long, OutcomeInvalidInput, notJSON("the text ends before the JSON value does", string([]rune(long)[:InvalidJSONExcerpt]))},
		{"not an object", "read", `["a"]`, OutcomeInvalidInput, prefix + "/: the input is not an object"},
		{"null", "read", `null`, OutcomeInvalidInput, prefix + "/: the input is not an object"},
		{"missing", "read", ``, OutcomeInvalidInput, prefix + `/: missing required property "path"`},
		{"types", "read", `{"path":1,"offset":0,"limit":1.5,"extra":true}`, OutcomeInvalidInput,
			prefix + "/extra: unexpected property\n/limit: expected integer, got number\n/offset: less than the minimum 1\n/path: expected string, got number"},
	} {
		tool, res := r.Validate(c.tool, json.RawMessage(c.input))
		if tool != nil || res == nil || res.Outcome != c.outcome || text(*res) != c.want || !res.IsError() {
			t.Fatalf("%s: %v %+v\n%q", c.name, tool, res, text(*res))
		}
	}
	// At most five problem lines.
	_, res := r.Validate("grep", json.RawMessage(`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7}`))
	if res == nil || strings.Count(text(*res), "\n") != 5 {
		t.Fatalf("five lines: %q", text(*res))
	}
	for _, c := range []struct{ tool, input string }{
		{"read", `{"path":"a.txt","offset":1.0}`},
		{"free", ``},
		{"free", `{"anything":[1,2]}`},
		{"read", " {\"path\":\"a\"} \n"},
	} {
		tool, res := r.Validate(c.tool, json.RawMessage(c.input))
		if tool == nil || res != nil {
			t.Fatalf("%s %s: %+v", c.tool, c.input, res)
		}
	}
}

func TestStateOfFoldsMetas(t *testing.T) {
	ev := func(t *testing.T, thread string, meta any) session.Event {
		t.Helper()
		var raw json.RawMessage
		if meta != nil {
			b, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			raw = b
		}
		e, err := session.NewEvent(session.TypeToolResult, session.ToolResult{ToolUseID: "toolu_x", Meta: raw}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.Thread = thread
		return e
	}
	todos := []Todo{{ID: "1", Content: "c", Status: TodoPending}}
	other, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_x", Name: "read", Input: json.RawMessage(`{}`), Verdict: "allow"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other.Thread = "thr_a"
	redacted := ev(t, "thr_a", Meta{Path: "/w/r", SHA256: "rr"})
	redacted.Payload = json.RawMessage(`{"tombstone":true}`)
	broken := ev(t, "thr_a", nil)
	broken.Payload = json.RawMessage(`{"meta":`)
	badMeta := ev(t, "thr_a", nil)
	badMeta.Payload = json.RawMessage(`{"tool_use_id":"x","meta":[1]}`)
	events := []session.Event{
		ev(t, "thr_a", Meta{Path: "/w/a", SHA256: "a1"}),
		ev(t, "thr_a", Meta{Path: "/w/b"}),
		ev(t, "thr_b", Meta{Path: "/w/a", SHA256: "other thread"}),
		ev(t, "thr_a", Meta{Dir: "/w/sub"}),
		ev(t, "thr_a", Meta{Todos: todos}),
		ev(t, "thr_a", nil),
		other, redacted, broken, badMeta,
		ev(t, "thr_a", Meta{Path: "/w/a", SHA256: "a2", Dir: "/w"}),
	}
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	st := StateOf(events, "thr_a")
	if st.DirSeq != uint64(len(events)) {
		t.Fatalf("the directory came from %d, want the last result", st.DirSeq)
	}
	want := map[string]string{"/w/a": "a2", "/w/b": ""}
	if len(st.Hashes) != len(want) || st.Hashes["/w/a"] != "a2" || st.Hashes["/w/b"] != "" || st.Dir != "/w" || !slices.Equal(st.Todos, todos) {
		t.Fatalf("state %+v", st)
	}
	if _, ok := st.Hashes["/w/b"]; !ok {
		t.Fatal("a path seen absent is not recorded")
	}
	st = StateOf(append(events, ev(t, "thr_a", Meta{Todos: []Todo{}})), "thr_a")
	if st.Todos == nil || len(st.Todos) != 0 {
		t.Fatalf("an emptied list %#v", st.Todos)
	}
	if st := StateOf(nil, "thr_a"); st.Hashes == nil || st.Dir != "" || st.Todos != nil {
		t.Fatalf("an empty log %+v", st)
	}
}

func TestCap(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	if got, spill, err := Cap(ctx, f.h, "id", "short", 0); err != nil || got != "short" || spill != nil {
		t.Fatalf("short %q %v %v", got, spill, err)
	}
	// The cut points of the default limit fall inside three-byte
	// characters: 20 KiB and 10 KiB are not multiples of three.
	full := strings.Repeat("€", 20000)
	got, spill, err := Cap(ctx, f.h, "toolu_../../x", full, 0)
	if err != nil || spill == nil {
		t.Fatalf("spill %v %v", spill, err)
	}
	if !utf8.ValidString(got) {
		t.Fatal("the capped text cuts a character")
	}
	if spill.Path != filepath.Join(f.spill, "tool-toolu_______x.txt") || spill.Bytes != int64(len(full)) || get(t, spill.Path) != full {
		t.Fatalf("the spill file %+v", spill)
	}
	head, tail, ok := strings.Cut(got, "\n[... ")
	if !ok || len(head) > 20<<10 || len(head) < 20<<10-3 || !strings.HasPrefix(full, head) {
		t.Fatalf("head of %d bytes", len(head))
	}
	_, tail, _ = strings.Cut(tail, " ...]\n")
	if len(tail) > 10<<10 || len(tail) < 10<<10-3 || !strings.HasSuffix(full, tail) {
		t.Fatalf("tail of %d bytes", len(tail))
	}
	omitted := len(full) - len(head) - len(tail)
	if !strings.Contains(got, fmt.Sprintf("[... %d bytes omitted; the full output is in %s ...]", omitted, spill.Path)) {
		t.Fatalf("the omission line is wrong: %d omitted", omitted)
	}
	// A small limit scales the head and the tail.
	got, spill, err = Cap(ctx, f.h, "", strings.Repeat("a", 2000), 1000)
	if err != nil || spill == nil || spill.Path != filepath.Join(f.spill, "tool-call.txt") || !strings.HasPrefix(got, strings.Repeat("a", 625)+"\n[") {
		t.Fatalf("a small limit %q %+v %v", got, spill, err)
	}
	if _, _, err := Cap(ctx, faulty{Machine: f.h, writeErr: errors.New("full")}, "id", strings.Repeat("a", 2000), 1000); err == nil {
		t.Fatal("a failed spill write is not an error")
	}
}

func TestResultHelpers(t *testing.T) {
	if (Result{}).IsError() || (Result{Outcome: OutcomeOK}).IsError() || !(Result{Outcome: OutcomeDenied}).IsError() {
		t.Fatal("IsError")
	}
	for _, o := range []string{OutcomeError, OutcomeTimeout, OutcomeUnknownTool, OutcomeInvalidInput, OutcomeDenied, OutcomeBlocked, OutcomeCanceled, OutcomeUnknownEffect} {
		if !(Result{Outcome: o}).IsError() {
			t.Fatalf("%s is not an error", o)
		}
	}
}

func TestSubsetKeepsOrderAndSchemas(t *testing.T) {
	r := NewRegistry()
	for _, tool := range Builtins() {
		if err := r.AddBuiltin(tool); err != nil {
			t.Fatal(err)
		}
	}
	sub := r.Subset([]string{NameGrep, NameRead, "absent"})
	if got := sub.Names(); len(got) != 2 || got[0] != NameRead || got[1] != NameGrep {
		t.Fatalf("subset %v", got)
	}
	if _, bad := sub.Validate(NameRead, []byte(`{"path":5}`)); bad == nil || bad.Outcome != OutcomeInvalidInput {
		t.Fatal("the subset lost its schemas")
	}
	if _, bad := sub.Validate(NameBash, []byte(`{}`)); bad == nil || bad.Outcome != OutcomeUnknownTool {
		t.Fatal("a tool outside the subset validated")
	}
}

// TestCutInputQuotesTheStartOfTheArguments: the answer to a call the
// output limit cut names the limit and quotes at most the excerpt.
func TestCutInputQuotesTheStartOfTheArguments(t *testing.T) {
	long := `{"content":"` + strings.Repeat("é", 2*InvalidJSONExcerpt)
	res := CutInput("write", []byte(long), 8192)
	want := "The response reached its output limit of 8192 tokens inside the arguments of write, so the call did not run. They began:\n" +
		string([]rune(long)[:InvalidJSONExcerpt]) + "\nCall write again with shorter arguments, splitting long content over several calls."
	if res.Outcome != OutcomeInvalidInput || !res.IsError() || text(res) != want {
		t.Fatalf("%+v\n%q", res, text(res))
	}
}
