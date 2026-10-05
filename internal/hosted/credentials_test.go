// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// issued is a Credentials that answers a new value per call, named
// after the audience and the workload, each living life, or err.
type issued struct {
	mu    sync.Mutex
	n     map[string]int
	life  time.Duration
	err   map[string]error
	calls []string
}

func (c *issued) Credential(_ context.Context, audience, workload string) (runner.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, audience+"/"+workload)
	if err := c.err[audience]; err != nil {
		return runner.Credential{}, err
	}
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[audience+workload]++
	return runner.Credential{Value: fmt.Sprintf("%s-%s-%d", audience, workload, c.n[audience+workload]), ExpiresAt: time.Now().Add(c.life)}, nil
}

// helper is the topos-machine helper for the machine the tests run on.
func helper(t *testing.T) map[string][]byte {
	t.Helper()
	return helpers(t)
}

// credentialFixture is a hosted harness on the stub Lux and the stub
// Cella, whose drive's token source is src over creds.
type credentialFixture struct {
	lux   *luxstub.Server
	cella *cellastub.Server
	st    session.Store
	s     session.Session
	creds *issued
	h     func(context.Context, session.Session) (harness.Config, error)
}

func newCredentialFixture(t *testing.T, helpers map[string][]byte, creds *issued, file client.TokenSource, opts ...func(*CellaOptions)) credentialFixture {
	t.Helper()
	f := credentialFixture{lux: luxstub.New(t), cella: cellastub.New(t), st: session.NewMemoryStore(), creds: creds}
	f.lux.Script("anthropic/claude-haiku-4.5", luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: "ok"}}, StopReason: ir.StopEndTurn}})
	f.s = newSession(t, f.st, reviewer)
	f.s.ExpiresAt = time.Now().Add(time.Hour)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	co := CellaOptions{URL: f.cella.URL(), Token: file, Helpers: helpers, Dir: dir, ModelsURL: f.lux.URL(), OrigoURL: "https://origo.example"}
	for _, opt := range opts {
		opt(&co)
	}
	machines := Cella(co)
	if f.h, err = Harness(Options{Store: f.st, ModelsURL: f.lux.URL() + "/anthropic", ModelsKey: "installation-key", Machines: machines}); err != nil {
		t.Fatal(err)
	}
	return f
}

// opened builds the harness of s, opens its machine, as the first tool
// that acts on it does, and ends the session, answering the first
// failure: a machine that cannot be had answers as the setup error its
// code names.
func opened(ctx context.Context, h func(context.Context, session.Session) (harness.Config, error), s session.Session) error {
	cfg, err := open(ctx, h, s)
	if err != nil {
		return err
	}
	return cfg.Machine.Release(context.WithoutCancel(ctx), true)
}

// open is opened without the session's end, for a test that reads what
// the open made before the end removes it.
func open(ctx context.Context, h func(context.Context, session.Session) (harness.Config, error), s session.Session) (harness.Config, error) {
	cfg, err := h(ctx, s)
	if err != nil {
		return harness.Config{}, err
	}
	if oe, ok := errors.AsType[*machine.OpenError](machine.Open(ctx, cfg.Machine)); ok {
		return harness.Config{}, &runner.SetupError{Code: oe.Code, Err: oe.Err}
	}
	return cfg, nil
}

