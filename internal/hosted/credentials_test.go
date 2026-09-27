// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
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

// helper builds the topos-machine helper for the machine the tests run
// on.
func helper(t *testing.T) map[string][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "topos-machine")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "latere.ai/x/topos/cmd/topos-machine")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the helper: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{runtime.GOOS + "/" + runtime.GOARCH: b}
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

func newCredentialFixture(t *testing.T, helpers map[string][]byte, creds *issued, file client.TokenSource) credentialFixture {
	t.Helper()
	f := credentialFixture{lux: luxstub.New(t), cella: cellastub.New(t), st: session.NewMemoryStore(), creds: creds}
	f.lux.Script("anthropic/claude-haiku-4.5", luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: "ok"}}, StopReason: ir.StopEndTurn}})
	f.s = newSession(t, f.st, reviewer)
	f.s.ExpiresAt = time.Now().Add(time.Hour)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	machines := Cella(CellaOptions{URL: f.cella.URL(), Token: file, Helpers: helpers, Dir: dir, ModelsURL: f.lux.URL(), OrigoURL: "https://origo.example"})
	if f.h, err = Harness(Options{Store: f.st, ModelsURL: f.lux.URL() + "/anthropic", ModelsKey: "installation-key", Machines: machines}); err != nil {
		t.Fatal(err)
	}
	return f
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
	if _, err := g.h(ctx, g.s); code(t, err) != CodeMachineUnavailable {
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
		if _, err := f.h(ctx, f.s); err == nil || code(t, err) != c.code {
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
