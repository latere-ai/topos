// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// Bundle renders a resolved Agent and the agents it pins as one JSON
// stream, the agent first and the pinned agents by reference, each a
// whole object with its status: what a session keeps so that it can be
// continued on another machine without the store that resolved it.
func (r Resolved) Bundle() ([]byte, error) {
	if r.Agent == nil {
		return nil, fmt.Errorf("manifest: a bundle holds an Agent, not a %s", r.Kind)
	}
	var buf bytes.Buffer
	write := func(a *v1.Agent) error {
		b, err := session.Marshal(a)
		if err != nil {
			return fmt.Errorf("manifest: render %s: %w", a.Metadata.Name, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
		return nil
	}
	if err := write(r.Agent); err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(r.Pinned))
	for ref := range r.Pinned {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		if err := write(r.Pinned[ref]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ReadBundle reads what Bundle wrote. It resolves nothing again: each
// object is decoded strictly with its status, and its spec must hash to
// the digest its status records.
func ReadBundle(b []byte) (Resolved, error) {
	trees, err := parseJSON(b)
	if err != nil {
		return Resolved{}, err
	}
	if len(trees) == 0 {
		return Resolved{}, errors.New("manifest: an empty bundle")
	}
	var r Resolved
	for i, tree := range trees {
		kind, vp, ip := envelope(i, tree)
		m, ok := tree.(map[string]any)
		if !ok || kind != v1.KindAgent || len(vp)+len(ip) > 0 {
			return Resolved{}, fmt.Errorf("manifest: bundle document %d is not a %s Agent", i+1, v1.APIVersion)
		}
		ob, problems := decodeObject(i, kind, m, true)
		if len(problems) > 0 {
			return Resolved{}, newError(CodeInvalidManifest, len(trees), problems)
		}
		spec, err := session.Marshal(ob.agent.Spec)
		if err != nil {
			return Resolved{}, fmt.Errorf("manifest: render bundle document %d: %w", i+1, err)
		}
		if d := string(session.DigestOf(spec)); d != ob.agent.Status.Digest {
			return Resolved{}, fmt.Errorf("manifest: bundle document %d hashes to %s, its status says %s", i+1, d, ob.agent.Status.Digest)
		}
		if i == 0 {
			r = Resolved{Kind: kind, Name: ob.name, Spec: spec, Digest: ob.agent.Status.Digest, Agent: ob.agent, Pinned: map[string]*v1.Agent{}}
			continue
		}
		r.Pinned[pin(ob.agent.Status)] = ob.agent
	}
	return r, nil
}