// TestSessionAndSandboxLuxKeys, the runner's half: a session on an
// installation that mints its credentials asks models with its own Lux
// key, asked again for every request, and never with the installation's
// key; its sandbox gets a second key as a Cella Secret scoped to Lux's
// host and mounted as LUX_KEY, beside LUX_URL, and the git host's token
// as a Secret scoped to the git host, which git inside the sandbox sends
// as a bearer; every call the runner makes to Cella carries the agent's
// token for the workload session.
func TestSessionAndSandboxLuxKeys(t *testing.T) {
	creds := &issued{life: 15 * time.Minute}
	f := newCredentialFixture(t, helper(t), creds, client.StaticToken("installation-bearer"))
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	cfg, err := f.h(ctx, f.s)
	if err != nil {
		t.Fatal(err)
	}
	// The sandbox and its secrets are made when a tool first acts on it.
	if err := machine.Open(ctx, cfg.Machine); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
			t.Error(err)
		}
	}()
	if cfg.Connection.Credential != "lux-session-1" {
		t.Fatalf("the model connection's credential is %q", cfg.Connection.Credential)
	}
	limit := int64(16)
	st, err := cfg.Model.Stream(ctx, models.Request{IR: ir.Request{Model: cfg.Connection.Model, Messages: []ir.Message{{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}, MaxTokens: &limit}, Connection: cfg.Connection})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reqs := f.lux.Requests()
	if len(reqs) == 0 {
		t.Fatal("the model was not asked")
	}
	for _, r := range reqs {
		key := r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
		if !strings.Contains(key, "lux-session-1") || strings.Contains(key, "installation-key") {
			t.Fatalf("a model request carried %q", key)
		}
	}
	name := cella.SandboxName(f.s.ID)
	for _, want := range []struct{ secret, value, host string }{{name + "-lux", "lux-sandbox-1", "127.0.0.1"}, {name + "-origo", "origo-sandbox-1", "origo.example"}} {
		sec, value, ok := f.cella.Secret(want.secret)
		if !ok || value != want.value || len(sec.Spec.Scope.Hosts) != 1 || sec.Spec.Scope.Hosts[0] != want.host ||
			sec.Spec.Inject != (cellav1.SecretInject{Header: "Authorization", Scheme: cellav1.SchemeBearer}) {
			t.Fatalf("%s: %+v %q %v", want.secret, sec.Spec, value, ok)
		}
	}
	sb, ok := f.cella.Sandbox(name)
	if !ok {
		t.Fatal("no sandbox")
	}
	mounts := map[string]string{}
	for _, m := range sb.Spec.Secrets {
		mounts[m.Env] = m.Name
	}
	if mounts[EnvLuxKey] != name+"-lux" || mounts[EnvOrigoToken] != name+"-origo" || sb.Spec.Env[EnvLuxURL] != f.lux.URL() {
		t.Fatalf("mounts %v, env %v", mounts, sb.Spec.Env)
	}
	res, err := cfg.Machine.Exec(ctx, machine.ExecRequest{Command: "git config --global --get http.https://origo.example/.extraheader; env | grep -c lux-sandbox || true", Timeout: time.Minute})
	if err != nil || !strings.HasPrefix(string(res.Output), "Authorization: Bearer cella-placeholder-"+name+"-origo\n0") {
		t.Fatalf("git in the sandbox: %q %v", res.Output, err)
	}
	for _, r := range f.cella.Requests() {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer cella-session-") {
			t.Fatalf("%s %s carried %q", r.Method, r.Path, r.Header.Get("Authorization"))
		}
	}
}

// TestSandboxCredentialsAreRenewed: before a sandbox credential expires
// the runner asks for it again and applies a new value to its Secret,
// until the drive ends.
func TestSandboxCredentialsAreRenewed(t *testing.T) {
	creds := &issued{life: runner.RefreshBefore + 500*time.Millisecond}
	f := newCredentialFixture(t, helper(t), creds, nil)
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	cfg, err := f.h(ctx, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Open(ctx, cfg.Machine); err != nil {
		t.Fatal(err)
	}
	name := cella.SandboxName(f.s.ID) + "-origo"
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if sec, value, _ := f.cella.Secret(name); sec.Status.Version >= 2 && value != "origo-sandbox-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the git host's token was not renewed before it expired")
		}
	}
	cancel()
	if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
		t.Fatal(err)
	}
}

