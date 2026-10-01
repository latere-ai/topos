// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
)

// owner is whom agents belong to (spec 036): a rendered subject and
// whether it is a person's or an organization's.
type owner struct {
	subject string
	kind    string
}

// contextOf is the owner of what a caller's context holds: the
// organization the caller's token names, rendered under the caller's
// issuer as a person is, or the caller as a person. The context
// addresses objects; the authorizer decides what the caller may do in
// it.
func contextOf(c auth.Caller) owner {
	if org := c.Organization(); org != "" {
		return owner{subject: authz.Subject(c.Issuer, org), kind: store.OwnerOrganization}
	}
	return owner{subject: c.Subject, kind: store.OwnerUser}
}

// ownerOf is an agent's owner.
func ownerOf(a store.Agent) owner {
	return owner{subject: a.Owner, kind: store.OwnerTypeOf(a.OwnerType)}
}

// organization reports whether the owner is an organization, and its id.
func (o owner) organization() (string, bool) {
	if o.kind != store.OwnerOrganization {
		return "", false
	}
	_, id, _ := authz.SplitSubject(o.subject)
	return id, true
}

// field is the owner as a question names it: a person's rendered
// subject, which every authorizer reads, or an organization as
// {type, id}.
func (o owner) field() any {
	if id, ok := o.organization(); ok {
		return map[string]any{"type": store.OwnerOrganization, "id": id}
	}
	return o.subject
}

// here is the owner of what the route's caller's context holds.
func (c *call) here() owner { return contextOf(c.caller) }

// agentsOf lists the ids of every agent o holds, the agents whose
// sessions a list in o's context holds.
func (s *Server) agentsOf(ctx context.Context, o owner) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		page, next, err := s.o.Objects.ListAgents(ctx, store.AgentList{Owners: []string{o.subject}, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			ids = append(ids, a.ID)
		}
		if next == "" {
			return ids, nil
		}
		cursor = next
	}
}
