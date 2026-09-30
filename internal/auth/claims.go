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
	org, _ := c.Claims["org_id"].(string)
	return map[string]any{"org_id": org}
}