// TestAnInstallationThatMintsNothingActsAsToday: with no session
// credentials the model connection carries the installation's key, Cella
// the installation's bearer, and the sandbox no secret; a server that
// mints no Cella token and has no bearer refuses the machine.
func TestAnInstallationThatMintsNothingActsAsToday(t *testing.T) {
	helpers := helper(t)
	creds := &issued{err: map[string]error{runner.AudienceLux: runner.ErrNotMinted, AudienceCella: runner.ErrNotMinted, AudienceOrigo: runner.ErrNotMinted}}
	f := newCredentialFixture(t, helpers, creds, client.StaticToken("installation-bearer"))
	ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil))
	cfg, err := f.h(ctx, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Open(ctx, cfg.Machine); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Machine.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Connection.Credential != "installation-key" {
		t.Fatalf("the connection's credential %q", cfg.Connection.Credential)
	}
	if _, _, ok := f.cella.Secret(cella.SandboxName(f.s.ID) + "-lux"); ok {
		t.Fatal("a sandbox secret with nothing minted")
	}
	for _, r := range f.cella.Requests() {
		if r.Header.Get("Authorization") != "Bearer installation-bearer" {
			t.Fatalf("%s carried %q", r.Path, r.Header.Get("Authorization"))
		}
	}
	g := newCredentialFixture(t, helpers, creds, nil)
	if err := opened(ctx, g.h, g.s); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("no Cella bearer at all: %v", err)
	}
}

// TestSessionCredentialFailuresCloseTheTurn: a refused key closes the
// turn with the minter's code, an agent with no identity with
// agent_identity_missing, and a refused sandbox token with its code; a
// connection to a base URL of the agent's own never carries the
// session's key.
func TestSessionCredentialFailuresCloseTheTurn(t *testing.T) {
	helpers := helper(t)
	for _, c := range []struct {
		audience string
		err      error
		code     string
	}{
		{runner.AudienceLux, &runner.SetupError{Code: "session_unknown", Err: errors.New("no record")}, "session_unknown"},
		{runner.AudienceLux, errors.New("no answer"), CodeModelCredentialMissing},
		{AudienceCella, runner.ErrNoIdentity, runner.CodeAgentIdentityMissing},
		{AudienceCella, errors.New("no answer"), CodeMachineUnavailable},
		{AudienceOrigo, &runner.SetupError{Code: "agent_disabled", Err: errors.New("disabled")}, "agent_disabled"},
	} {
		creds := &issued{life: 15 * time.Minute, err: map[string]error{c.audience: c.err}}
		f := newCredentialFixture(t, helpers, creds, client.StaticToken("b"))
		ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil))
		if err := opened(ctx, f.h, f.s); err == nil || code(t, err) != c.code {
			t.Errorf("%s refused with %v: %v, want %s", c.audience, c.err, err, c.code)
		}
	}
	creds := &issued{life: 15 * time.Minute}
	src := runner.NewTokenSource(creds, nil, nil)
	cat, err := models.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	b := builder{o: Options{ModelsURL: "https://lux.example/anthropic", ModelsKey: "installation-key", Model: &keyedModel{}}, cat: cat}
	own := models.Entry{Family: models.FamilyAnthropic, InputWindow: 200_000, MaxOutputTokens: 8_000}
	if _, conn, _, err := b.connect(runner.WithTokens(t.Context(), src), v1.AgentModel{Name: "anthropic/claude-haiku-4.5", BaseURL: "https://api.anthropic.com"}, own); err != nil || conn.Credential != "installation-key" {
		t.Fatalf("an agent's own base URL: %q %v", conn.Credential, err)
	}
	if len(creds.calls) != 0 {
		t.Fatalf("the session's key was asked for another base URL: %v", creds.calls)
	}
	km := &keyedModel{src: runner.NewTokenSource(&issued{err: map[string]error{runner.AudienceLux: errors.New("gone")}}, nil, nil)}
	if _, err := km.Stream(t.Context(), models.Request{}); err == nil || !strings.Contains(err.Error(), "the session's model key") {
		t.Fatalf("a key that could not be had: %v", err)
	}
	if _, err := hostOf("::"); err == nil {
		t.Fatal("a URL with no host")
	}
}

