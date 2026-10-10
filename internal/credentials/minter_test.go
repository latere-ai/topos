// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/idpstub"
	"latere.ai/x/topos/test/stubs/keystub"
)

// fixture is a minter over the stub identity provider and the stub key
// routes, and a session of an agent whose identity is subject.
type fixture struct {
	m       *Minter
	idp     *idpstub.Server
	keys    *keystub.Server
	session string
	subject string
	now     time.Time
}

func newFixture(t *testing.T, withIdentity bool) *fixture {
	t.Helper()
	ctx := t.Context()
	idp := idpstub.New(t, "host", "secret")
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ic, err := identity.New(identity.Options{URL: idp.URL(), ClientID: "host", SecretFile: file, HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{idp: idp, keys: keystub.New(t, "keys-token"), now: time.Now()}
	objects := store.NewMemory(nil)
	rs, err := manifest.Resolve(ctx, []byte("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: reviewer}\nspec:\n  model: {name: m}\n  machine: {kind: cella}\n"), manifest.Options{Lookup: store.Lookup(objects, "alice")})
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	if withIdentity {
		if f.subject, err = ic.Create(ctx, r.Agent.Status.ID, "reviewer", identity.Owner{Type: identity.OwnerUser, ID: "alice"}, "alice"); err != nil {
			t.Fatal(err)
		}
		r.Agent.Status.Identity = f.subject
	}
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	st := r.Agent.Status
	if err := objects.PutVersion(ctx, store.Agent{ID: st.ID, Name: "reviewer", Owner: "alice", CreatedAt: st.CreatedAt}, store.AgentVersion{AgentID: st.ID, Version: 1, Digest: st.Digest, Doc: doc, Bundle: bundle, CreatedAt: st.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	sessions := session.NewMemoryStore()
	s := session.New(session.AgentRef{ID: st.ID, Name: "reviewer", Version: 1}, session.Sender{Subject: "alice", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineCella}, f.now)
	if err := sessions.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.session = s.ID
	f.keys.AddSession(s.ID)
	f.m, err = New(Options{Tokens: ic, Keys: &KeyRoutes{URL: f.keys.URL(), Token: "keys-token", HTTP: http.DefaultClient}, Sessions: sessions, Agents: objects, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// lease is a lease whose loss a test decides.
type lease struct{ lost chan struct{} }

func (l *lease) Renew(context.Context) error { return nil }
func (l *lease) Release() error              { return nil }
func (l *lease) Lost() <-chan struct{}       { return l.lost }

var keyValue = regexp.MustCompile(`^lux_[A-Za-z0-9_-]{40}$`)

// TestSessionAndSandboxLuxKeys, the minter's half: each workload of a
// session gets its own generated key, whose value is Lux's shape and
// whose hash alone reaches the authorizer; a key with less than ten
// minutes left is renewed under the same hash; another lease of the
// session replaces the key with a new value; an expired key is
// generated anew.
func TestSessionAndSandboxLuxKeys(t *testing.T) {
	f := newFixture(t, true)
	ctx := t.Context()
	first := f.m.Local(f.session, &lease{lost: make(chan struct{})})
	runnerKey, err := first.Credential(ctx, runner.AudienceLux, runner.WorkloadSession)
	if err != nil {
		t.Fatal(err)
	}
	sandboxKey, err := first.Credential(ctx, runner.AudienceLux, runner.WorkloadSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if !keyValue.MatchString(runnerKey.Value) || !keyValue.MatchString(sandboxKey.Value) || runnerKey.Value == sandboxKey.Value {
		t.Fatalf("keys %q and %q", runnerKey.Value, sandboxKey.Value)
	}
	for workload, v := range map[string]string{runner.WorkloadSession: runnerKey.Value, runner.WorkloadSandbox: sandboxKey.Value} {
		if k, ok := f.keys.Key(f.session, workload); !ok || k.Hash != HashKeyValue(v) || k.Puts != 1 {
			t.Fatalf("%s: the authorizer holds %+v", workload, k)
		}
	}
	for _, r := range f.keys.Requests() {
		if strings.Contains(r.Path, "lux_") {
			t.Fatalf("a value reached the authorizer: %s", r.Path)
		}
	}
	if again, _ := first.Credential(ctx, runner.AudienceLux, runner.WorkloadSession); again.Value != runnerKey.Value {
		t.Fatal("a fresh key was asked again")
	}
	if k, _ := f.keys.Key(f.session, runner.WorkloadSession); k.Puts != 1 {
		t.Fatalf("a fresh key was registered again: %+v", k)
	}
	f.now = f.now.Add(7 * time.Minute)
	renewed, err := first.Credential(ctx, runner.AudienceLux, runner.WorkloadSession)
	if err != nil || renewed.Value != runnerKey.Value {
		t.Fatalf("a renewal changed the value: %v", err)
	}
	if k, _ := f.keys.Key(f.session, runner.WorkloadSession); k.Puts != 2 || len(k.Replaced) != 0 {
		t.Fatalf("a key with under ten minutes left was not renewed under its hash: %+v", k)
	}
	second := f.m.Local(f.session, &lease{lost: make(chan struct{})})
	rotated, err := second.Credential(ctx, runner.AudienceLux, runner.WorkloadSession)
	if err != nil || rotated.Value == runnerKey.Value {
		t.Fatalf("a new lease kept the old key: %v", err)
	}
	if k, _ := f.keys.Key(f.session, runner.WorkloadSession); k.Hash != HashKeyValue(rotated.Value) || len(k.Replaced) != 1 {
		t.Fatalf("the new lease's key did not replace the old one: %+v", k)
	}
	f.now = f.now.Add(time.Hour)
	expired, err := second.Credential(ctx, runner.AudienceLux, runner.WorkloadSession)
	if err != nil || expired.Value == rotated.Value {
		t.Fatalf("an expired key was answered again: %v", err)
	}
}

// counted is a lease that counts the session's leases, at generation gen
// of a run of this process's leases that started at since.
type counted struct {
	lease
	gen, since int64
}

func (l *counted) Generation() int64 { return l.gen }
func (l *counted) Since() int64      { return l.since }

// TestAFollowingDriveTakesUpTheSessionsKey: each turn is a drive under a
// lease of its own, and a drive that follows the session's last drive in
// this process, with every lease between taken here, answers the key that
// drive held, so the turn registers nothing before its first model
// request. A drive after another process held the session, a lease that
// does not count, and a key held for a remote runner each get a new value,
// whose hash replaces the old one; a key taken up with under ten minutes
// left is renewed under its hash.
func TestAFollowingDriveTakesUpTheSessionsKey(t *testing.T) {
	f := newFixture(t, true)
	ctx := t.Context()
	ask := func(c runner.Credentials) string {
		t.Helper()
		k, err := c.Credential(ctx, runner.AudienceLux, runner.WorkloadSession)
		if err != nil {
			t.Fatal(err)
		}
		return k.Value
	}
	puts := func() int {
		t.Helper()
		k, _ := f.keys.Key(f.session, runner.WorkloadSession)
		return k.Puts + len(k.Replaced)
	}
	drive := func(gen, since int64) runner.Credentials {
		return f.m.Local(f.session, &counted{lost: make(chan struct{}), gen: gen, since: since})
	}
	first := ask(drive(3, 3))
	// A probe of the queue took generation 4 here and drove nothing.
	if got := ask(drive(5, 3)); got != first || puts() != 1 {
		t.Fatalf("the following drive got a new key: same %v, registrations %d", got == first, puts())
	}
	f.now = f.now.Add(7 * time.Minute)
	if got := ask(drive(6, 3)); got != first {
		t.Fatal("a renewal of a key taken up changed its value")
	}
	if k, _ := f.keys.Key(f.session, runner.WorkloadSession); k.Puts != 2 || len(k.Replaced) != 0 {
		t.Fatalf("a key taken up with under ten minutes left was not renewed under its hash: %+v", k)
	}
	// Another process held generation 7.
	afterOther := ask(drive(8, 8))
	if afterOther == first {
		t.Fatal("a drive after another process held the session took up the old key")
	}
	if k, _ := f.keys.Key(f.session, runner.WorkloadSession); k.Hash != HashKeyValue(afterOther) || len(k.Replaced) != 1 {
		t.Fatalf("the new key did not replace the old one: %+v", k)
	}
	// A lease that does not count the session's leases.
	plain := ask(f.m.Local(f.session, &lease{lost: make(chan struct{})}))
	if plain == afterOther {
		t.Fatal("a lease that does not count took up a key")
	}
	// A remote runner's key is never taken up by a local drive.
	remote, err := f.m.Credential(ctx, f.session, "claim:1", runner.AudienceLux, runner.WorkloadSession)
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(drive(9, 8)); got == remote.Value {
		t.Fatal("a local drive took up a remote runner's key")
	}
}

// TestTokensOnlyForTheLeaseHolder, the in-process half: a drive's
// credentials mint the agent's token with the session claim while its
// lease is held, and nothing once it is lost, with no call reaching the
// identity provider or the authorizer.
func TestTokensOnlyForTheLeaseHolder(t *testing.T) {
	f := newFixture(t, true)
	ctx := t.Context()
	l := &lease{lost: make(chan struct{})}
	c := f.m.Local(f.session, l)
	tok, err := c.Credential(ctx, "cella", runner.WorkloadSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := f.idp.Token(tok.Value); !ok || m.Subject != f.subject || m.SessionID != f.session || m.Workload != runner.WorkloadSandbox || m.Audience != "cella" {
		t.Fatalf("minted %+v", m)
	}
	mints, puts := f.idp.Count(idpstub.OpMint), len(f.keys.Requests())
	close(l.lost)
	for _, audience := range []string{"cella", runner.AudienceLux} {
		if _, err := c.Credential(ctx, audience, runner.WorkloadSession); !errors.Is(err, runner.ErrLeaseLost) {
			t.Fatalf("%s after the lease was lost: %v", audience, err)
		}
	}
	if f.idp.Count(idpstub.OpMint) != mints || len(f.keys.Requests()) != puts {
		t.Fatal("a lost lease reached the identity provider or the authorizer")
	}
}

// TestWhatTheMinterRefuses: an agent with no identity is ErrNoIdentity,
// an installation with no identity provider or no session keys mints
// nothing for those audiences, a bad workload or audience is an error,
// and the provider's and the authorizer's refusals pass through.
func TestWhatTheMinterRefuses(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, false)
	if _, err := f.m.Credential(ctx, f.session, "l", "cella", runner.WorkloadSession); !errors.Is(err, runner.ErrNoIdentity) {
		t.Fatalf("an agent with no identity: %v", err)
	}
	if _, err := f.m.Credential(ctx, "ses_none", "l", "cella", runner.WorkloadSession); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("an unknown session: %v", err)
	}
	for _, c := range []struct{ audience, workload string }{{"cella", "other"}, {"", runner.WorkloadSession}} {
		if _, err := f.m.Credential(ctx, f.session, "l", c.audience, c.workload); err == nil {
			t.Errorf("%+v minted", c)
		}
	}
	f.keys.EndSession(f.session)
	var refused *Refused
	var se *runner.SetupError
	if _, err := f.m.Credential(ctx, f.session, "l", runner.AudienceLux, runner.WorkloadSession); !errors.As(err, &refused) || !errors.As(err, &se) || se.Code != "session_ended" || refused.Error() == "" {
		t.Fatalf("an ended session's key: %v", err)
	}
	if _, err := f.m.Credential(ctx, f.session, "l", "cella", runner.WorkloadSession); !errors.As(err, &se) || se.Code != runner.CodeAgentIdentityMissing {
		t.Fatalf("the missing identity's code: %v", err)
	}
	if err := coded(identity.ErrUnavailable); !errors.As(err, &se) || se.Code != CodeUnavailable {
		t.Fatalf("an unanswered provider: %v", err)
	}
	none, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{"cella", runner.AudienceLux} {
		if _, err := none.Credential(ctx, f.session, "l", audience, runner.WorkloadSession); !errors.Is(err, runner.ErrNotMinted) {
			t.Fatalf("%s with nothing configured: %v", audience, err)
		}
	}
	if _, err := New(Options{Tokens: f.m.o.Tokens}); err == nil {
		t.Fatal("a token minter with no stores")
	}
	g := newFixture(t, true)
	g.idp.SetAudiences("arca")
	var ie *identity.Error
	if _, err := g.m.Credential(ctx, g.session, "l", "cella", runner.WorkloadSession); !errors.As(err, &ie) || !errors.As(err, &se) || se.Code != "invalid_target" {
		t.Fatalf("a refused mint: %v", err)
	}
}

// TestKeyRoutesAnswers: a server error or no answer is unavailable, a
// refusal outside the envelope is still a refusal, and an answer with no
// expiry is an error.
func TestKeyRoutesAnswers(t *testing.T) {
	ctx := t.Context()
	srv := func(status int, body string) *KeyRoutes {
		s := httptestServer(t, status, body)
		return &KeyRoutes{URL: s, Token: "t", HTTP: http.DefaultClient}
	}
	if _, err := srv(http.StatusServiceUnavailable, `{}`).Put(ctx, "s", "session", "h"); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("a 503: %v", err)
	}
	var refused *Refused
	if _, err := srv(http.StatusForbidden, `nope`).Put(ctx, "s", "session", "h"); !errors.As(err, &refused) || refused.Code != "unknown" {
		t.Fatalf("a refusal outside the envelope: %v", err)
	}
	if _, err := srv(http.StatusOK, `{"name":"k"}`).Put(ctx, "s", "session", "h"); err == nil {
		t.Fatal("an answer with no expiry")
	}
	if _, err := (&KeyRoutes{URL: "http://127.0.0.1:1", HTTP: http.DefaultClient}).Put(ctx, "s", "session", "h"); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("no answer: %v", err)
	}
	if v, err := NewKeyValue(); err != nil || !keyValue.MatchString(v) {
		t.Fatalf("a key value %q %v", v, err)
	}
}
