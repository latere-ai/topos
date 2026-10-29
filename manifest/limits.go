// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"errors"
	"fmt"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// Renamed is a stream of stored Agent documents as RenameSubagentLimits
// rewrote it.
type Renamed struct {
	// Stream is the documents as a store writes them: a version's
	// document alone, or a bundle as Bundle renders it.
	Stream []byte
	// Spec is the first document's spec as a session keeps it, the blob
	// Digest names, and Digest is the first document's status.digest.
	Spec   []byte
	Digest string
	// Changed is false when no document held spec.threads; Stream is
	// then the stream as given.
	Changed bool
}

// RenameSubagentLimits rewrites Agent documents a store kept before
// spec.threads was named spec.subagentLimits (spec 013): a version's
// document or a bundle, each document with its status. In each document
// the spec's threads, and the threads of every inline subagent's spec,
// move to subagentLimits with the same members, and status.digest becomes
// the digest of the spec so renamed, as a resolve computes it, so the
// stream reads with ReadBundle and an unchanged manifest applied again
// resolves to the same digest. A pinned agent keeps its reference, which
// names its id and version and no digest. It is the one reader of the old
// name, for the stores to rename what they kept.
func RenameSubagentLimits(stream []byte) (Renamed, error) {
	trees, err := parseJSON(stream)
	if err != nil {
		return Renamed{}, err
	}
	if len(trees) == 0 {
		return Renamed{}, errors.New("manifest: no stored agent to rename")
	}
	bundle := bytes.HasSuffix(stream, []byte("\n"))
	var out Renamed
	var buf bytes.Buffer
	for i, tree := range trees {
		kind, vp, ip := envelope(i, tree)
		m, ok := tree.(map[string]any)
		if !ok || kind != v1.KindAgent || len(vp)+len(ip) > 0 {
			return Renamed{}, fmt.Errorf("manifest: stored document %d is not a %s Agent", i+1, v1.APIVersion)
		}
		if renameLimits(m["spec"]) {
			out.Changed = true
		}
		ob, problems := decodeObject(i, kind, m, true)
		if len(problems) > 0 {
			return Renamed{}, newError(CodeInvalidManifest, len(trees), problems)
		}
		spec, err := session.Marshal(ob.agent.Spec)
		if err != nil {
			return Renamed{}, fmt.Errorf("manifest: render stored document %d: %w", i+1, err)
		}
		ob.agent.Status.Digest = string(session.DigestOf(spec))
		b, err := session.Marshal(ob.agent)
		if err != nil {
			return Renamed{}, fmt.Errorf("manifest: render stored document %d: %w", i+1, err)
		}
		buf.Write(b)
		if bundle {
			buf.WriteByte('\n')
		}
		if i == 0 {
			out.Spec, out.Digest = spec, ob.agent.Status.Digest
		}
	}
	out.Stream = stream
	if out.Changed {
		out.Stream = buf.Bytes()
	}
	return out, nil
}

// renameLimits moves a spec's threads to subagentLimits, and those of
// each inline subagent's spec, and reports whether it moved any. A spec
// that holds both keeps them, for the decode to refuse.
func renameLimits(spec any) bool {
	m, ok := spec.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if v, ok := m["threads"]; ok {
		if _, taken := m["subagentLimits"]; !taken {
			delete(m, "threads")
			m["subagentLimits"] = v
			changed = true
		}
	}
	subs, _ := m["subagents"].([]any)
	for _, s := range subs {
		if sub, ok := s.(map[string]any); ok && renameLimits(sub["spec"]) {
			changed = true
		}
	}
	return changed
}