// TestAnInstallationGitCredential: an installation that mints no git
// host token and has a git host credential of its own,
// TOPOS_ORIGO_TOKEN_FILE's, puts that credential in the sandbox's git
// host Secret, scoped to the git host alone, and git in the sandbox
// sends its placeholder there; the sandbox gets no Lux key, Cella gets
// the installation's bearer, and the Secret is not renewed, since the
// credential has no expiry. The file is read at each open, so a rotated
// value reaches the next sandbox, a file that cannot be read refuses the
// machine without its contents, and a token the server mints for the
// git host is used in its place.
func TestAnInstallationGitCredential(t *testing.T) {
	helpers := helper(t)
	path := filepath.Join(t.TempDir(), "origo-token")
	write := func(v string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withFile := func(o *CellaOptions) { o.OrigoToken = client.TokenFile(path) }
	nothing := &issued{err: map[string]error{runner.AudienceLux: runner.ErrNotMinted, AudienceCella: runner.ErrNotMinted, AudienceOrigo: runner.ErrNotMinted}}
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"no token source", t.Context()},
		{"nothing minted", runner.WithTokens(t.Context(), runner.NewTokenSource(nothing, nil, nil))},
	} {
		t.Run(c.name, func(t *testing.T) {
			write("installation-git-1")
			f := newCredentialFixture(t, helpers, nothing, client.StaticToken("installation-bearer"), withFile)
			cfg, err := f.h(c.ctx, f.s)
			if err != nil {
				t.Fatal(err)
			}
			if err := machine.Open(c.ctx, cfg.Machine); err != nil {
				t.Fatal(err)
			}
			name := cella.SandboxName(f.s.ID)
			sec, value, ok := f.cella.Secret(name + "-origo")
			if !ok || value != "installation-git-1" || !slices.Equal(sec.Spec.Scope.Hosts, []string{"origo.example"}) ||
				sec.Spec.Inject != (cellav1.SecretInject{Header: "Authorization", Scheme: cellav1.SchemeBearer}) {
				t.Fatalf("the git host's Secret: %+v %v", sec.Spec, ok)
			}
			if _, _, ok := f.cella.Secret(name + "-lux"); ok {
				t.Fatal("the sandbox got a Lux key from an installation that mints none")
			}
			sb, ok := f.cella.Sandbox(name)
			if !ok || len(sb.Spec.Secrets) != 1 || sb.Spec.Secrets[0] != (cellav1.SecretMount{Name: name + "-origo", Env: EnvOrigoToken}) || sb.Spec.Env[EnvLuxURL] != "" {
				t.Fatalf("the sandbox's secrets %+v, env %v", sb.Spec.Secrets, sb.Spec.Env)
			}
			res, err := cfg.Machine.Exec(c.ctx, machine.ExecRequest{Command: "git config --global --get http.https://origo.example/.extraheader; env | grep -c installation-git || true", Timeout: time.Minute})
			if err != nil || string(res.Output) != "Authorization: Bearer cella-placeholder-"+name+"-origo\n0\n" {
				t.Fatalf("git in the sandbox: %q %v", res.Output, err)
			}
			// A renewal would come within a second of the open; the
			// installation's credential is applied once.
			time.Sleep(1500 * time.Millisecond)
			if sec, _, _ := f.cella.Secret(name + "-origo"); sec.Status.Version != 1 {
				t.Fatalf("the installation's credential was applied again: version %d", sec.Status.Version)
			}
			for _, r := range f.cella.Requests() {
				if r.Header.Get("Authorization") != "Bearer installation-bearer" {
					t.Fatalf("%s carried %q", r.Path, r.Header.Get("Authorization"))
				}
			}
			if err := cfg.Machine.Release(context.WithoutCancel(c.ctx), true); err != nil {
				t.Fatal(err)
			}
			write("installation-git-2")
			next := newSession(t, f.st, reviewer)
			next.ExpiresAt = time.Now().Add(time.Hour)
			ncfg, err := open(c.ctx, f.h, next)
			if err != nil {
				t.Fatal(err)
			}
			if _, value, _ := f.cella.Secret(cella.SandboxName(next.ID) + "-origo"); value != "installation-git-2" {
				t.Fatal("the next sandbox did not get the rotated credential")
			}
			if err := ncfg.Machine.Release(context.WithoutCancel(c.ctx), true); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			last := newSession(t, f.st, reviewer)
			last.ExpiresAt = time.Now().Add(time.Hour)
			err = opened(c.ctx, f.h, last)
			if code(t, err) != CodeMachineUnavailable || !strings.Contains(err.Error(), "TOPOS_ORIGO_TOKEN_FILE") {
				t.Fatalf("a file that cannot be read: %v", err)
			}
		})
	}
	t.Run("a minted token wins", func(t *testing.T) {
		write("installation-git-1")
		creds := &issued{life: 15 * time.Minute}
		f := newCredentialFixture(t, helpers, creds, client.StaticToken("installation-bearer"), withFile)
		ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
		defer cancel()
		cfg, err := open(ctx, f.h, f.s)
		if err != nil {
			t.Fatal(err)
		}
		if _, value, _ := f.cella.Secret(cella.SandboxName(f.s.ID) + "-origo"); value != "origo-sandbox-1" {
			t.Fatalf("the git host's Secret holds %q, not the session's token", value)
		}
		if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
			t.Fatal(err)
		}
	})
}

