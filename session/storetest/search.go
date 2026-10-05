// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

// The search cases of spec 050. Every store is searched through
// session.Search, which reads a store's own index where it has one and
// each session's log otherwise, so one contract holds for both.

// Reply returns an agent.message event of the session's own thread
// holding text, and a thinking block beside it.
func Reply(t *testing.T, text string, at time.Time) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{
		Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
			{Type: ir.BlockThinking, Text: "pondering zeppelins"},
			{Type: ir.BlockText, Text: text},
		}},
		StopReason: ir.StopEndTurn,
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	e.Turn = 1
	return e
}

// searchFor parses q.
func searchFor(t *testing.T, q string) session.Query {
	t.Helper()
	query, err := session.ParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// found is the ids of the sessions a search of st for q finds in o's
// scope, on its first page, and the cursor of the next.
func found(t *testing.T, st session.Store, o session.ListOptions, q string) ([]string, string) {
	t.Helper()
	hits, next, err := session.Search(t.Context(), st, session.SearchOptions{ListOptions: o, Query: searchFor(t, q)})
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.Session.ID)
	}
	return ids, next
}

// sameAsScan holds a store's own search to the scan's answer.
func sameAsScan(t *testing.T, st session.Store, o session.ListOptions, q string) {
	t.Helper()
	if _, ok := st.(session.Searcher); !ok {
		return
	}
	opts := session.SearchOptions{ListOptions: o, Query: searchFor(t, q)}
	own, ownNext, err := session.Search(t.Context(), st, opts)
	if err != nil {
		t.Fatal(err)
	}
	scan, scanNext, err := session.ScanSearch(t.Context(), st, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ownNext != scanNext || len(own) != len(scan) {
		t.Fatalf("%q: the store's index finds %d sessions (next %q), the scan %d (next %q)", q, len(own), ownNext, len(scan), scanNext)
	}
	for i := range own {
		a, b := own[i], scan[i]
		if a.Session.ID != b.Session.ID || len(a.Events) != len(b.Events) {
			t.Fatalf("%q: hit %d is %s with %d events, the scan's %s with %d", q, i, a.Session.ID, len(a.Events), b.Session.ID, len(b.Events))
		}
		for j := range a.Events {
			if a.Events[j].ID != b.Events[j].ID || a.Events[j].Seq != b.Events[j].Seq {
				t.Fatalf("%q: hit %d's event %d is %s, the scan's %s", q, i, j, a.Events[j].ID, b.Events[j].ID)
			}
		}
	}
}

func testSearchMatches(t *testing.T, st session.Store) {
	ctx := t.Context()
	pricing := create(t, st)
	appendAll(t, st, pricing.ID, 0,
		Message(t, "Compare the **Pricing** of three plans for me", t0),
		Reply(t, "Here are the [plans](https://hidden.example/tariffs) side by side.", t0.Add(time.Second)))
	beijing := create(t, st)
	appendAll(t, st, beijing.ID, 0, Message(t, "我在北京工作，想找周末去处", t0))
	north := create(t, st)
	appendAll(t, st, north.ID, 0, Message(t, "北方的京城很冷", t0))
	hidden := create(t, st)
	result, err := session.NewEvent(session.TypeToolResult, session.ToolResult{ToolUseID: "tu_1", Content: []lux.Block{{Type: ir.BlockText, Text: "walrus figures from a tool"}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	threaded := Reply(t, "a subagent mentions walrus too", t0)
	threaded.Thread = "thr_child"
	secret := Message(t, "my walrus password is swordfish", t0)
	appendAll(t, st, hidden.ID, 0, result, threaded, secret)
	if err := st.Redact(ctx, hidden.ID, secret.ID, session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, "a secret"); err != nil {
		t.Fatal(err)
	}

	all := session.ListOptions{}
	for _, c := range []struct {
		q    string
		want []string
	}{
		{"pric", []string{pricing.ID}},
		{"PRICING plans", []string{pricing.ID}},
		{"pricing walrus", nil},
		{"side by side", []string{pricing.ID}},
		{"icing", nil},
		{"北京", []string{beijing.ID}},
		{"京", []string{beijing.ID, north.ID}},
		{"hidden tariffs", nil},
		{"walrus", nil},
		{"swordfish", nil},
		{"zeppelins", nil},
	} {
		got, next := found(t, st, all, c.q)
		want := slices.Clone(c.want)
		slices.SortFunc(want, func(a, b string) int { return strings.Compare(b, a) })
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(got, want) || next != "" {
			t.Errorf("search %q found %q (next %q), want %q", c.q, got, next, want)
		}
		sameAsScan(t, st, all, c.q)
	}

	hits, _, err := session.Search(ctx, st, session.SearchOptions{Query: searchFor(t, "plans")})
	if err != nil || len(hits) != 1 || len(hits[0].Events) != 2 {
		t.Fatalf("plans: %+v, %v", hits, err)
	}
	if hits[0].Events[0].Type != session.TypeAgentMessage || hits[0].Events[1].Type != session.TypeUserMessage {
		t.Fatalf("matches are not newest first: %s then %s", hits[0].Events[0].Type, hits[0].Events[1].Type)
	}
	m := session.MatchOf(hits[0].Events[1], searchFor(t, "plans"))
	var marked strings.Builder
	for _, f := range m.Excerpt {
		if f.Match {
			marked.WriteString("[" + f.Text + "]")
		} else {
			marked.WriteString(f.Text)
		}
	}
	if marked.String() != "Compare the Pricing of three [plans] for me" || m.Seq != hits[0].Events[1].Seq {
		t.Fatalf("excerpt %q at %d", marked.String(), m.Seq)
	}
}

func testSearchBounds(t *testing.T, st session.Store) {
	many := create(t, st)
	var evs []session.Event
	for i := range session.MaxSearchMatches + 2 {
		evs = append(evs, Message(t, "the lighthouse again", t0.Add(time.Duration(i)*time.Second)))
	}
	appendAll(t, st, many.ID, 0, evs...)
	long := create(t, st)
	appendAll(t, st, long.ID, 0, Message(t, strings.Repeat("filler ", session.MaxSearchText/7)+"farewell", t0))

	hits, _, err := session.Search(t.Context(), st, session.SearchOptions{Query: searchFor(t, "lighthouse")})
	if err != nil || len(hits) != 1 {
		t.Fatalf("lighthouse: %d hits, %v", len(hits), err)
	}
	if got := hits[0].Events; len(got) != session.MaxSearchMatches || got[0].Seq != uint64(session.MaxSearchMatches+2) || got[0].Seq <= got[1].Seq {
		t.Fatalf("%d matches, newest %d", len(got), got[0].Seq)
	}
	sameAsScan(t, st, session.ListOptions{}, "lighthouse")
	if ids, _ := found(t, st, session.ListOptions{}, "farewell"); len(ids) != 0 {
		t.Fatalf("a word past MaxSearchText was found: %q", ids)
	}
	if ids, _ := found(t, st, session.ListOptions{}, "filler"); len(ids) != 1 || ids[0] != long.ID {
		t.Fatalf("a long message's start: %q", ids)
	}
	sameAsScan(t, st, session.ListOptions{}, "filler")
}

func testSearchScope(t *testing.T, st session.Store) {
	archiver, archives := st.(session.Archiver)
	agents := []session.AgentRef{
		{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		{ID: session.NewID(session.PrefixAgent), Name: "writer", Version: 1},
	}
	var ids []string
	for i, c := range []struct {
		owner    string
		agent    int
		status   session.Status
		archived bool
	}{
		{"usr_a", 0, session.StatusIdle, false},
		{"usr_a", 1, session.StatusIdle, false},
		{"usr_b", 0, session.StatusEnded, false},
		{"usr_a", 0, session.StatusEnded, true},
		{"usr_a", 0, session.StatusIdle, false},
	} {
		s := NewSession()
		s.Agent, s.Initiator.Subject = agents[c.agent], c.owner
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
		}
		at := t0.Add(time.Duration(i) * time.Minute)
		evs := []session.Event{Message(t, "harbor schedule", at)}
		if c.status == session.StatusEnded {
			evs = append(evs, Status(t, session.StatusEnded, session.StopCompleted, at))
		}
		appendAll(t, st, s.ID, 0, evs...)
		if c.archived && archives {
			if _, err := archiver.SetArchived(t.Context(), s.ID, &at); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, s.ID)
	}
	desc := func(pick ...int) []string {
		var out []string
		for _, i := range pick {
			out = append(out, ids[i])
		}
		slices.SortFunc(out, func(a, b string) int { return strings.Compare(b, a) })
		return out
	}
	cases := []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"owners", session.ListOptions{Owners: []string{"usr_a"}}, desc(0, 1, 3, 4)},
		{"agents", session.ListOptions{Agents: []string{agents[1].ID}}, desc(1)},
		{"agent", session.ListOptions{AgentID: agents[0].ID, Owners: []string{"usr_b"}}, desc(2)},
		{"status", session.ListOptions{Status: session.StatusEnded}, desc(2, 3)},
	}
	if archives {
		cases = append(cases,
			struct {
				name string
				o    session.ListOptions
				want []string
			}{"archived out", session.ListOptions{Archived: session.ArchivedExclude}, desc(0, 1, 2, 4)},
			struct {
				name string
				o    session.ListOptions
				want []string
			}{"archived only", session.ListOptions{Archived: session.ArchivedOnly}, desc(3)})
	}
	for _, c := range cases {
		got, _ := found(t, st, c.o, "harbor")
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: found %q, want %q", c.name, got, c.want)
		}
		sameAsScan(t, st, c.o, "harbor")
	}

	// Pages follow the list's order and cursor, and end without one.
	var paged []string
	o := session.ListOptions{Limit: 2}
	for range len(ids) {
		page, next := found(t, st, o, "harbor schedule")
		if len(page) > 2 {
			t.Fatalf("a page of %d, limit 2", len(page))
		}
		sameAsScan(t, st, o, "harbor schedule")
		paged = append(paged, page...)
		if next == "" {
			break
		}
		if next != page[len(page)-1] {
			t.Fatalf("the cursor %q is not the page's last session %q", next, page[len(page)-1])
		}
		o.Cursor = next
	}
	if want := desc(0, 1, 2, 3, 4); !slices.Equal(paged, want) {
		t.Fatalf("paged %q, want %q", paged, want)
	}
}
