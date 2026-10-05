// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/session"
)

// searchPage is a page of the search as a client reads it.
type searchPage struct {
	Items      []session.SearchResult `json:"items"`
	NextCursor string                 `json:"next_cursor"`
}

func (f *fixture) search(token, query string) (answer, searchPage) {
	f.t.Helper()
	a := f.do(http.MethodGet, "/v1/sessions/search?"+query, token, "")
	var p searchPage
	if a.status == http.StatusOK {
		a.decode(f.t, &p)
	}
	return a, p
}

func ids(p searchPage) []string {
	out := []string{}
	for _, r := range p.Items {
		out = append(out, r.Session.ID)
	}
	return out
}

func TestASearchFindsTheCallersSessionsByWhatWasSaid(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("bob", "reviewer", "Review.")
	mine := f.create("alice", "reviewer")
	f.turn(mine.ID, 1, "The **handler** leaks a goroutine in main.go.", 0)
	other := f.create("alice", "reviewer")
	f.turn(other.ID, 1, "Nothing to report.", 0)
	theirs := f.create("bob", "reviewer")
	f.turn(theirs.ID, 1, "The handler leaks too.", 0)

	f.authz.take()
	a, p := f.search("alice", "q=Handler+leak")
	if a.status != http.StatusOK {
		t.Fatalf("search: %d %s", a.status, a.body)
	}
	if got := f.authz.take(); !slices.Equal(got, []string{authorizer.ActionSessionList, authorizer.ActionSessionRead}) {
		t.Fatalf("the search asked %v, want the list's question and one read", got)
	}
	if !slices.Equal(ids(p), []string{mine.ID}) || p.NextCursor != "" {
		t.Fatalf("alice found %q (next %q), want only her own session", ids(p), p.NextCursor)
	}
	m := p.Items[0].Matches
	if len(m) != 1 || m[0].Type != session.TypeAgentMessage || m[0].Seq != 3 || m[0].EventID == "" {
		t.Fatalf("matches %+v", m)
	}
	var marked strings.Builder
	for _, fr := range m[0].Excerpt {
		if fr.Match {
			marked.WriteString("[" + fr.Text + "]")
		} else {
			marked.WriteString(fr.Text)
		}
	}
	if marked.String() != "The [handler] [leak]s a goroutine in main.go." {
		t.Fatalf("excerpt %q", marked.String())
	}
	if _, p := f.search("alice", "q=main.go"); len(p.Items) != 2 {
		t.Fatalf("both of alice's sessions say main.go: %q", ids(p))
	}
	if _, p := f.search("alice", "q=handler&agent=nobody"); len(p.Items) != 0 {
		t.Fatalf("an agent the caller holds none of found %q", ids(p))
	}
	if _, p := f.search("alice", "q=handler&status=ended"); len(p.Items) != 0 {
		t.Fatalf("the status filter found %q", ids(p))
	}
}

func TestASearchLeavesOutASessionTheCallerMayNotRead(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	var made []string
	for range 3 {
		s := f.create("alice", "reviewer")
		made = append(made, s.ID)
	}
	slices.Reverse(made)
	hidden := made[1]
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionRead && req.Resource.ID == hidden {
			return authz.Decision{Reason: "not_yours"}, nil
		}
		return f.authz.next.Authorize(t.Context(), req)
	}
	a, p := f.search("alice", "q=review&limit=2")
	if a.status != http.StatusOK || !slices.Equal(ids(p), []string{made[0]}) || p.NextCursor != hidden {
		t.Fatalf("first page: %d %q next %q, want %s and a cursor past the one left out", a.status, ids(p), p.NextCursor, made[0])
	}
	if link := a.header.Get("Link"); !strings.Contains(link, "cursor="+hidden) || !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link %q", link)
	}
	_, p = f.search("alice", "q=review&limit=2&cursor="+url.QueryEscape(p.NextCursor))
	if !slices.Equal(ids(p), []string{made[2]}) || p.NextCursor != "" {
		t.Fatalf("second page: %q next %q", ids(p), p.NextCursor)
	}

	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionRead {
			return authz.Decision{}, &authz.Unavailable{URL: "http://authz", Err: errors.New("connection refused")}
		}
		return f.authz.next.Authorize(t.Context(), req)
	}
	if a, _ := f.search("alice", "q=review"); a.status != http.StatusServiceUnavailable || a.code() != auth.CodeAuthorizerUnavailable {
		t.Fatalf("a read the authorizer could not decide: %d %s", a.status, a.body)
	}
}

func TestASearchPagesByTheLimit(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	for range session.MaxSearchResults + 1 {
		f.create("alice", "reviewer")
	}
	_, p := f.search("alice", "q=review")
	if len(p.Items) != session.MaxSearchResults || p.NextCursor == "" {
		t.Fatalf("a search without a limit holds %d, next %q", len(p.Items), p.NextCursor)
	}
	_, p = f.search("alice", "q=review&cursor="+p.NextCursor)
	if len(p.Items) != 1 || p.NextCursor != "" {
		t.Fatalf("the last page holds %d, next %q", len(p.Items), p.NextCursor)
	}
}

func TestASearchRefusesWhatItCannotRead(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.create("alice", "reviewer")
	for _, query := range []string{
		"", "q=", "q=+++", "q=" + url.QueryEscape("?! … 🙂"), "q=" + strings.Repeat("a", session.MaxSearchQuery+1),
		"q=review&limit=0", "q=review&limit=" + strconv.Itoa(session.MaxSearchResults+1), "q=review&limit=many",
		"q=review&status=sleeping", "q=review&archived=maybe", "q=review&runner=cloud",
	} {
		f.authz.take()
		a, _ := f.search("alice", query)
		if a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
			t.Errorf("%q: %d %s", query, a.status, a.body)
		}
		if strings.Contains(query, "limit") && !strings.Contains(string(a.body), strconv.Itoa(session.MaxSearchResults)) {
			t.Errorf("%q: the refusal does not name the bound: %s", query, a.body)
		}
	}
	if a, _ := f.search("alice", "q="+strings.Repeat("a", session.MaxSearchQuery)); a.status != http.StatusOK {
		t.Fatalf("the longest query: %d %s", a.status, a.body)
	}
}
