// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
)

// AuthorizerOptions selects the decision point: an installation's
// authorizer when URL is set, the owner policy over Admins when not.
// HTTP is the instrumented client the authorizer is called with.
type AuthorizerOptions struct {
	URL, Token string
	Admins     []string
	HTTP       *http.Client
	Now        func() time.Time
}

// NewAuthorizer is TOPOS_AUTHORIZER_URL's client, which carries the
// vocabulary so a typo is refused before the wire, or the owner policy.
func NewAuthorizer(o AuthorizerOptions) (authz.Authorizer, error) {
	if o.URL == "" {
		return &OwnerPolicy{Admins: o.Admins}, nil
	}
	return authz.NewClient(authz.Options{URL: o.URL, Token: o.Token, HTTP: o.HTTP, Now: o.Now, Vocabulary: authorizer.Vocabulary()})
}

// Guard asks the decision point one question per API action and turns
// the answer into what the API returns.
type Guard struct {
	Authorizer authz.Authorizer
}

// Envelope is the question for caller taking action on res, with the
// request's id, peer address and user agent.
func Envelope(c Caller, action string, res authz.Resource, r *http.Request) authz.Request {
	req := authz.Request{Subject: c.Subject, Issuer: c.Issuer, Sub: c.Sub, Claims: c.Claims, Action: action, Resource: res}
	if r != nil {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		req.Request = authz.Caller{ID: r.Header.Get("X-Request-Id"), IP: host, UserAgent: r.UserAgent()}
	}
	return req
}

// Ask asks one question and returns the allow, or the refusal the API
// answers:
//
//   - no decision could be had: authorizer_unavailable, never an allow;
//   - a denied read of one object: not_found, the same answer as an
//     object that does not exist;
//   - a denied mutation of an existing object: forbidden when the caller
//     may read the object, and not_found when it may not;
//   - any other deny, a create or a list: forbidden.
func (g Guard) Ask(ctx context.Context, req authz.Request) (authz.Decision, error) {
	d, err := g.Authorizer.Authorize(ctx, req)
	if err != nil {
		return authz.Decision{}, refuse(CodeAuthorizerUnavailable, err, "the authorizer gave no decision on %s", req.Action)
	}
	if d.Allow {
		return d, nil
	}
	verb := verbOf(req.Action)
	switch {
	case verb == "read":
		return authz.Decision{}, refuse(CodeNotFound, nil, "no %s %s", req.Resource.Kind, req.Resource.ID)
	case verb == "create" || authz.IsList(req.Action) || req.Resource.ID == "":
		return authz.Decision{}, refuse(CodeForbidden, nil, "%s is denied: %s", req.Action, d.Reason)
	}
	read := req
	read.Action = req.Resource.Kind + ".read"
	if !authorizer.Known(read.Action) {
		return authz.Decision{}, refuse(CodeForbidden, nil, "%s is denied: %s", req.Action, d.Reason)
	}
	rd, err := g.Authorizer.Authorize(ctx, read)
	switch {
	case err != nil:
		return authz.Decision{}, refuse(CodeAuthorizerUnavailable, err, "the authorizer gave no decision on %s", read.Action)
	case rd.Allow:
		return authz.Decision{}, refuse(CodeForbidden, nil, "%s is denied: %s", req.Action, d.Reason)
	}
	return authz.Decision{}, refuse(CodeNotFound, nil, "no %s %s", req.Resource.Kind, req.Resource.ID)
}

// Create asks session.create and decodes the limits the allow carries.
// Limits that do not decode refuse the create as authorizer_unavailable,
// because a ceiling that cannot be read is not applied.
func (g Guard) Create(ctx context.Context, req authz.Request) (authorizer.Limits, error) {
	d, err := g.Ask(ctx, req)
	if err != nil {
		return authorizer.Limits{}, err
	}
	l, err := authorizer.DecodeLimits(d)
	if err != nil {
		return authorizer.Limits{}, refuse(CodeAuthorizerUnavailable, err, "the authorizer's limits on %s do not decode", req.Action)
	}
	return l, nil
}

func verbOf(action string) string {
	_, verb, _ := strings.Cut(action, ".")
	return verb
}
