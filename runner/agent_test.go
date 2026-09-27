// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

const reviewer = "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: claude-haiku-4-5}\n  instructions: Review.\n"

func TestASessionKeepsItsAgent(t *testing.T) {
	rs, err := manifest.Resolve(t.Context(), []byte(reviewer), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, blobs, err := AgentRef(rs[0])
	if err != nil || ref.Name != "reviewer" || ref.Version != 1 || ref.Bundle == "" || len(blobs) != 2 {
		t.Fatalf("AgentRef = %+v, %d blobs, %v", ref, len(blobs), err)
	}
	st := session.NewMemoryStore()
	s := session.New(ref, session.Sender{Subject: "u", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineCella}, time.Now())
	if err := st.Create(t.Context(), s, blobs); err != nil {
		t.Fatal(err)
	}
	r, ok, err := Agent(t.Context(), st, s)
	if err != nil || !ok || r.Name != "reviewer" || r.Agent.Spec.Instructions != "Review." {
		t.Fatalf("Agent = %+v, %v, %v", r.Name, ok, err)
	}

	builtin := s
	builtin.Agent.Bundle = ""
	if _, ok, err := Agent(t.Context(), st, builtin); ok || err != nil {
		t.Fatalf("a session of no manifest: %v, %v", ok, err)
	}
	other := s
	other.Agent.Digest = session.DigestOf([]byte("another spec"))
	if _, _, err := Agent(t.Context(), st, other); err == nil || !strings.Contains(err.Error(), "names") {
		t.Fatalf("a bundle of another agent: %v", err)
	}
	gone := s
	gone.Agent.Bundle = session.DigestOf([]byte("never stored"))
	if _, _, err := Agent(t.Context(), st, gone); err == nil {
		t.Fatal("a missing bundle read")
	}
	junk := []byte("not a bundle")
	bad := session.New(ref, s.Initiator, session.RunnerHosted, s.Machine, time.Now())
	bad.Agent.Bundle = session.DigestOf(junk)
	if err := st.Create(t.Context(), bad, map[session.Digest][]byte{bad.Agent.Bundle: junk}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Agent(t.Context(), st, bad); err == nil {
		t.Fatal("a bundle that does not parse read")
	}
	if _, _, err := AgentRef(manifest.Resolved{Kind: "Trigger", Name: "t"}); err == nil {
		t.Fatal("a Trigger made an agent ref")
	}
}
