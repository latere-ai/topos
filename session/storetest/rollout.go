// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"slices"
	"testing"

	"latere.ai/x/topos/session"
)

// testListByRollout holds a store to spec 077's filter: none keeps the
// sessions of no rollout, a rollout's id that rollout's beside every
// other filter, any and the zero value every session; a session reads
// back with its rollout; and a summary counts the same set.
func testListByRollout(t *testing.T, st session.Store) {
	s := map[string]session.Session{}
	for _, c := range []struct{ name, rollout string }{
		{"a1", "cel_1.smoke"},
		{"b", ""},
		{"a2", "cel_1.smoke"},
		{"c", "cel_2.main"},
		{"a3", ""},
	} {
		x := NewSession()
		x.Rollout = c.rollout
		if c.name[0] != 'a' {
			x.Initiator.Subject = "usr_2"
		}
		if err := st.Create(t.Context(), x, nil); err != nil {
			t.Fatal(err)
		}
		s[c.name] = x
	}
	got, err := st.Get(t.Context(), s["a2"].ID)
	if err != nil || got.Rollout != "cel_1.smoke" {
		t.Fatalf("read back: rollout %q, %v; want cel_1.smoke", got.Rollout, err)
	}
	for _, c := range []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"no rollout", session.ListOptions{Rollout: session.RolloutNone}, []string{"a3", "b"}},
		{"one rollout", session.ListOptions{Rollout: "cel_1.smoke"}, []string{"a2", "a1"}},
		{"another rollout", session.ListOptions{Rollout: "cel_2.main"}, []string{"c"}},
		{"within the owners", session.ListOptions{Rollout: "cel_1.smoke", Owners: []string{"usr_2"}}, []string{}},
		{"no session's rollout", session.ListOptions{Rollout: "cel_3.smoke"}, []string{}},
		{"any", session.ListOptions{Rollout: session.RolloutAny}, []string{"a3", "c", "a2", "b", "a1"}},
		{"every session", session.ListOptions{}, []string{"a3", "c", "a2", "b", "a1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, st, c.o, s); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
	if summarizer, ok := st.(session.Summarizer); ok {
		sum, err := summarizer.Summarize(t.Context(), session.ListOptions{Rollout: session.RolloutNone})
		if err != nil || sum.Sessions.Idle+sum.Sessions.Running+sum.Sessions.Ended != 2 {
			t.Fatalf("summary of no rollout: %+v, %v; want 2 sessions", sum, err)
		}
	}
}
