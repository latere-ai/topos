// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/prompts"
)

// TestHistoryIsOptIn (spec 084): history is a built-in an agent holds
// only by naming it, in no default set; it changes nothing, so it never
// opens a machine and runs in every mode, it runs beside a step's other
// calls, and it may run again, which only a built-in may claim.
func TestHistoryIsOptIn(t *testing.T) {
	if !slices.Equal(OptIn(), []string{NameWebSearch, NameHistory}) {
		t.Fatalf("opt-in %v", OptIn())
	}
	if slices.ContainsFunc(Builtins(), func(b Tool) bool { return b.Definition().Name == NameHistory }) {
		t.Fatal("history is in the default set")
	}
	tool := History(nil)
	d, p := tool.Definition(), tool.Properties()
	if d.Name != NameHistory || d.Description != prompts.Text(prompts.ToolHistory) {
		t.Fatalf("definition %+v", d)
	}
	if p != (Properties{Parallel: true, Repeatable: true, Effect: EffectNone}) {
		t.Fatalf("properties %+v", p)
	}
	if err := NewRegistry().Add(tool); !errors.Is(err, ErrRepeatable) {
		t.Fatalf("registered as a tool from outside the built-ins: %v", err)
	}
	if err := NewRegistry().AddBuiltin(tool); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryRefusesInvalidInput (spec 084): a read names one of a range,
// a call or a query. The schema refuses a bound below 1, a value of the
// wrong type, an empty string and a field it does not know; the tool
// refuses a call read with anything else, a blank call or query, and a
// range whose start is past its end, each as invalid_input without
// reading, in the registry's words.
func TestHistoryRefusesInvalidInput(t *testing.T) {
	reads := 0
	tool := History(func(context.Context, Call, HistoryRead) (string, error) {
		reads++
		return "read", nil
	})
	reg := NewRegistry()
	if err := reg.AddBuiltin(tool); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"from":0}`, `{"to":-3}`, `{"from":"1"}`, `{"from":1.5}`, `{"query":""}`, `{"tool_use_id":""}`, `{"depth":1}`,
	} {
		if _, r := reg.Validate(NameHistory, json.RawMessage(input)); r == nil || r.Outcome != OutcomeInvalidInput {
			t.Errorf("the schema took %s", input)
		}
	}
	for input, problem := range map[string]string{
		`{"tool_use_id":"toolu_1","from":3}`:    "/tool_use_id: a call is read alone, without from, to or query",
		`{"tool_use_id":"toolu_1","to":3}`:      "/tool_use_id: a call is read alone, without from, to or query",
		`{"tool_use_id":"toolu_1","query":"x"}`: "/tool_use_id: a call is read alone, without from, to or query",
		`{"tool_use_id":"  "}`:                  "/tool_use_id: empty",
		`{"query":" \n "}`:                      "/query: empty",
		`{"query":"port","from":9,"to":3}`:      "/from: 9 is past to, 3",
		`{"from":200,"to":100}`:                 "/from: 200 is past to, 100",
		`{"from":-1}`:                           "/from: expected integer, got number -1",
		`{"tool_use_id":["toolu_1","toolu_2"]}`: "/tool_use_id: expected string, got array",
	} {
		if _, r := reg.Validate(NameHistory, json.RawMessage(input)); r != nil && !strings.Contains(input, "-1") && !strings.Contains(input, "[") {
			t.Errorf("the schema refused %s: %+v", input, r)
		}
		res, err := tool.Run(t.Context(), Call{ID: "toolu_h", Input: json.RawMessage(input)})
		want := prompts.Render(prompts.RegistryInvalidInput, prompts.Data{"Tool": NameHistory, "Problems": problem})
		if err != nil || res.Outcome != OutcomeInvalidInput || res.Content[0].Text != want {
			t.Errorf("%s: %+v, %v; want %q", input, res, err, want)
		}
	}
	if reads != 0 {
		t.Fatalf("an invalid read was read %d times", reads)
	}
}

// TestHistoryHandsTheReadToItsReader: a read the tool takes reaches its
// reader with its call and blank space trimmed, the answer is the
// reader's text whole, never capped, and a reader's failure is the
// harness's.
func TestHistoryHandsTheReadToItsReader(t *testing.T) {
	var got []HistoryRead
	long := strings.Repeat("x", 3*DefaultOutputLimit)
	tool := History(func(_ context.Context, c Call, r HistoryRead) (string, error) {
		if c.ID != "toolu_h" {
			t.Errorf("the call %q", c.ID)
		}
		got = append(got, r)
		return long, nil
	})
	for _, input := range []string{``, `{}`, `{"from":3}`, `{"from":3,"to":3}`, `{"to":9}`, `{"tool_use_id":" toolu_1 "}`, `{"query":" ECONNREFUSED ","from":2}`} {
		res, err := tool.Run(t.Context(), Call{ID: "toolu_h", Input: json.RawMessage(input)})
		if err != nil || res.Outcome != OutcomeOK || res.Content[0].Text != long || res.Spill != nil {
			t.Fatalf("%s: %v, %v", input, res.Outcome, err)
		}
	}
	want := []HistoryRead{{}, {}, {From: 3}, {From: 3, To: 3}, {To: 9}, {ToolUseID: "toolu_1"}, {Query: "ECONNREFUSED", From: 2}}
	if !slices.Equal(got, want) {
		t.Fatalf("reads %+v", got)
	}
	failing := History(func(context.Context, Call, HistoryRead) (string, error) { return "", errors.New("the log is gone") })
	if _, err := failing.Run(t.Context(), Call{ID: "toolu_h", Input: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a reader's failure became a result")
	}
	b, err := json.Marshal(HistoryRead{From: 181, To: 400})
	if err != nil || string(b) != `{"from":181,"to":400}` {
		t.Fatalf("the input that reads on: %s, %v", b, err)
	}
}
