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
	return map[string]any{"org_id": c.Organization()}
}

// Organization is the organization the caller's token names in its
// org_id claim, the context the caller acts in, "" for the caller's own
// (spec 036). The core reads it to address what it holds, an agent's
// owner and the namespace its names are read in, never to decide.
func (c Caller) Organization() string {
	org, _ := c.Claims["org_id"].(string)
	return org
}
