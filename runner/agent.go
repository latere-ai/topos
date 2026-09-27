// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"

	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// AgentRef is the session header's reference to a resolved agent, and
// the two blobs it names: the resolved spec under the agent's digest and
// the bundle of the agent and the agents it pins. A session keeps both,
// so whoever runs it rebuilds the same agent without the store that
// resolved it (spec 003).
func AgentRef(r manifest.Resolved) (session.AgentRef, map[session.Digest][]byte, error) {
	if r.Agent == nil {
		return session.AgentRef{}, nil, fmt.Errorf("runner: %s %s is not an Agent", r.Kind, r.Name)
	}
	bundle, err := r.Bundle()
	if err != nil {
		return session.AgentRef{}, nil, err
	}
	st := r.Agent.Status
	ref := session.AgentRef{ID: st.ID, Name: r.Name, Version: st.Version, Digest: session.Digest(st.Digest), Bundle: session.DigestOf(bundle)}
	return ref, map[session.Digest][]byte{ref.Digest: r.Spec, ref.Bundle: bundle}, nil
}

// Agent reads a session's agent back from its bundle blob. A session
// whose header names no bundle runs no manifest, and Agent reports false.
func Agent(ctx context.Context, st session.Store, s session.Session) (manifest.Resolved, bool, error) {
	if s.Agent.Bundle == "" {
		return manifest.Resolved{}, false, nil
	}
	rc, err := st.Blob(ctx, s.ID, s.Agent.Bundle)
	if err != nil {
		return manifest.Resolved{}, false, err
	}
	b, err := io.ReadAll(rc)
	if err := errors.Join(err, rc.Close()); err != nil {
		return manifest.Resolved{}, false, err
	}
	r, err := manifest.ReadBundle(b)
	if err != nil {
		return manifest.Resolved{}, false, err
	}
	if r.Digest != string(s.Agent.Digest) {
		return manifest.Resolved{}, false, fmt.Errorf("runner: the bundle holds agent %s, the session names %s", r.Digest, s.Agent.Digest)
	}
	return r, true, nil
}
