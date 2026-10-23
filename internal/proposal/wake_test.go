// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// wakeNow is the clock of the wake tests.
var wakeNow = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// newWake is the wake tool over f in a session whose life ends in a week.
func newWake(f *fakeTriggers) *Wake {
	return NewWake(f, wakeNow.Add(7*24*time.Hour), func() time.Time { return wakeNow })
}

// TestWakeBounds is Topos spec 086's bounds criterion at the tool: a set
// sooner than MinWakeDelay, later than MaxWakeAhead or past the session's
// end is answered invalid_input with the bound named and asks nobody; a
// set inside them runs, records the wake with its place in the chain,
// and names the window it was read in; a cancel records the clear, or
// nothing when none was pending.
func TestWakeBounds(t *testing.T) {
	f := &fakeTriggers{}
	w := newWake(f)
	set := func(at string) json.RawMessage {
		return json.RawMessage(`{"action":"set","at":"` + at + `","title":"Flight prices","note":"Look at the fare again."}`)
	}
	cases := map[string]struct {
		input json.RawMessage
		want  string
	}{
		"too soon":            {set(wakeNow.Add(trigger.MinWakeDelay - time.Minute).Format(time.RFC3339)), "(too_soon). The earliest a wake may be is " + wakeNow.Add(trigger.MinWakeDelay).Format(time.RFC3339)},
		"too far ahead":       {json.RawMessage(`{"action":"set","after":"200h","title":"t","note":"n"}`), "(too_late). The latest a wake may be is " + wakeNow.Add(trigger.MaxWakeAhead).Format(time.RFC3339)},
		"after the session":   {set(wakeNow.Add(8 * 24 * time.Hour).Format(time.RFC3339)), "(too_late)"},
		"no offset":           {set("2026-10-23T12:00:00"), "not an RFC 3339 time with an offset"},
		"seconds":             {json.RawMessage(`{"action":"set","after":"90s","title":"t","note":"n"}`), "in hours and minutes"},
		"both at and after":   {json.RawMessage(`{"action":"set","at":"2026-10-23T12:00:00Z","after":"3h","title":"t","note":"n"}`), "at or after, one of them"},
		"neither":             {json.RawMessage(`{"action":"set","title":"t","note":"n"}`), "at or after, one of them"},
		"no title":            {json.RawMessage(`{"action":"set","after":"3h","note":"n"}`), "title is required"},
		"a long title":        {json.RawMessage(`{"action":"set","after":"3h","note":"n","title":"` + strings.Repeat("x", trigger.MaxWakeTitle+1) + `"}`), "title is longer than"},
		"a title of 2 lines":  {json.RawMessage(`{"action":"set","after":"3h","note":"n","title":"a\nb"}`), "title is one line"},
		"no note":             {json.RawMessage(`{"action":"set","after":"3h","title":"t"}`), "note is required"},
		"a long note":         {json.RawMessage(`{"action":"set","after":"3h","title":"t","note":"` + strings.Repeat("x", trigger.MaxWakeNote+1) + `"}`), "note is longer than"},
		"another action":      {json.RawMessage(`{"action":"snooze"}`), "it is set or cancel"},
		"an unknown field":    {json.RawMessage(`{"action":"set","in":"3h"}`), "unknown field"},
		"an input not object": {json.RawMessage(`[]`), "cannot unmarshal"},
	}
	for name, c := range cases {
		r := w.Check(tools.Call{ID: "toolu_1", Input: c.input})
		if r == nil || r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, c.want) {
			t.Errorf("%s: %+v, want invalid_input naming %q", name, r, c.want)
		}
		if res, err := w.Run(t.Context(), tools.Call{ID: "toolu_1", Input: c.input}); err != nil || res.Outcome != tools.OutcomeInvalidInput {
			t.Errorf("%s ran: %+v, %v", name, res, err)
		}
	}
	if len(f.wakes) != 0 {
		t.Fatalf("a refused set reached the server: %+v", f.wakes)
	}
	for _, in := range []string{string(set(wakeNow.Add(trigger.MinWakeDelay).Format(time.RFC3339))), `{"action":"set","after":"36h","title":"t","note":"n"}`, `{"action":"cancel"}`} {
		if r := w.Check(tools.Call{ID: "toolu_1", Input: json.RawMessage(in)}); r != nil {
			t.Errorf("%s was refused: %+v", in, r)
		}
	}

	// A set inside the bounds, the third in a row.
	res, err := w.Run(t.Context(), tools.Call{ID: "toolu_s", Input: json.RawMessage(`{"action":"set","after":"90m","title":" Flight prices ","note":"Look at the fare again."}`), State: tools.State{Wakes: 2}})
	at := wakeNow.Add(90 * time.Minute)
	if err != nil || res.Outcome != tools.OutcomeOK || res.WakeSet == nil || *res.WakeSet != (session.WakeSet{TriggerID: "trg_w", At: at, Title: "Flight prices", Note: "Look at the fare again.", Count: 3}) {
		t.Fatalf("the set answered %+v, %v", res, err)
	}
	if want := "It is wake 3 in a row with no word from the person; after 10, the next set waits for their yes. A wake may be set from " + wakeNow.Add(trigger.MinWakeDelay).Format(time.RFC3339) + " to " + wakeNow.Add(7*24*time.Hour).Format(time.RFC3339) + "."; !strings.Contains(res.Content[0].Text, want) {
		t.Fatalf("the set's result is %q", res.Content[0].Text)
	}
	if len(f.wakes) != 1 || f.wakes[0] != (runner.Wake{ToolUseID: "toolu_s", At: at, Title: "Flight prices", Note: "Look at the fare again."}) {
		t.Fatalf("the server was asked %+v", f.wakes)
	}
	f.refuse = &runner.TriggerRefusal{Code: "wakes_full", Message: "one person holds at most 20"}
	if res, err := w.Run(t.Context(), tools.Call{ID: "toolu_r", Input: json.RawMessage(`{"action":"set","after":"3h","title":"t","note":"n"}`)}); err != nil || res.Outcome != tools.OutcomeError ||
		!strings.Contains(res.Content[0].Text, "wakes_full: one person holds at most 20") || res.WakeSet != nil {
		t.Fatalf("a refused set answered %+v, %v", res, err)
	}
	f.refuse, f.fail = nil, errors.New("the server is away")
	if _, err := w.Run(t.Context(), tools.Call{ID: "toolu_f", Input: json.RawMessage(`{"action":"set","after":"3h","title":"t","note":"n"}`)}); err == nil {
		t.Fatal("a set the server could not take was answered as a result")
	}

	// A cancel: of none, of the pending wake, refused, and failed.
	f.fail = nil
	if res, err := w.Run(t.Context(), tools.Call{ID: "toolu_c", Input: json.RawMessage(`{"action":"cancel"}`)}); err != nil || res.WakeCleared != nil || res.Content[0].Text != prompts.Text(prompts.WakeNone) {
		t.Fatalf("a cancel of none answered %+v, %v", res, err)
	}
	f.pending = "trg_w"
	if res, err := w.Run(t.Context(), tools.Call{ID: "toolu_c", Input: json.RawMessage(`{"action":"cancel"}`)}); err != nil || res.WakeCleared == nil ||
		*res.WakeCleared != (session.WakeCleared{TriggerID: "trg_w", Reason: session.WakeCanceled}) {
		t.Fatalf("a cancel answered %+v, %v", res, err)
	}
	f.refuse = &runner.TriggerRefusal{Code: "forbidden"}
	if res, err := w.Run(t.Context(), tools.Call{ID: "toolu_c", Input: json.RawMessage(`{"action":"cancel"}`)}); err != nil || res.Outcome != tools.OutcomeError {
		t.Fatalf("a refused cancel answered %+v, %v", res, err)
	}
	f.refuse, f.fail = nil, errors.New("away")
	if _, err := w.Run(t.Context(), tools.Call{ID: "toolu_c", Input: json.RawMessage(`{"action":"cancel"}`)}); err == nil {
		t.Fatal("a cancel the server could not take was answered as a result")
	}
	if res, err := w.Run(t.Context(), tools.Call{ID: "toolu_x", Input: json.RawMessage(`{`)}); err != nil || res.Outcome != tools.OutcomeInvalidInput {
		t.Fatalf("a call that does not read ran: %+v, %v", res, err)
	}
}

