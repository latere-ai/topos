// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
)

// fakeTriggers is a server's reach that records each question and
// answers as its fields say.
type fakeTriggers struct {
	list    []runner.ListedTrigger
	refuse  *runner.TriggerRefusal
	fail    error
	checked []runner.Proposal
	applied []runner.Proposal
}

func (f *fakeTriggers) ListTriggers(context.Context) ([]runner.ListedTrigger, error) {
	if f.refuse != nil {
		return nil, f.refuse
	}
	return f.list, f.fail
}

func (f *fakeTriggers) CheckProposal(_ context.Context, p runner.Proposal) error {
	f.checked = append(f.checked, p)
	if f.refuse != nil {
		return f.refuse
	}
	return f.fail
}

func (f *fakeTriggers) ApplyProposal(_ context.Context, p runner.Proposal) (runner.ListedTrigger, error) {
	f.applied = append(f.applied, p)
	if f.refuse != nil {
		return runner.ListedTrigger{}, f.refuse
	}
	if f.fail != nil {
		return runner.ListedTrigger{}, f.fail
	}
	next := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	return runner.ListedTrigger{ID: "trg_1", Name: routine.Name("ses_1/" + p.ToolUseID), Title: p.Name, NextFireAt: &next}, nil
}

// weekly is a proposal every routine rule takes.
const weekly = `{"action":"propose","name":"Weekly sign-ups","ask":"Count last week's sign-ups.","why":"Every Monday at 9:00, Berlin time, I count last week's sign-ups.","schedule":"0 9 * * 1","timeZone":"Europe/Berlin"}`

// with is weekly with one field replaced.
func with(t *testing.T, key string, v any) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(weekly), &m); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		delete(m, key)
	} else {
		m[key] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAProposalIsCheckedBeforeAnyoneIsAsked: a proposal the routine's
// rules refuse is answered invalid_input by Check, naming what is wrong,
// and one they take passes, as a list does.
func TestAProposalIsCheckedBeforeAnyoneIsAsked(t *testing.T) {
	tool := New(&fakeTriggers{})
	cases := map[string]struct {
		input json.RawMessage
		want  string
	}{
		"no action":            {json.RawMessage(`{}`), "action"},
		"another action":       {json.RawMessage(`{"action":"delete"}`), "action"},
		"an unknown field":     {json.RawMessage(`{"action":"list","id":"trg_1"}`), "unknown field"},
		"no why":               {with(t, "why", nil), "why is required"},
		"a long why":           {with(t, "why", strings.Repeat("x", trigger.MaxPurposeLength+1)), "why is longer"},
		"no time zone":         {with(t, "timeZone", nil), "timeZone is required"},
		"an unknown time zone": {with(t, "timeZone", "Mars/Olympus"), "no time zone"},
		"every five minutes":   {with(t, "schedule", "*/5 * * * *"), "not one a routine runs on"},
		"weekdays as a range":  {with(t, "schedule", "0 9 * * 1-5"), "not one a routine runs on"},
		"no name":              {with(t, "name", " "), "no title"},
		"a long name":          {with(t, "name", strings.Repeat("x", routine.MaxTitle+1)), "not one short line"},
		"no ask":               {with(t, "ask", ""), "no instruction"},
	}
	for name, c := range cases {
		r := tool.Check(tools.Call{ID: "toolu_1", Input: c.input})
		if r == nil || r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, c.want) {
			t.Errorf("%s: %+v, want invalid_input naming %q", name, r, c.want)
		}
	}
	for _, in := range []string{weekly, `{"action":"list"}`} {
		if r := tool.Check(tools.Call{ID: "toolu_1", Input: json.RawMessage(in)}); r != nil {
			t.Errorf("%s was refused: %+v", in, r)
		}
	}
}

// TestEveryProposalWaitsForItsPerson: Confirm holds every proposal and no
// list, and Probe answers a proposal the server refuses with its reason,
// asks nothing of a list, and leaves a proposal it could not ask about to
// be decided.
func TestEveryProposalWaitsForItsPerson(t *testing.T) {
	f := &fakeTriggers{}
	tool := New(f)
	if r := tool.Confirm(tools.Call{Input: json.RawMessage(weekly)}); r != confirmPropose {
		t.Fatalf("a proposal is held %q", r)
	}
	if r := tool.Confirm(tools.Call{Input: json.RawMessage(`{"action":"list"}`)}); r != "" {
		t.Fatalf("a list is held %q", r)
	}
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_1", Input: json.RawMessage(weekly)}); r != nil {
		t.Fatalf("a proposal the server takes answered %+v", r)
	}
	if len(f.checked) != 1 || f.checked[0].ToolUseID != "toolu_1" || f.checked[0].Schedule != "0 9 * * 1" || f.checked[0].TimeZone != "Europe/Berlin" {
		t.Fatalf("the server was asked %+v", f.checked)
	}
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)}); r != nil || len(f.checked) != 1 {
		t.Fatalf("a list was probed: %+v", r)
	}
	f.fail = errors.New("the server is away")
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_2", Input: json.RawMessage(weekly)}); r != nil {
		t.Fatalf("a proposal the server could not be asked about answered %+v", r)
	}
	f.fail, f.refuse = nil, &runner.TriggerRefusal{Code: "proposals_full", Message: "the person holds as many as they may"}
	r := tool.Probe(t.Context(), tools.Call{ID: "toolu_3", Input: json.RawMessage(weekly)})
	if r == nil || r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, "proposals_full") || !strings.Contains(r.Content[0].Text, "were not asked") {
		t.Fatalf("a refused proposal answered %+v", r)
	}
}

