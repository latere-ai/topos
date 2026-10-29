// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"strings"
	"testing"

	"latere.ai/x/topos/session"
)

// stored is what a store kept of r before spec.threads was named
// spec.subagentLimits: its document and its bundle with each spec's
// subagentLimits under threads, and each status.digest the digest of the
// spec so spelled, as a resolve of that release computed it.
func stored(t *testing.T, r Resolved) (doc, bundle []byte) {
	t.Helper()
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err = r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	old := func(stream []byte, specs map[string][]byte) []byte {
		out := bytes.ReplaceAll(stream, []byte(`"subagentLimits":`), []byte(`"threads":`))
		for digest, spec := range specs {
			before := session.DigestOf(bytes.ReplaceAll(spec, []byte(`"subagentLimits":`), []byte(`"threads":`)))
			out = bytes.ReplaceAll(out, []byte(digest), []byte(before))
		}
		return out
	}
	specs := map[string][]byte{r.Digest: r.Spec}
	for _, p := range r.Pinned {
		spec, err := session.Marshal(p.Spec)
		if err != nil {
			t.Fatal(err)
		}
		specs[p.Status.Digest] = spec
	}
	return old(doc, specs), old(bundle, specs)
}

// TestAStoredAgentRenamesItsSubagentLimits: a version's document and a
// bundle stored with spec.threads, in the agent's spec, an inline
// subagent's spec and a pinned agent's, read with ReadBundle once renamed
// and not before. The renamed streams are byte for byte what this release
// stores for the same manifest, each digest the one its resolve computes,
// so the manifest applied again keeps its version; and a stream already
// renamed is reported unchanged.
func TestAStoredAgentRenamesItsSubagentLimits(t *testing.T) {
	s := newStore()
	helper := one(t, agent("helper", "model: {name: claude-haiku-4-5}", "tools: [read]", "subagentLimits: {maxDepth: 1}"), fixed(s))
	s.apply([]Resolved{helper})
	lead := one(t, agent("lead",
		"model: {name: claude-haiku-4-5}",
		"subagents:",
		"  - {name: helper, agent: helper}",
		"  - {name: inline, spec: {model: {name: m}, subagentLimits: {maxConcurrent: 3}}}",
	), fixed(s))
	doc, bundle := stored(t, lead)
	if !bytes.Contains(bundle, []byte(`"threads":`)) || bytes.Contains(bundle, []byte(`"subagentLimits":`)) {
		t.Fatalf("the stored bundle is not of the release before: %s", bundle)
	}
	if _, err := ReadBundle(bundle); err == nil || !strings.Contains(err.Error(), "spec.threads") {
		t.Fatalf("a bundle stored before reads without its rename: %v", err)
	}

	wantDoc, err := session.Marshal(lead.Agent)
	if err != nil {
		t.Fatal(err)
	}
	wantBundle, err := lead.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	d, err := RenameSubagentLimits(doc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenameSubagentLimits(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Changed || !b.Changed || !bytes.Equal(d.Stream, wantDoc) || !bytes.Equal(b.Stream, wantBundle) {
		t.Fatalf("the renamed streams:\n%s\n%s\nwant\n%s\n%s", d.Stream, b.Stream, wantDoc, wantBundle)
	}
	if d.Digest != lead.Digest || b.Digest != lead.Digest || !bytes.Equal(b.Spec, lead.Spec) {
		t.Fatalf("the renamed digest %s and %s, want %s", d.Digest, b.Digest, lead.Digest)
	}
	read, err := ReadBundle(b.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if *read.Agent.Spec.Subagents[1].Spec.SubagentLimits.MaxConcurrent != 3 || *read.Pinned[pin(helper.Agent.Status)].Spec.SubagentLimits.MaxDepth != 1 {
		t.Fatalf("the renamed bundle lost a limit: %s", b.Stream)
	}

	// The manifest applied again resolves to the renamed version's digest.
	s.apply([]Resolved{{Kind: lead.Kind, Name: lead.Name, Spec: b.Spec, Digest: b.Digest, Agent: read.Agent}})
	again := one(t, agent("lead",
		"model: {name: claude-haiku-4-5}",
		"subagents:",
		"  - {name: helper, agent: helper}",
		"  - {name: inline, spec: {model: {name: m}, subagentLimits: {maxConcurrent: 3}}}",
	), fixed(s))
	if again.Agent.Status.Version != lead.Agent.Status.Version || again.Digest != b.Digest {
		t.Fatalf("applied again: version %d digest %s, want version %d digest %s", again.Agent.Status.Version, again.Digest, lead.Agent.Status.Version, b.Digest)
	}

	same, err := RenameSubagentLimits(wantBundle)
	if err != nil || same.Changed || !bytes.Equal(same.Stream, wantBundle) || same.Digest != lead.Digest {
		t.Fatalf("a renamed bundle renamed again: %+v, %v", same, err)
	}
}

// TestRenameSubagentLimitsRefusesWhatIsNoStoredAgent: an empty stream,
// one that is not JSON, a document of another kind and an agent that does
// not decode are errors, and nothing is renamed.
func TestRenameSubagentLimitsRefusesWhatIsNoStoredAgent(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"not JSON":     "{",
		"a trigger":    `{"apiVersion":"topos.latere.ai/v1","kind":"Trigger","metadata":{"name":"t"},"spec":{}}`,
		"not an agent": `{"apiVersion":"topos.latere.ai/v1","kind":"Agent","metadata":{"name":"a"},"spec":{"threads":{"maxDepth":"deep"}}}`,
	} {
		if _, err := RenameSubagentLimits([]byte(in)); err == nil {
			t.Fatalf("%s renamed", name)
		}
	}
}
