// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
)

// ReasonUnknownAction is an action outside the vocabulary, or one asked
// about a resource of another kind. The client refuses one before the
// wire, so reaching it is a caller in the process that skipped the
// client.
const ReasonUnknownAction = "unknown_action"

// OwnerPolicy is the policy toposd applies with TOPOS_AUTHORIZER_URL
// unset: the admins of TOPOS_ADMIN_SUBJECTS act on every object, the
// subject that created an object owns it and may take every action on
// it, a create is allowed on an object that does not exist, and a list
// is allowed narrowed to the caller's own objects. Everything else is
// denied as not_owner. It carries no limits, so a session on it has the
// core's defaults.
//
// The owner is read off the resource's owner field, which the API fills
// from the object it looked up before it asked; a resource without one
// is an object that does not exist yet.
type OwnerPolicy struct {
	Admins []string
}

// Authorize answers one request: the policy's own decision intersected
// with the grants the caller's token carries, so a narrowed key is
// narrowed without an authorizer too. The intersection turns an allow
// into a deny and never the reverse.
func (p *OwnerPolicy) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	return restrict(p.decide(req), req), nil
}

// restrict narrows an allow by the caller's grants. A credential whose
// reach cannot be read is denied, not an outage: there is no authorizer
// that failed to answer.
func restrict(d authz.Decision, req authz.Request) authz.Decision {
	if !d.Allow {
		return d
	}
	grants, err := authz.ParseGrants(req.Claims)
	if err != nil {
		return authz.Decision{Reason: authz.ReasonGrant}
	}
	return authz.Restrict(authorizer.Core, d, req, grants)
}

// Decide is Authorize under the name latere.ai/x/pkg/authz/server calls
// a decider by, so the policy toposd runs in process is also an endpoint
// an operator can serve from it.
func (p *OwnerPolicy) Decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	return p.Authorize(ctx, req)
}

func (p *OwnerPolicy) decide(req authz.Request) authz.Decision {
	kind := authorizer.Kind(req.Action)
	switch {
	case strings.EqualFold(req.Resource.ID, authz.ProbeID):
		return authz.Decision{Reason: authz.ReasonProbe}
	case req.Subject == "":
		return authz.Decision{Reason: authz.ReasonAnonymous}
	case kind == "" || kind != req.Resource.Kind:
		return authz.Decision{Reason: ReasonUnknownAction}
	}
	admin := slices.Contains(p.Admins, req.Subject)
	if authz.IsList(req.Action) {
		if admin {
			return authz.Decision{Allow: true}
		}
		return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{req.Subject}}}
	}
	// A session runs an agent, so starting one is the agent owner's to
	// do, the way every other action on the agent is; a fork starts one
	// too, and is then the forked session's owner's.
	if (req.Action == authorizer.ActionSessionCreate || req.Action == authorizer.ActionSessionFork) && !admin && req.Resource.String("agent_owner") != req.Subject {
		return authz.Decision{Reason: authz.ReasonNotOwner}
	}
	owner := req.Resource.String("owner")
	frame := authz.Policy{Admins: p.Admins, Create: authorizer.Create(kind)}
	return frame.Decide(req, authz.Object{Exists: owner != "", Owner: owner})
}