// TestTheInstallationsLabels: every sandbox and every Secret the runner
// applies for it carries TOPOS_CELLA_LABELS beside the session's and the
// agent's labels, the Secrets at their renewal too, since a Cella's
// authorizer may hold an object's placing labels fixed.
func TestTheInstallationsLabels(t *testing.T) {
	labels := map[string]string{"tenant.example/id": "t-1", "tenant.example/principal": "p"}
	creds := &issued{life: runner.RefreshBefore + 500*time.Millisecond}
	f := newCredentialFixture(t, helper(t), creds, nil, func(o *CellaOptions) { o.Labels = labels })
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	cfg, err := f.h(ctx, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Open(ctx, cfg.Machine); err != nil {
		t.Fatal(err)
	}
	name := cella.SandboxName(f.s.ID)
	sb, ok := f.cella.Sandbox(name)
	if !ok || sb.Metadata.Labels["tenant.example/id"] != "t-1" || sb.Metadata.Labels["tenant.example/principal"] != "p" || sb.Metadata.Labels[cella.LabelSession] != f.s.ID {
		t.Fatalf("the sandbox is labeled %v", sb.Metadata.Labels)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if sec, _, _ := f.cella.Secret(name + "-origo"); sec.Status.Version >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the git host's token was not renewed")
		}
	}
	want := maps.Clone(labels)
	want[cella.LabelSession], want[cella.LabelAgent] = f.s.ID, "reviewer"
	for _, secret := range []string{name + "-lux", name + "-origo"} {
		if sec, _, ok := f.cella.Secret(secret); !ok || !maps.Equal(sec.Metadata.Labels, want) {
			t.Fatalf("%s is labeled %v", secret, sec.Metadata.Labels)
		}
	}
	if !maps.Equal(sb.Metadata.Labels, want) {
		t.Fatalf("the sandbox is labeled %v", sb.Metadata.Labels)
	}
	cancel()
	if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
		t.Fatal(err)
	}
}

// reservedPrefix is the label prefix sessionRule's authorizer reserves for
// the placing labels it stamps itself.
const reservedPrefix = "platform.example/"

// sessionRule is the rule of an authorizer that binds a hosted session's
// Cella token to its session, as a hosting platform's may; that platform's
// own decider is not importable here, so the rule is written out. What the
// session creates is its caller's, names the session in LabelSession and
// carries no label under reservedPrefix, since those would place it in a
// tenant, which is the session's to hold and not the runner's to claim;
// what it reads, mounts or updates is what it made: its caller's and
// labeled with the session. Cella asks an update about the stored object,
// so a renewal is held to the Secret the create admitted.
func sessionRule(sessionID string) cellastub.Decider {
	return func(action string, res cellastub.Resource) string {
		if !strings.HasSuffix(action, ".create") {
			if res.Owner != cellastub.Owner || res.Labels[cella.LabelSession] != sessionID {
				return "not_in_org"
			}
			return ""
		}
		if res.Owner != cellastub.Owner || res.Labels[cella.LabelSession] != sessionID {
			return "invalid_tenant_mutation"
		}
		for k := range res.Labels {
			if strings.HasPrefix(k, reservedPrefix) {
				return "invalid_tenant_mutation"
			}
		}
		return ""
	}
}