// TestTheEleventhWakeWaitsForThePerson: a set past MaxWakesWithoutPerson
// wakes in a row is held with the chain named, and no earlier set, no
// cancel and no call that does not read is.
func TestTheEleventhWakeWaitsForThePerson(t *testing.T) {
	w := newWake(&fakeTriggers{})
	set := json.RawMessage(`{"action":"set","after":"3h","title":"t","note":"n"}`)
	if r := w.Confirm(tools.Call{Input: set, State: tools.State{Wakes: trigger.MaxWakesWithoutPerson - 1}}); r != "" {
		t.Fatalf("the tenth wake is held %q", r)
	}
	r := w.Confirm(tools.Call{Input: set, State: tools.State{Wakes: trigger.MaxWakesWithoutPerson}})
	if r != "sets wake 11 in a row with no word from the person, past 10, so the person decides whether the agent keeps looking" {
		t.Fatalf("the eleventh wake is held %q", r)
	}
	for _, in := range []string{`{"action":"cancel"}`, `{`} {
		if r := w.Confirm(tools.Call{Input: json.RawMessage(in), State: tools.State{Wakes: 20}}); r != "" {
			t.Errorf("%s is held %q", in, r)
		}
	}
	if p := w.Properties(); p.Effect != tools.EffectWrite || p.Repeatable || p.Parallel {
		t.Fatalf("the properties are %+v", p)
	}
}

// TestWakeTextsReadTheirBounds: the wake tool's description states the
// bounds its schema and checks hold a call to, and its schema holds them.
func TestWakeTextsReadTheirBounds(t *testing.T) {
	d := newWake(nil).Definition()
	text := prompts.Text(prompts.ToolWake)
	if d.Description != text || d.Name != WakeName {
		t.Fatalf("the definition is %+v", d)
	}
	for _, bound := range []string{
		"at least " + minutes(trigger.MinWakeDelay),
		"at most " + hours(trigger.MaxWakeAhead) + " ahead",
		"After " + strconv.Itoa(trigger.MaxWakesWithoutPerson) + " wakes in a row",
		"at most " + strconv.Itoa(trigger.MaxWakeTitle) + " characters",
		"at most " + strconv.Itoa(trigger.MaxWakeNote) + " characters",
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
	if s.Properties["title"].MaxLength != trigger.MaxWakeTitle || s.Properties["note"].MaxLength != trigger.MaxWakeNote {
		t.Fatalf("the schema bounds are %+v", s.Properties)
	}
	if NewWake(nil, time.Time{}, nil).now == nil {
		t.Fatal("a wake tool with no clock has none")
	}
}