// TestAnAllowedProposalIsApplied: once the person allowed it, a proposal
// is applied and its result names the routine and its next run; a
// refusal then is the call's result, and a server's failure an error.
func TestAnAllowedProposalIsApplied(t *testing.T) {
	f := &fakeTriggers{}
	tool := New(f)
	res, err := tool.Run(t.Context(), tools.Call{ID: "toolu_1", Input: json.RawMessage(weekly)})
	if err != nil || res.Outcome != tools.OutcomeOK || !strings.Contains(res.Content[0].Text, `"Weekly sign-ups" is theirs now`) || !strings.Contains(res.Content[0].Text, "2026-10-26T08:00:00Z") {
		t.Fatalf("the allowed proposal answered %+v, %v", res, err)
	}
	if len(f.applied) != 1 || f.applied[0] != (runner.Proposal{ToolUseID: "toolu_1", Name: "Weekly sign-ups", Ask: "Count last week's sign-ups.",
		Why: "Every Monday at 9:00, Berlin time, I count last week's sign-ups.", Schedule: "0 9 * * 1", TimeZone: "Europe/Berlin"}) {
		t.Fatalf("applied %+v", f.applied)
	}
	f.refuse = &runner.TriggerRefusal{Code: "forbidden", Message: "no allow"}
	if res, err := tool.Run(t.Context(), tools.Call{ID: "toolu_2", Input: json.RawMessage(weekly)}); err != nil || res.Outcome != tools.OutcomeError || !strings.Contains(res.Content[0].Text, "was not made (forbidden: no allow)") {
		t.Fatalf("a refused apply answered %+v, %v", res, err)
	}
	f.refuse, f.fail = nil, errors.New("the server is away")
	if _, err := tool.Run(t.Context(), tools.Call{ID: "toolu_3", Input: json.RawMessage(weekly)}); err == nil {
		t.Fatal("a server's failure was answered as a result")
	}
}

// TestAListNamesWhatThePersonHolds: a list names each trigger's title,
// schedule, next run and whether a conversation offered it, and says so
// when there is none.
func TestAListNamesWhatThePersonHolds(t *testing.T) {
	next := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	f := &fakeTriggers{list: []runner.ListedTrigger{
		{ID: "trg_1", Name: "routine-a", Title: "Weekly sign-ups", Schedule: "0 9 * * 1", TimeZone: "Europe/Berlin", NextFireAt: &next, Origin: &v1.TriggerOrigin{Kind: v1.OriginProposal}},
		{ID: "trg_2", Name: "on-mail", Product: "mail", Suspended: true},
	}}
	res, err := New(f).Run(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := "The person holds these triggers of you, newest first:\n" +
		"- Weekly sign-ups (routine-a): 0 9 * * 1 in Europe/Berlin, next at 2026-10-26T08:00:00Z, offered in a conversation\n" +
		"- on-mail (on-mail): when mail sends an event, paused\n"
	if res.Outcome != tools.OutcomeOK || res.Content[0].Text != want {
		t.Fatalf("the list answered %q, want %q", res.Content[0].Text, want)
	}
	f.list = nil
	if res, err := New(f).Run(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)}); err != nil || res.Content[0].Text != "The person holds no triggers of you." {
		t.Fatalf("an empty list answered %+v, %v", res, err)
	}
}

// TestToolTextsReadTheirBounds: the tool's description states the bounds
// its schema and checks hold a call to, and its schema holds them.
func TestToolTextsReadTheirBounds(t *testing.T) {
	d := New(nil).Definition()
	text := prompts.Text(prompts.ToolTrigger)
	if d.Description != text || d.Name != Name {
		t.Fatalf("the definition is %+v", d)
	}
	for _, bound := range []string{
		"at most " + strconv.Itoa(routine.MaxTitle) + " characters",
		"at most " + strconv.Itoa(trigger.MaxPurposeLength) + " characters",
		"at most " + strconv.Itoa(trigger.MaxListed) + " triggers",
	} {
		if !strings.Contains(text, bound) {
			t.Errorf("the description does not say %q", bound)
		}
	}
	var s struct {
		Properties map[string]struct {
			MaxLength int `json:"maxLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.InputSchema, &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties["name"].MaxLength != routine.MaxTitle || s.Properties["why"].MaxLength != trigger.MaxPurposeLength {
		t.Fatalf("the schema bounds are %+v", s.Properties)
	}
}