// TestSessionSecretsNameTheSession: under an authorizer that admits only
// what names the session, the runner's Lux key and git host token
// Secrets are admitted at their create, the sandbox mounts them, and each
// renewal is admitted, because every Secret carries the session's and the
// agent's labels as the sandbox does; a Secret without the session's
// label, or with a label the authorizer reserves, is refused.
func TestSessionSecretsNameTheSession(t *testing.T) {
	creds := &issued{life: runner.RefreshBefore + 500*time.Millisecond}
	f := newCredentialFixture(t, helper(t), creds, nil)
	f.cella.Authorize(sessionRule(f.s.ID))
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	cfg, err := f.h(ctx, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Open(ctx, cfg.Machine); err != nil {
		t.Fatal(err)
	}
	name := cella.SandboxName(f.s.ID)
	want := map[string]string{cella.LabelSession: f.s.ID, cella.LabelAgent: "reviewer"}
	secrets := []string{name + "-lux", name + "-origo"}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		renewed := 0
		for _, secret := range secrets {
			if sec, _, _ := f.cella.Secret(secret); sec.Status.Version >= 2 {
				renewed++
			}
		}
		if renewed == len(secrets) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a sandbox credential was not renewed under the session's authorizer")
		}
	}
	for _, secret := range secrets {
		if sec, _, ok := f.cella.Secret(secret); !ok || !maps.Equal(sec.Metadata.Labels, want) {
			t.Fatalf("%s is labeled %v", secret, sec.Metadata.Labels)
		}
	}
	if sb, ok := f.cella.Sandbox(name); !ok || !maps.Equal(sb.Metadata.Labels, want) || len(sb.Spec.Secrets) != 2 {
		t.Fatalf("the sandbox %v %+v", sb.Metadata.Labels, sb.Spec.Secrets)
	}
	cancel()
	if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Config{URL: f.cella.URL(), Token: client.StaticToken("b")})
	if err != nil {
		t.Fatal(err)
	}
	for label, labels := range map[string]map[string]string{
		"no labels":                   nil,
		"the installation's alone":    {"tenant.example/id": "t-1"},
		"another session's":           {cella.LabelSession: "ses_other"},
		"a label the authorizer owns": {cella.LabelSession: f.s.ID, reservedPrefix + "tenant": "t-1"},
	} {
		sec := &sandboxSecret{name: "unnamed-" + strings.ReplaceAll(strings.ReplaceAll(label, " ", "-"), "'", ""), host: "lux.example", labels: labels}
		if err := sec.apply(t.Context(), c, runner.Credential{Value: "v"}); client.CodeOf(err) != "forbidden" {
			t.Errorf("a Secret with %s: %v, want forbidden", label, err)
		}
	}
}

