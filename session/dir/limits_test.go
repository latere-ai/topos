// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	objstoretest "latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// TestADirectoryRenamesItsSessionsSubagentLimits: a session whose agent a
// release before spec.subagentLimits kept, its spec and bundle with the
// limits under threads, opens with its agent renamed: its header names
// the renamed spec and bundle, runner.Agent reads it back as this release
// resolves its manifest, the blobs before are left, a session that runs
// no manifest is passed by, and the marker keeps a later Open from
// reading the sessions again (spec 013).
func TestADirectoryRenamesItsSessionsSubagentLimits(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	st := open(t.Context(), t, data)
	rs, err := manifest.Resolve(ctx, []byte(objstoretest.LimitsManifest), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	before := objstoretest.BeforeLimits(t, r)
	s := storetest.NewSession()
	s.Agent = session.AgentRef{ID: r.Agent.Status.ID, Name: r.Name, Version: 1, Digest: before.Digest, Bundle: session.DigestOf(before.Bundle)}
	if err := st.Create(ctx, s, map[session.Digest][]byte{before.Digest: before.Spec, s.Agent.Bundle: before.Bundle}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Agent(ctx, st, s); err == nil {
		t.Fatal("a session's agent kept before the rename reads without it")
	}
	bare := newSession(ctx, t, st)
	// A session directory that holds no header yet is passed by.
	if err := os.Mkdir(filepath.Join(data, "sessions", session.NewID(session.PrefixSession)), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(data, "sessions", limitsMarker)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	st = open(t.Context(), t, data)
	got, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Agent.Digest) != r.Digest {
		t.Fatalf("the session's agent digest is %s, want %s", got.Agent.Digest, r.Digest)
	}
	ran, ok, err := runner.Agent(ctx, st, got)
	if err != nil || !ok || ran.Digest != r.Digest || *ran.Agent.Spec.SubagentLimits.MaxDepth != 3 || *ran.Agent.Spec.Subagents[0].Spec.SubagentLimits.MaxConcurrent != 2 {
		t.Fatalf("the session's agent after the rename: %+v, %v, %v", ran.Agent, ok, err)
	}
	want, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	for d, body := range map[session.Digest][]byte{got.Agent.Bundle: want, got.Agent.Digest: r.Spec, s.Agent.Bundle: before.Bundle} {
		rc, err := st.Blob(ctx, s.ID, d)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if cerr := rc.Close(); err != nil || cerr != nil || !bytes.Equal(b, body) {
			t.Fatalf("blob %s after the rename: %v, %v\n%s", d, err, cerr, b)
		}
	}
	if b, err := st.Get(ctx, bare.ID); err != nil || b.Agent != bare.Agent {
		t.Fatalf("a session of no manifest after the rename: %+v, %v", b.Agent, err)
	}

	hdr, err := os.ReadFile(st.headerPath(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.ReplaceAll(hdr, []byte(got.Agent.Bundle), []byte(s.Agent.Bundle))
	if err := os.WriteFile(st.headerPath(s.ID), old, 0o600); err != nil {
		t.Fatal(err)
	}
	open(t.Context(), t, data)
	if again, err := os.ReadFile(st.headerPath(s.ID)); err != nil || !bytes.Equal(again, old) {
		t.Fatalf("a later Open renamed the agent again: %v", err)
	}
}

// TestARenameOfASessionsAgentThatCannotReadFailsTheOpen: a header that is
// not JSON, and an agent's bundle that is not a stored agent, fail the
// Open that would rename the session's agent, as does a bundle the
// session does not hold, and the Open writes no marker.
func TestARenameOfASessionsAgentThatCannotReadFailsTheOpen(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, st *Store, s session.Session){
		"a header that is not JSON": func(t *testing.T, st *Store, s session.Session) {
			if err := os.WriteFile(st.headerPath(s.ID), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a bundle the session does not hold": func(t *testing.T, st *Store, s session.Session) {
			hdr, err := st.header(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			hdr.Agent.Bundle = session.DigestOf([]byte("absent"))
			if err := st.writeHeader(s.ID, hdr); err != nil {
				t.Fatal(err)
			}
		},
		"a bundle that is no stored agent": func(t *testing.T, st *Store, s session.Session) {
			d, err := st.PutBlob(t.Context(), s.ID, bytes.NewReader([]byte(`{"kind":"Agent"}`+"\n")))
			if err != nil {
				t.Fatal(err)
			}
			hdr, err := st.header(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			hdr.Agent.Bundle = d
			if err := st.writeHeader(s.ID, hdr); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			st := open(t.Context(), t, data)
			s := newSession(t.Context(), t, st)
			spoil(t, st, s)
			marker := filepath.Join(data, "sessions", limitsMarker)
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(t.Context(), data); err == nil {
				t.Fatalf("Open renamed over %s", name)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the marker after a failed rename: %v", err)
			}
		})
	}
}
