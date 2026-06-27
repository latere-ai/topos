// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package sandbox

import "context"

// bearerCtxKey is the unexported context key under which a per-request bearer
// token is carried. Using an unexported type prevents collisions with keys
// from other packages.
type bearerCtxKey struct{}

// WithBearer returns a copy of ctx carrying bearer as the credential to use for
// outbound sandbox-provider requests made under that context. A TokenSource
// that honours the context (see cella.ContextTokenSource) reads it back via
// BearerFromContext.
//
// This is how the Topos control plane scopes an entire agent run to the
// session user's identity: it bridges the inbound user JWT to a user-subject
// Cella bearer once at run start, stores it here, and threads the resulting
// context through every sandbox call (create, exec, destroy).
func WithBearer(ctx context.Context, bearer string) context.Context {
	return context.WithValue(ctx, bearerCtxKey{}, bearer)
}

// BearerFromContext returns the bearer carried by ctx and whether one was set.
// An empty stored value returns ("", true) so callers can distinguish "set to
// empty" from "absent"; in practice WithBearer is only called with a non-empty
// token.
func BearerFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(bearerCtxKey{}).(string)
	return v, ok
}
