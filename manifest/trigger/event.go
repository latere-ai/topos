// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	v1 "latere.ai/x/topos/manifest/v1"
)

// MaxEventID bounds an envelope's id, in characters.
const MaxEventID = 200

// Envelope is a delivered event: the object the signals plane carries,
// so it can deliver to a trigger without translating (spec 022). ID is
// the producer's id for this delivery, unique per Product, which a
// server deduplicates on.
type Envelope struct {
	ID       string          `json:"id"`
	Product  string          `json:"product"`
	Verb     string          `json:"verb"`
	Resource string          `json:"resource"`
	Actor    string          `json:"actor,omitempty"`
	Subject  string          `json:"subject,omitempty"`
	Time     time.Time       `json:"time"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Check refuses an envelope that lacks a required field, whose id is
// past MaxEventID, whose product is not lowercase, or whose payload is
// not an object.
func (e Envelope) Check() error {
	var missing []string
	for _, f := range []struct{ name, v string }{{"id", e.ID}, {"product", e.Product}, {"verb", e.Verb}, {"resource", e.Resource}} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if e.Time.IsZero() {
		missing = append(missing, "time")
	}
	switch {
	case len(missing) > 0:
		return fmt.Errorf("the envelope has no %s", strings.Join(missing, ", "))
	case utf8.RuneCountInString(e.ID) > MaxEventID:
		return fmt.Errorf("the envelope's id is %d characters, at most %d", utf8.RuneCountInString(e.ID), MaxEventID)
	case e.Product != strings.ToLower(e.Product):
		return errors.New("the envelope's product is lowercase, such as github")
	}
	if p := bytes.TrimSpace(e.Payload); len(p) > 0 && !bytes.Equal(p, []byte("null")) && p[0] != '{' {
		return errors.New("the envelope's payload is an object")
	}
	return nil
}

// Value is the envelope as the event root of a template reads it: its
// fields, time in RFC 3339, and the payload decoded with its numbers
// kept as written.
func (e Envelope) Value() (map[string]any, error) {
	v := map[string]any{
		"id": e.ID, "product": e.Product, "verb": e.Verb, "resource": e.Resource,
		"actor": e.Actor, "subject": e.Subject, "time": e.Time.UTC().Format(time.RFC3339Nano),
	}
	if p := bytes.TrimSpace(e.Payload); len(p) > 0 {
		dec := json.NewDecoder(bytes.NewReader(p))
		dec.UseNumber()
		var payload any
		if err := dec.Decode(&payload); err != nil {
			return nil, fmt.Errorf("the envelope's payload does not decode: %w", err)
		}
		v["payload"] = payload
	}
	return v, nil
}

// Matches reports whether the event e selects falls inside on: its
// product exactly, a verb and a resource each by one of the entries
// given, and every match rule by the payload value at its path.
func Matches(on v1.TriggerOn, e Envelope) (bool, error) {
	if e.Product != on.Product || !anyPattern(on.Verbs, e.Verb) {
		return false, nil
	}
	if len(on.Resources) > 0 && !anyPattern(on.Resources, e.Resource) {
		return false, nil
	}
	if len(on.Match) == 0 {
		return true, nil
	}
	event, err := e.Value()
	if err != nil {
		return false, err
	}
	values := Values{RootEvent: event}
	for _, m := range on.Match {
		path, perr := ParsePath(RootEvent + "." + m.Path)
		if perr != nil {
			return false, fmt.Errorf("the match path %q: %w", m.Path, perr)
		}
		v, scalar := Lookup(values, path)
		if !scalar || !anyPattern(m.In, v) {
			return false, nil
		}
	}
	return true, nil
}

// MatchPayloadRoot is the first segment every on.match path names.
const MatchPayloadRoot = "payload"

// CheckMatchPath refuses an on.match path that is not a path into the
// payload.
func CheckMatchPath(p string) error {
	path, err := ParsePath(RootEvent + "." + p)
	if err != nil || len(path) < 3 || path[1] != MatchPayloadRoot {
		return fmt.Errorf("not a path into the payload, such as %s.action", MatchPayloadRoot)
	}
	return nil
}

// Pattern reports whether an entry of a filter matches v: exactly, or,
// ending in *, by the prefix before it.
func Pattern(entry, v string) bool {
	if prefix, ok := strings.CutSuffix(entry, "*"); ok {
		return strings.HasPrefix(v, prefix)
	}
	return entry == v
}

func anyPattern(entries []string, v string) bool {
	for _, e := range entries {
		if Pattern(e, v) {
			return true
		}
	}
	return false
}

// FilterProblem is one rule an event filter breaks: the path of the field
// under the filter, such as verbs[0] or match[1].path, and why.
type FilterProblem struct {
	Path, Detail string
}

// CheckFilter is every rule on breaks: a product, lowercase; at least one
// verb; each entry of verbs, resources and a match rule's in a value or a
// prefix and *; and each match rule a path into the payload with at least
// one entry. The resolver checks a trigger's on with it, and the trigger
// tool a proposal's before anyone is asked (spec 086).
func CheckFilter(on v1.TriggerOn) []FilterProblem {
	var out []FilterProblem
	add := func(path, detail string) { out = append(out, FilterProblem{path, detail}) }
	if on.Product == "" {
		add("product", "required")
	} else if on.Product != strings.ToLower(on.Product) {
		add("product", "a product is lowercase, such as github")
	}
	if len(on.Verbs) == 0 {
		add("verbs", "required: at least one verb, or * for every verb")
	}
	patterns := func(at string, entries []string) {
		for i, e := range entries {
			switch {
			case e == "":
				add(fmt.Sprintf("%s[%d]", at, i), "empty; an entry is a value, or a prefix and *")
			case strings.Contains(strings.TrimSuffix(e, "*"), "*"):
				add(fmt.Sprintf("%s[%d]", at, i), "a * may only end an entry, where it matches the prefix before it")
			}
		}
	}
	patterns("verbs", on.Verbs)
	patterns("resources", on.Resources)
	for i, m := range on.Match {
		mp := fmt.Sprintf("match[%d]", i)
		if err := CheckMatchPath(m.Path); err != nil {
			add(mp+".path", err.Error())
		}
		if len(m.In) == 0 {
			add(mp+".in", "required: at least one value the path's value may take")
		}
		patterns(mp+".in", m.In)
	}
	return out
}
