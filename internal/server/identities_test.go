// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/idpstub"
)

// orgTokens is tokens with one more form: a sub that starts acme- is
// signed in to the organization acme, whose token carries org_id.
type orgTokens struct{}

func (orgTokens) Authenticate(r *http.Request) (auth.Caller, error) {
	c, err := tokens{}.Authenticate(r)
	if err != nil {
		return c, err
	}
	if tok, _ := bearer.FromRequest(r); strings.HasPrefix(tok, "acme-") {
		c.Claims["org_id"] = "acme"
	}
	return c, nil
}

// identityFixture is the API with the stub identity provider as its
// Identities, and the session.create resources the authorizer was asked.
type identityFixture struct {
	*fixture
	idp     *idpstub.Server
	mu      sync.Mutex
	creates []authz.Resource
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	idp := idpstub.New(t, "host", "secret")
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ic, err := identity.New(identity.Options{URL: idp.URL(), ClientID: "host", SecretFile: file, HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	f := &identityFixture{idp: idp}
	f.fixture = newFixture(t, func(o *Options) { o.Identities = ic; o.Verifier = orgTokens{} })
	policy := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == "session.create" {
			f.mu.Lock()
			f.creates = append(f.creates, req.Resource)
			f.mu.Unlock()
		}
		return policy.Authorize(context.Background(), req)
	}
	return f
}

// end ends the session as its initiator.
func (f *identityFixture) end(token, id string) {
	f.t.Helper()
	if a := f.do(http.MethodPost, "/v1/sessions/"+id+"/end", token, `{"reason":"completed"}`); a.status != http.StatusOK {
		f.t.Fatalf("end: %d %s", a.status, a.body)
	}
}

// TestAgentIdentityLifecycle: an agent first applied gets its identity,
// owned by the applier, or by the organization the applier's token names,
// and keeps the subject as status.identity through its versions; a
// session's create asks the authorizer with its id and that identity; an
// archive archives the identity before the agent, and the identity is
// disabled with the confirmation once no session of the agent is left,
// at the archive when none runs and by the reconcile pass otherwise; a
// refused or unanswered create refuses the apply and stores nothing.
func TestAgentIdentityLifecycle(t *testing.T) {
	f := newIdentityFixture(t)
	a := f.apply("alice", "reviewer", "Review.")
	agents := f.idp.Agents()
	if len(agents) != 1 || a.Status.Identity == "" || agents[0].Subject != a.Status.Identity || agents[0].Ref != a.Status.ID ||
		agents[0].Owner != (idpstub.Owner{Type: "user", ID: "alice"}) || agents[0].AppliedBy != "alice" || agents[0].Name != "reviewer" {
		t.Fatalf("identity %+v, status %+v", agents, a.Status)
	}
	if v2 := f.apply("alice", "reviewer", "Review twice."); v2.Status.Version != 2 || v2.Status.Identity != a.Status.Identity || f.idp.Count(idpstub.OpPut) != 1 {
		t.Fatalf("a second version %+v, %d creates", v2.Status, f.idp.Count(idpstub.OpPut))
	}
	org := f.apply("acme-carol", "triager", "Triage.")
	if got := f.idp.Agents()[1]; got.Subject != org.Status.Identity || got.Owner != (idpstub.Owner{Type: "organization", ID: "acme"}) || got.AppliedBy != "acme-carol" {
		t.Fatalf("an organization's agent %+v", got)
	}
	s := f.create("alice", "reviewer")
	f.mu.Lock()
	fields := f.creates[len(f.creates)-1].Fields
	f.mu.Unlock()
	if fields["session_id"] != s.ID || fields["agent_identity"] != a.Status.Identity {
		t.Fatalf("session.create asked with %v, session %s", fields, s.ID)
	}
	if ar := f.do(http.MethodPost, "/v1/agents/reviewer/archive", "alice", ""); ar.status != http.StatusOK {
		t.Fatalf("archive: %d %s", ar.status, ar.body)
	}
	if got := f.idp.Agents()[0]; got.Status != idpstub.StatusArchived {
		t.Fatalf("the archived agent's identity is %s", got.Status)
	}
	if err := f.api.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.idp.Agents()[0]; got.Status != idpstub.StatusArchived || f.idp.Count(idpstub.OpDisable) != 0 {
		t.Fatalf("an identity was disabled while its session is live: %+v", got)
	}
	f.end("alice", s.ID)
	if err := f.api.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.idp.Agents()[0]; got.Status != idpstub.StatusDisabled {
		t.Fatalf("the last session ended and the identity is %s", got.Status)
	}
	for _, r := range f.idp.Requests() {
		if (r.Op == idpstub.OpArchive || r.Op == idpstub.OpDisable) && !strings.Contains(string(r.Body), `"permanent":true`) {
			t.Fatalf("%s without the confirmation: %s", r.Op, r.Body)
		}
	}
	// An agent with no session is disabled at its archive.
	if ar := f.do(http.MethodPost, "/v1/agents/triager/archive", "acme-carol", ""); ar.status != http.StatusOK {
		t.Fatalf("archive: %d %s", ar.status, ar.body)
	}
	if got := f.idp.Agents()[1]; got.Status != idpstub.StatusDisabled {
		t.Fatalf("an archived agent with no session keeps its identity %s", got.Status)
	}
	// A refusal, and silence, refuse the apply and store nothing.
	for _, c := range []struct {
		f    idpstub.Failure
		code string
	}{
		{idpstub.Failure{Status: http.StatusForbidden, Code: "owner_not_served"}, CodeIdentityRefused},
		{idpstub.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"}, CodeIdentityUnavailable},
	} {
		f.idp.Fail(idpstub.OpPut, c.f)
		if ar := f.do(http.MethodPut, "/v1/agents/linter", "alice", agentYAML("linter", "Lint.")); ar.code() != c.code {
			t.Fatalf("%+v: %d %s", c.f, ar.status, ar.body)
		}
		if _, err := f.objects.Agent(t.Context(), "linter"); err == nil {
			t.Fatalf("%+v stored the agent", c.f)
		}
	}
	// A refused archive leaves the agent unarchived.
	f.apply("alice", "linter", "Lint.")
	f.idp.Fail(idpstub.OpArchive, idpstub.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})
	if ar := f.do(http.MethodPost, "/v1/agents/linter/archive", "alice", ""); ar.code() != CodeIdentityUnavailable {
		t.Fatalf("an unanswered archive: %d %s", ar.status, ar.body)
	}
	if stored, err := f.objects.Agent(t.Context(), "linter"); err != nil || stored.ArchivedAt != nil {
		t.Fatalf("a failed archive archived the agent: %+v %v", stored, err)
	}
}

// TestReconcileCatchesUp: the reconcile pass disables an archived
// identity whose agent the store does not hold, leaves one whose
// agent's archive did not reach the store, and reports a provider that
// does not answer; a server with no identity provider reconciles
// nothing, and a session of an agent applied before the provider was
// configured is refused agent_identity_missing.
func TestReconcileCatchesUp(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := t.Context()
	ic := f.api.o.Identities
	sub, err := ic.Create(ctx, "agent_gone", "gone", identity.Owner{Type: identity.OwnerUser, ID: "alice"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ic.Archive(ctx, "agent_gone"); err != nil {
		t.Fatal(err)
	}
	a := f.apply("alice", "reviewer", "Review.")
	if err := ic.Archive(ctx, a.Status.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, got := range f.idp.Agents() {
		want := map[string]string{sub: idpstub.StatusDisabled, a.Status.Identity: idpstub.StatusArchived}[got.Subject]
		if got.Status != want {
			t.Fatalf("%s is %s, want %s", got.Ref, got.Status, want)
		}
	}
	f.idp.Fail(idpstub.OpList, idpstub.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})
	if err := f.api.Reconcile(ctx); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("an unanswered list: %v", err)
	}
	plain := newFixture(t)
	if err := plain.api.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	plain.apply("alice", "early", "Early.")
	late := newIdentityFixture(t)
	late.sessions, late.objects = plain.sessions, plain.objects
	late.api.o.Sessions, late.api.o.Objects = plain.sessions, plain.objects
	if ar := late.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"early"}`); ar.code() != CodeAgentIdentityMissing {
		t.Fatalf("a session of an agent with no identity: %d %s", ar.status, ar.body)
	}
	if _, _, err := late.sessions.List(ctx, session.ListOptions{}); err != nil {
		t.Fatal(err)
	}
}
