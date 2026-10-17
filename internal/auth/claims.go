// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

// TriggerClaims are the claims of an apply's token that a trigger keeps
// to forward: its firings ask the authorizer as the trigger's owner,
// whose token is not at hand when it fires, with these as the claims
// (spec 022). They are the org_id claim alone, the context the trigger
// was applied in, "" for a personal one. The core reads no meaning from
// them; the authorizer decides.
func TriggerClaims(c Caller) map[string]any {
	return ContextClaims(c.Organization())
}

// FiredBy is the fired_by field of the session.create a trigger's firing
// asks (spec 077): the token_use and authorization_details claims of the
// fire request's own token, verbatim, so the authorizer can hold the
// session to the key the token was minted from. A firing's create is
// asked as the trigger's owner, whose token is not at hand, so the key
// rides on the resource and not on the claims, where the create would be
// narrowed by the key's grants as a request of the key's own. It is nil
// for a token whose token_use is empty, which no key minted: a person's
// own sign-in or a service's token. The core reads no meaning from it;
// the authorizer decides.
func FiredBy(c Caller) map[string]any {
	use, _ := c.Claims["token_use"].(string)
	if use == "" {
		return nil
	}
	out := map[string]any{"token_use": use}
	if grants, ok := c.Claims["authorization_details"]; ok {
		out["authorization_details"] = grants
	}
	return out
}

// ContextClaims are the claims a question asked for a person with no token
// of theirs in hand carries: the org_id claim alone, naming the context
// org, "" for the person's own. A trigger's firing asks with them (spec
// 022), and so does a turn's failover, for the session's initiator in the
// context the session runs in (spec 051). The core reads no meaning from
// them; the authorizer decides.
func ContextClaims(org string) map[string]any {
	return map[string]any{"org_id": org}
}

// Organization is the organization the caller's token names in its
// org_id claim, the context the caller acts in, "" for the caller's own
// (spec 036). The core reads it to address what it holds, an agent's
// owner and the namespace its names are read in, never to decide.
func (c Caller) Organization() string {
	org, _ := c.Claims["org_id"].(string)
	return org
}
