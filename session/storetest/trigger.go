// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"slices"
	"testing"

	"latere.ai/x/topos/session"
)

// testListByTrigger holds a store to spec 070's filter: a list keeps the
// sessions the trigger started, a page at a time, beside every other
// filter, within the owners and within a tree, and under a grouping by
// tree, where a fork the trigger did not start leaves its tree to the
// session the trigger did; an id no session names, and a session's own
// id, list none; and a summary counts the same set.
func testListByTrigger(t *testing.T, st session.Store) {
	trg1, trg2 := session.NewID(session.PrefixTrigger), session.NewID(session.PrefixTrigger)
	s := map[string]session.Session{}
	for _, c := range []struct{ name, trigger string }{
		{"a1", trg1},
		{"b", trg1},
		{"a2", trg2},
		{"c", ""},
		{"e", trg1},
		{"f", ""},
		{"a3", trg1},
	} {
		x := NewSession()
		x.TriggerID = c.trigger
		if c.name[0] != 'a' {
			x.Initiator.Subject = "usr_2"
		}
		if c.name == "f" {
			x.Parent, x.Root = &session.Parent{SessionID: s["e"].ID, Seq: 2}, s["e"].ID
		}
		if err := st.Create(t.Context(), x, nil); err != nil {
			t.Fatal(err)
		}
		s[c.name] = x
	}
	for _, c := range []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"one trigger", session.ListOptions{Trigger: trg1}, []string{"a3", "e", "b", "a1"}},
		{"another trigger", session.ListOptions{Trigger: trg2}, []string{"a2"}},
		{"within the owners", session.ListOptions{Trigger: trg1, Owners: []string{"usr_1"}}, []string{"a3", "a1"}},
		{"within the agents", session.ListOptions{Trigger: trg1, Agents: []string{s["b"].Agent.ID, s["a2"].Agent.ID}}, []string{"b"}},
		{"of an agent", session.ListOptions{Trigger: trg2, AgentID: s["a1"].Agent.ID}, []string{}},
		{"within a tree", session.ListOptions{Trigger: trg1, Root: s["e"].ID}, []string{"e"}},
		{"grouped by tree", session.ListOptions{Trigger: trg1, Group: session.GroupTree}, []string{"a3(a3 1)", "e(e 1)", "b(b 1)", "a1(a1 1)"}},
		{"beside an archived filter", session.ListOptions{Trigger: trg1, Archived: session.ArchivedOnly}, []string{}},
		{"no session's trigger", session.ListOptions{Trigger: session.NewID(session.PrefixTrigger)}, []string{}},
		{"a session's id", session.ListOptions{Trigger: s["a1"].ID}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, st, c.o, s); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
	if summarizer, ok := st.(session.Summarizer); ok {
		sum, err := summarizer.Summarize(t.Context(), session.ListOptions{Trigger: trg1})
		if err != nil || sum.Sessions.Idle != 4 || sum.Agents != 4 {
			t.Fatalf("summary of trigger %s: %+v, %v; want 4 idle sessions of 4 agents", trg1, sum, err)
		}
	}
}
