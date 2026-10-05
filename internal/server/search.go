// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/session"
)

// searchSessions is GET /sessions/search (spec 050): the sessions of the
// caller's view whose messages and answers hold every word of q, newest
// first as the list orders them, each with its newest matches and an
// excerpt of each.
//
// The view is the list's: the route asks session.list with the list's
// fields and filters, and searches the agents of the caller's context
// among the owners its decision narrows to. A match's excerpt is the
// session's content, so each session found is then asked session.read,
// the question a read of its log asks, and one the caller may not read is
// left out of the page; a page may then hold fewer sessions than its
// limit while another follows.
func (c *call) searchSessions() error {
	q := c.r.URL.Query()
	words := q.Get("q")
	if strings.TrimSpace(words) == "" {
		return refuse(CodeInvalidRequest, "q is required: the words to search for")
	}
	query, err := session.ParseQuery(words)
	switch {
	case errors.Is(err, session.ErrQueryTooLong):
		return refuse(CodeInvalidRequest, "q is %d characters, at most %d", utf8.RuneCountInString(words), session.MaxSearchQuery)
	case errors.Is(err, session.ErrQueryEmpty):
		return refuse(CodeInvalidRequest, "q holds no word to search for, only marks and spaces")
	case err != nil:
		return err
	}
	limit := session.MaxSearchResults
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > session.MaxSearchResults {
			return refuse(CodeInvalidRequest, "limit is %q, not between 1 and %d", v, session.MaxSearchResults)
		}
		limit = n
	}
	o, none, err := c.sessionScope(session.Status(q.Get("status")))
	if err != nil {
		return err
	}
	if none {
		return c.replyPage([]session.SearchResult{}, "")
	}
	o.Limit, o.Cursor = limit, q.Get("cursor")
	ctx := c.r.Context()
	hits, next, err := session.Search(ctx, c.s.o.Sessions, session.SearchOptions{ListOptions: o, Query: query})
	if err != nil {
		return err
	}
	found := []session.SearchResult{}
	for _, h := range hits {
		if _, err := c.ask(ctx, authorizer.ActionSessionRead, sessionResource(h.Session, nil)); err != nil {
			if auth.Code(err) == auth.CodeNotFound {
				continue
			}
			return err
		}
		matches := make([]session.Match, len(h.Events))
		for i, e := range h.Events {
			matches[i] = session.MatchOf(e, query)
		}
		found = append(found, session.SearchResult{Session: h.Session, Matches: matches})
	}
	return c.replyPage(found, next)
}
