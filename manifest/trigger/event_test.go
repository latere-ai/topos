// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
)

func TestTheFilterMatchesEachField(t *testing.T) {
	e := Envelope{ID: "1", Product: "github", Verb: "pull_request.opened", Resource: "o/r#9", Time: time.Now(),
		Payload: json.RawMessage(`{"action":"opened","pull_request":{"base":{"ref":"main"},"draft":false,"number":9,"labels":[]}}`)}
	cases := []struct {
		name string
		on   v1.TriggerOn
		want bool
	}{
		{"product and verb", v1.TriggerOn{Product: "github", Verbs: []string{"pull_request.opened"}}, true},
		{"another product", v1.TriggerOn{Product: "gitlab", Verbs: []string{"pull_request.opened"}}, false},
		{"a verb prefix", v1.TriggerOn{Product: "github", Verbs: []string{"push", "pull_request.*"}}, true},
		{"another verb", v1.TriggerOn{Product: "github", Verbs: []string{"push"}}, false},
		{"a resource prefix", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Resources: []string{"o/r#*"}}, true},
		{"another resource", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Resources: []string{"o/other#*"}}, false},
		{"a payload value", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.pull_request.base.ref", In: []string{"release-*", "main"}}}}, true},
		{"a boolean", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.pull_request.draft", In: []string{"false"}}}}, true},
		{"a number", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.pull_request.number", In: []string{"9"}}}}, true},
		{"every rule", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.action", In: []string{"opened"}}, {Path: "payload.pull_request.base.ref", In: []string{"dev"}}}}, false},
		{"a path to nothing", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.missing", In: []string{"*"}}}}, false},
		{"a path to an object", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.pull_request", In: []string{"*"}}}}, false},
		{"a path to a list", v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.pull_request.labels", In: []string{"*"}}}}, false},
	}
	for _, c := range cases {
		got, err := Matches(c.on, e)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	bad := e
	bad.Payload = json.RawMessage(`{"action":`)
	if _, err := Matches(v1.TriggerOn{Product: "github", Verbs: []string{"*"}, Match: []v1.TriggerMatch{{Path: "payload.action", In: []string{"*"}}}}, bad); err == nil {
		t.Fatal("a payload that does not decode matched")
	}
}

func TestAnEnvelopeIsChecked(t *testing.T) {
	ok := Envelope{ID: "1", Product: "github", Verb: "push", Resource: "o/r", Time: time.Now(), Payload: json.RawMessage(`{"a":1}`)}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	null := ok
	null.Payload = json.RawMessage(`null`)
	if err := null.Check(); err != nil {
		t.Fatalf("a null payload is no payload: %v", err)
	}
	for want, e := range map[string]Envelope{
		"no id, verb, time":    {Product: "github", Resource: "o/r"},
		"at most 200":          {ID: strings.Repeat("x", MaxEventID+1), Product: "github", Verb: "push", Resource: "o/r", Time: time.Now()},
		"lowercase":            {ID: "1", Product: "GitHub", Verb: "push", Resource: "o/r", Time: time.Now()},
		"payload is an object": {ID: "1", Product: "github", Verb: "push", Resource: "o/r", Time: time.Now(), Payload: json.RawMessage(`[1]`)},
	} {
		if err := e.Check(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", e, err, want)
		}
	}
	for p, want := range map[string]bool{"payload.action": true, "payload.a.0.b": true, "payload": false, "action": false, "payload.": false, "trigger.id": false} {
		if got := CheckMatchPath(p) == nil; got != want {
			t.Errorf("match path %q accepted %v", p, got)
		}
	}
}

func TestTheAddedFieldsDefault(t *testing.T) {
	var s v1.TriggerSpec
	if Policy(s) != v1.PolicyNew || Key(s) != "" || MaxActive(s) != DefaultMaxActive {
		t.Fatalf("a schedule's defaults: %s %q %d", Policy(s), Key(s), MaxActive(s))
	}
	s.On = &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}
	if Key(s) != DefaultEventKey {
		t.Fatalf("an event trigger's key: %q", Key(s))
	}
	n := 3
	s.Session.Policy, s.Session.Key, s.MaxActive = v1.PolicyContinue, "{{event.subject}}", &n
	if Policy(s) != v1.PolicyContinue || Key(s) != "{{event.subject}}" || MaxActive(s) != 3 {
		t.Fatalf("set fields: %s %q %d", Policy(s), Key(s), MaxActive(s))
	}
}
