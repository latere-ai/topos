// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// values are the three roots as a firing of an event fills them.
func values(t *testing.T, payload string) Values {
	t.Helper()
	e := Envelope{ID: "d-1", Product: "github", Verb: "issue.opened", Resource: "o/r#7", Actor: "ann", Subject: "o",
		Time: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC), Payload: json.RawMessage(payload)}
	event, err := e.Value()
	if err != nil {
		t.Fatal(err)
	}
	return Values{
		RootEvent:   event,
		RootTrigger: map[string]any{"id": "trg_1", "name": "triage"},
		RootFiring:  map[string]any{"id": "frg_1", "time": "2026-09-30T08:00:01Z"},
	}
}

// TestTemplate: a template renders each root, a string as itself, a
// number and a boolean as JSON, an object and a list as compact JSON,
// and a missing path or null as nothing; a value past MaxValueBytes is
// cut at a character boundary and marked, and \{{ is a literal.
func TestTemplate(t *testing.T) {
	v := values(t, `{"title":"Crash <on> start","n":42,"f":1.5e3,"ok":true,"none":null,"labels":["bug","p1"],"user":{"login":"ann"}}`)
	cases := map[string]string{
		"{{event.id}} {{event.product}} {{event.verb}} {{event.resource}}":   "d-1 github issue.opened o/r#7",
		"{{ event.actor }} of {{event.subject}} at {{event.time}}":           "ann of o at 2026-09-30T08:00:00Z",
		"{{trigger.id}}/{{trigger.name}}/{{firing.id}}/{{firing.time}}":      "trg_1/triage/frg_1/2026-09-30T08:00:01Z",
		"{{event.payload.title}}":                                            "Crash <on> start",
		"{{event.payload.n}} {{event.payload.f}} {{event.payload.ok}}":       "42 1.5e3 true",
		"[{{event.payload.none}}][{{event.payload.missing}}][{{trigger.x}}]": "[][][]",
		"{{event.payload.labels}} {{event.payload.labels.1}}":                `["bug","p1"] p1`,
		"{{event.payload.labels.9}}{{event.payload.labels.01}}":              "",
		"{{event.payload.user}}":                                             `{"login":"ann"}`,
		`\{{event.id}} stays`:                                                "{{event.id}} stays",
		"no placeholder }} here":                                             "no placeholder }} here",
	}
	for in, want := range cases {
		tpl, err := Parse(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := tpl.Render(v); got != want {
			t.Errorf("%q renders %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("a", MaxValueBytes-1) + "é" + "tail"
	b, err := json.Marshal(map[string]string{"body": long})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := Parse("<{{event.payload.body}}>")
	if err != nil {
		t.Fatal(err)
	}
	got := tpl.Render(values(t, string(b)))
	if want := "<" + strings.Repeat("a", MaxValueBytes-1) + CutMark + ">"; got != want {
		t.Fatalf("a long value is cut to %d bytes, not at the character it splits: got %d bytes", MaxValueBytes-1, len(got))
	}
	if short := Cut("é"); short != "é" {
		t.Fatalf("a short value is cut: %q", short)
	}

	if tpl, err := Parse("plain"); err != nil || !tpl.Literal() || tpl.Uses(RootEvent) {
		t.Fatalf("a literal template: %v", err)
	}
	if tpl, err := Parse("{{firing.id}}"); err != nil || tpl.Literal() || !tpl.Uses(RootFiring) || tpl.Uses(RootEvent) {
		t.Fatalf("a template of the firing root: %v", err)
	}

	for in, want := range map[string]string{
		"{{env.HOME}}":    "unknown root",
		"a {{event.id":    "has no }}",
		"{{event..id}}":   "opens no placeholder",
		"{{ }}":           "opens no placeholder",
		"{{event.id!}}":   "opens no placeholder",
		"{{event}}":       "names its root alone",
		"{{ if event }}x": "opens no placeholder",
	} {
		_, err := Parse(in)
		var te *Error
		if !errors.As(err, &te) || !strings.Contains(te.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
	if _, err := Parse("ok {{event.id}} then {{bad"); err == nil || !strings.HasPrefix(err.Error(), "byte 21:") {
		t.Fatalf("the offset names where the unopened {{ is: %v", err)
	}
}