// TestTheSessionsEndDeletesItsSecrets: a drive's end leaves the sandbox's
// Secrets for the session's next drive, and the session's end deletes the
// sandbox and then each Secret under an authorizer that admits only what
// names the session; no renewal applies one again after, a second end
// finds nothing left, and an installation's own git credential goes the
// same way, since its Secret is the session's record.
func TestTheSessionsEndDeletesItsSecrets(t *testing.T) {
	creds := &issued{life: runner.RefreshBefore + 500*time.Millisecond}
	f := newCredentialFixture(t, helper(t), creds, nil)
	f.cella.Authorize(sessionRule(f.s.ID))
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	name := cella.SandboxName(f.s.ID)
	secrets := []string{name + "-lux", name + "-origo"}
	gone := func(t *testing.T) {
		t.Helper()
		for _, secret := range secrets {
			if _, _, ok := f.cella.Secret(secret); ok {
				t.Fatalf("%s outlived the session", secret)
			}
		}
		if _, ok := f.cella.Sandbox(name); ok {
			t.Fatal("the sandbox outlived the session")
		}
	}

	// Each drive runs under a context of its own, which ends with it, as
	// the runner's does.
	drive, ended := context.WithCancel(ctx)
	first, err := open(drive, f.h, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Machine.Release(drive, false); err != nil {
		t.Fatal(err)
	}
	ended()
	for _, secret := range secrets {
		if _, _, ok := f.cella.Secret(secret); !ok {
			t.Fatalf("%s was deleted at a drive's end", secret)
		}
	}

	// The next drive opens the machine again, and the session ends while
	// its renewals are still running.
	cfg, err := open(ctx, f.h, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Machine.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	gone(t)
	// A renewal would have come within a second of the open.
	time.Sleep(1500 * time.Millisecond)
	gone(t)
	if err := cfg.Machine.Release(ctx, true); err != nil {
		t.Fatalf("a second end: %v", err)
	}

	t.Run("an installation's git credential", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "origo-token")
		if err := os.WriteFile(path, []byte("installation-git\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		nothing := &issued{err: map[string]error{runner.AudienceLux: runner.ErrNotMinted, AudienceCella: runner.ErrNotMinted, AudienceOrigo: runner.ErrNotMinted}}
		g := newCredentialFixture(t, helper(t), nothing, client.StaticToken("installation-bearer"), func(o *CellaOptions) { o.OrigoToken = client.TokenFile(path) })
		secret := cella.SandboxName(g.s.ID) + "-origo"
		cfg, err := open(t.Context(), g.h, g.s)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := g.cella.Secret(secret); !ok {
			t.Fatal("no git host Secret")
		}
		if err := cfg.Machine.Release(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := g.cella.Secret(secret); ok {
			t.Fatal("the installation's git Secret outlived the session")
		}
	})
}

// TestASecretCellaCouldNotDeleteIsDeletedAtTheNextEnd: a refused Secret
// delete fails the release, and releasing again deletes what is left
// without deleting the sandbox twice.
func TestASecretCellaCouldNotDeleteIsDeletedAtTheNextEnd(t *testing.T) {
	creds := &issued{life: 15 * time.Minute}
	f := newCredentialFixture(t, helper(t), creds, nil)
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	cfg, err := open(ctx, f.h, f.s)
	if err != nil {
		t.Fatal(err)
	}
	name := cella.SandboxName(f.s.ID)
	f.cella.Authorize(func(action string, res cellastub.Resource) string {
		if action == "secret.delete" && res.Name == name+"-origo" {
			return "held"
		}
		return ""
	})
	err = cfg.Machine.Release(ctx, true)
	if err == nil || !strings.Contains(err.Error(), name+"-origo") {
		t.Fatalf("a refused Secret delete: %v", err)
	}
	if _, _, ok := f.cella.Secret(name + "-lux"); ok {
		t.Fatal("the Secret Cella could delete was kept")
	}
	f.cella.Authorize(nil)
	deletes := f.cella.Count(cellastub.OpDelete)
	if err := cfg.Machine.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.cella.Secret(name + "-origo"); ok {
		t.Fatal("the next end left the Secret")
	}
	if n := f.cella.Count(cellastub.OpDelete); n != deletes {
		t.Fatalf("the next end deleted the sandbox again: %d deletes, want %d", n, deletes)
	}
}

// TestSearchCredential (spec 047): a hosted session whose agent names
// web_search is offered it with the installation's search service. On an
// installation that mints session keys each search carries the session's
// own key for the runner's workload, asked again at each search, and the
// sandbox's key is never asked for; one that mints none sends
// TOPOS_SEARCH_KEY; a key that cannot be had closes the turn; and with no
// search URL the tool is offered and answers that search is not
// available.
func TestSearchCredential(t *testing.T) {
	var seen []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","snippet":"The Go language."}],"cost_usd_micro":10000}`)
	}))
	t.Cleanup(svc.Close)
	st := session.NewMemoryStore()
	s := newSession(t, st, strings.Replace(reviewer, "tools: [read, grep]", "tools: [read, web_search]", 1))
	door := luxstub.New(t).URL() + "/anthropic"
	var asked v1.Machine
	harnessOf := func(o Options) func(context.Context, session.Session) (harness.Config, error) {
		t.Helper()
		o.Store, o.ModelsURL, o.Machines = st, door, hostMachines(t, &asked)
		h, err := Harness(o)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	searchOnce := func(ctx context.Context, cfg harness.Config) tools.Result {
		t.Helper()
		tool, ok := cfg.Tools.Get(tools.NameWebSearch)
		if !ok {
			t.Fatalf("web_search is not offered: %v", cfg.Tools.Names())
		}
		res, err := tool.Run(ctx, tools.Call{ID: "toolu_s", Input: json.RawMessage(`{"query":"go"}`), Machine: cfg.Machine})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Session keys: the session's own key, asked at each search.
	creds := &issued{life: time.Minute}
	ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil))
	h := harnessOf(Options{SearchURL: svc.URL})
	cfg, err := h(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	if !slices.Contains(cfg.Tools.Names(), tools.NameWebSearch) || !slices.Contains(cfg.Tools.Names(), "read") {
		t.Fatalf("offered %v", cfg.Tools.Names())
	}
	first, second := searchOnce(ctx, cfg), searchOnce(ctx, cfg)
	if first.Outcome != tools.OutcomeOK || first.CostUSDMicro == nil || *first.CostUSDMicro != 10000 || second.Outcome != tools.OutcomeOK {
		t.Fatalf("the searches %+v %+v", first, second)
	}
	n := len(seen)
	if n < 2 || !strings.HasPrefix(seen[n-2], "Bearer lux-session-") || seen[n-2] == seen[n-1] || !strings.HasPrefix(seen[n-1], "Bearer lux-session-") {
		t.Fatalf("the searches carried %v", seen)
	}
	if slices.Contains(creds.calls, runner.AudienceLux+"/"+runner.WorkloadSandbox) {
		t.Fatalf("the sandbox's key was asked for: %v", creds.calls)
	}

	// No session keys: the installation's search key.
	none := &issued{err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}
	plain := runner.WithTokens(t.Context(), runner.NewTokenSource(none, nil, nil))
	cfg, err = harnessOf(Options{SearchURL: svc.URL, SearchKey: "installation-search-key", ModelsKey: "k"})(plain, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	if searchOnce(plain, cfg); seen[len(seen)-1] != "Bearer installation-search-key" {
		t.Fatalf("an installation without session keys sent %q", seen[len(seen)-1])
	}

	// A key that cannot be had closes the turn.
	failing := runner.WithTokens(t.Context(), runner.NewTokenSource(&issued{err: map[string]error{runner.AudienceLux: errors.New("no answer")}}, nil, nil))
	if _, err := harnessOf(Options{SearchURL: svc.URL})(failing, s); code(t, err) != CodeModelCredentialMissing {
		t.Fatalf("a key that could not be had: %v", err)
	}

	// No search URL: offered, and not available.
	cfg, err = harnessOf(Options{ModelsKey: "k"})(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	if res := searchOnce(t.Context(), cfg); res.Outcome != tools.OutcomeError || res.Content[0].Text != "Web search is not available on this server." {
		t.Fatalf("no service: %+v", res)
	}
	// An agent that does not name it is not offered it.
	other := newSession(t, st, reviewer)
	cfg, err = harnessOf(Options{SearchURL: svc.URL, ModelsKey: "k"})(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	if _, ok := cfg.Tools.Get(tools.NameWebSearch); ok {
		t.Fatal("web_search offered to an agent that does not name it")
	}
}
