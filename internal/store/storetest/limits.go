// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"bytes"
	"testing"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// LimitsManifest is an agent whose spec and inline subagent's spec set
// subagentLimits, for the tests of the rename of spec.threads (spec 013).
const LimitsManifest = "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: lead}\nspec:\n  model: {name: m}\n  subagentLimits: {maxDepth: 3}\n  subagents:\n  - {name: inline, spec: {model: {name: m}, subagentLimits: {maxConcurrent: 2}}}\n"

// Before is an agent as a release before spec.subagentLimits kept it:
// its document, bundle and spec with every subagentLimits under threads,
// and the digest of that spec, which its status records.
type Before struct {
	Doc, Bundle, Spec []byte
	Digest            session.Digest
}

// BeforeLimits renders r as a release before spec.subagentLimits stored
// it.
func BeforeLimits(t *testing.T, r manifest.Resolved) Before {
	t.Helper()
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	old := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte(`"subagentLimits":`), []byte(`"threads":`)) }
	spec := old(r.Spec)
	digest := session.DigestOf(spec)
	swap := func(b []byte) []byte { return bytes.ReplaceAll(old(b), []byte(r.Digest), []byte(digest)) }
	return Before{Doc: swap(doc), Bundle: swap(bundle), Spec: spec, Digest: digest}
}

// ApplyBeforeLimits stores LimitsManifest for owner as a release before
// spec.subagentLimits stored it, and returns the agent as this release
// resolves it, whose document, bundle and digest a store's rename arrives
// at, with what was stored.
func ApplyBeforeLimits(t *testing.T, st store.Store, owner string) (manifest.Resolved, Before) {
	t.Helper()
	rs, err := manifest.Resolve(t.Context(), []byte(LimitsManifest), manifest.Options{Lookup: store.Lookup(st, owner)})
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	b := BeforeLimits(t, r)
	status := r.Agent.Status
	a := store.Agent{ID: status.ID, Name: r.Name, Owner: owner, CreatedAt: status.CreatedAt}
	v := store.AgentVersion{AgentID: status.ID, Version: status.Version, Digest: string(b.Digest), Doc: b.Doc, Bundle: b.Bundle, CreatedBy: owner, CreatedAt: status.CreatedAt}
	if err := st.PutVersion(t.Context(), a, v); err != nil {
		t.Fatal(err)
	}
	return r, b
}

// RenamedLimits checks that st holds the agent ApplyBeforeLimits stored
// as this release stores r, and that r's manifest applied again resolves
// to that version.
func RenamedLimits(t *testing.T, st store.Store, owner string, r manifest.Resolved) {
	t.Helper()
	status := r.Agent.Status
	v, err := st.Version(t.Context(), status.ID, status.Version)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	if v.Digest != r.Digest || !bytes.Equal(v.Doc, doc) || !bytes.Equal(v.Bundle, bundle) {
		t.Fatalf("the version after the rename: digest %s, want %s\n%s\nwant\n%s", v.Digest, r.Digest, v.Bundle, bundle)
	}
	rs, err := manifest.Resolve(t.Context(), []byte(LimitsManifest), manifest.Options{Lookup: store.Lookup(st, owner)})
	if err != nil {
		t.Fatal(err)
	}
	if again := rs[0].Agent.Status; again.ID != status.ID || again.Version != status.Version || again.Digest != r.Digest {
		t.Fatalf("the manifest applied again resolves to %+v, want version %d of %s", again, status.Version, status.ID)
	}
}
