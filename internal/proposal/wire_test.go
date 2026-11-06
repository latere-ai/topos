// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"encoding/json/jsontext"
	"testing"
)

// TestAModelsInputIsReadAsItWrote: member names in another letter case and
// a repeated member are read, a member no field names is refused, and the
// allowance is read from a number or a string.
func TestAModelsInputIsReadAsItWrote(t *testing.T) {
	p, err := decode("toolu_1", jsontext.Value(`{"Action":"propose","action":"change","NAME":"Digest","perWeekUsd":"0.40"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Action != "change" || p.Name != "Digest" || p.PerWeekUSDMicro == nil || *p.PerWeekUSDMicro != 400000 {
		t.Fatalf("read %+v", p)
	}
	p, err = decode("toolu_1", jsontext.Value(`{"action":"propose","perWeekUsd":1.25,"ask":"a`+"\xff"+`"}`))
	if err != nil || p.PerWeekUSDMicro == nil || *p.PerWeekUSDMicro != 1250000 {
		t.Fatalf("a number allowance read %+v, %v", p, err)
	}
	if _, err := decode("toolu_1", jsontext.Value(`{"action":"propose","when":"now"}`)); err == nil {
		t.Fatal("a member no field names was read")
	}
	if _, err := decodeWake(jsontext.Value(`{"Action":"set","AT":"2026-10-24T09:00:00+02:00","at":"2026-10-24T09:00:00+02:00","in":"3h"}`)); err == nil {
		t.Fatal("a wake with a member no field names was read")
	}
	in, err := decodeWake(jsontext.Value(`{"Action":"set","AT":"2026-10-24T09:00:00+02:00","at":"2026-10-25T09:00:00+02:00"}`))
	if err != nil || in.Action != "set" || in.At != "2026-10-25T09:00:00+02:00" {
		t.Fatalf("a wake read %+v, %v", in, err)
	}
}

// TestAnEditKeepsTheNumbersAsWritten: a trigger document changed by a
// proposal keeps every number's digits and its members in sorted order.
func TestAnEditKeepsTheNumbersAsWritten(t *testing.T) {
	doc := jsontext.Value(`{"status":{"id":"trg_1"},"spec":{"budget":{"perWeek":9007199254740993},"ratio":1.50,"session":{"message":"x"},"list":[1,2.0]},"kind":"Trigger"}`)
	out, err := edit(doc, func(spec, session map[string]any) {
		session["title"] = "T"
		spec["suspend"] = true
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"Trigger","spec":{"budget":{"perWeek":9007199254740993},"list":[1,2.0],"ratio":1.50,"session":{"message":"x","title":"T"},"suspend":true}}`
	if string(out) != want {
		t.Fatalf("the document is\n%s\nwant\n%s", out, want)
	}
}

// TestAnEditSortsTheMembersOfObjectsInLists: an object inside a list is
// written with sorted members, as every other object is.
func TestAnEditSortsTheMembersOfObjectsInLists(t *testing.T) {
	doc := jsontext.Value(`{"spec":{"on":[{"verb":"a","product":"b"},{"z":1,"a":[{"y":2,"x":3}]}],"session":{}}}`)
	out, err := edit(doc, func(spec, session map[string]any) {})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"spec":{"on":[{"product":"b","verb":"a"},{"a":[{"x":3,"y":2}],"z":1}],"session":{}}}`
	if string(out) != want {
		t.Fatalf("the document is\n%s\nwant\n%s", out, want)
	}
}
