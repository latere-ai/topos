// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// withConnectionService names the connection service of
// connections.example.
func withConnectionService(o *CellaOptions) {
	o.ConnectionsURL, o.ConnectionsAudience = "https://connections.example/v1", "connections"
}

// setConnections records cs as the session's connections, as a send's
// allow does.
func setConnections(t *testing.T, st session.Store, id string, cs ...session.Connection) {
	t.Helper()
	if cs == nil {
		cs = []session.Connection{}
	}
	e, err := session.NewEvent(session.TypeConnectionsChanged, session.ConnectionsChanged{Connections: cs}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{e}
	session.Stamp(id, s.LastSeq, evs)
	if _, err := st.Append(t.Context(), id, s.LastSeq, evs); err != nil {
		t.Fatal(err)
	}
}

// TestTheConnectionsSecret: every sandbox of an installation with a
// connection service mounts -connections as CONNECTIONS_TOKEN, whether or
// not the session has a connection: the session's token for the audience
// and workload sandbox, scoped to the service's host, injected as a
// bearer and labeled with the session and the agent. It is renewed before
// it expires and deleted at the session's end after the sandbox; an
// installation that mints nothing for the audience mounts none, and one
// that names no audience opens no sandbox (spec 069).
func TestTheConnectionsSecret(t *testing.T) {
	creds := &issued{life: runner.RefreshBefore + 500*time.Millisecond}
	f := newCredentialFixture(t, helper(t), creds, nil, withConnectionService)
	f.cella.Authorize(sessionRule(f.s.ID))
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	cfg, err := open(ctx, f.h, f.s)
	if err != nil {
		t.Fatal(err)
	}
	name := cella.SandboxName(f.s.ID)
	secret := name + SecretConnections
	sec, value, ok := f.cella.Secret(secret)
	if !ok || value != "connections-sandbox-1" || !slices.Equal(sec.Spec.Scope.Hosts, []string{"connections.example"}) ||
		sec.Spec.Inject != (cellav1.SecretInject{Header: "Authorization", Scheme: cellav1.SchemeBearer}) {
		t.Fatalf("the connection service's Secret %+v %q %v", sec.Spec, value, ok)
	}
	if want := map[string]string{cella.LabelSession: f.s.ID, cella.LabelAgent: "reviewer"}; !maps.Equal(sec.Metadata.Labels, want) {
		t.Fatalf("it is labeled %v", sec.Metadata.Labels)
	}
	sb, ok := f.cella.Sandbox(name)
	if !ok || !slices.Contains(sb.Spec.Secrets, cellav1.SecretMount{Name: secret, Env: session.ConnectionsToken}) {
		t.Fatalf("the sandbox mounts %+v", sb.Spec.Secrets)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if sec, value, _ := f.cella.Secret(secret); sec.Status.Version >= 2 && value != "connections-sandbox-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection service's token was not renewed before it expired")
		}
	}
	if !slices.Contains(creds.calls, "connections/sandbox") {
		t.Fatalf("the tokens asked %v", creds.calls)
	}
	if err := cfg.Machine.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.cella.Secret(secret); ok {
		t.Fatal("the connection service's Secret outlived the session")
	}
	if _, ok := f.cella.Sandbox(name); ok {
		t.Fatal("the sandbox outlived the session")
	}

	nothing := &issued{life: 15 * time.Minute, err: map[string]error{"connections": runner.ErrNotMinted}}
	g := newCredentialFixture(t, helper(t), nothing, nil, withConnectionService)
	gctx := runner.WithTokens(t.Context(), runner.NewTokenSource(nothing, nil, nil))
	if err := opened(gctx, g.h, g.s); err != nil {
		t.Fatal(err)
	}
	if n := g.cella.Count(cellastub.OpSecret); n == 0 {
		t.Fatal("the other Secrets were not applied")
	}
	if _, _, ok := g.cella.Secret(cella.SandboxName(g.s.ID) + SecretConnections); ok {
		t.Fatal("an audience nothing mints for was mounted")
	}

	bare := newCredentialFixture(t, helper(t), creds, nil, func(o *CellaOptions) { o.ConnectionsURL = "https://connections.example" })
	if err := opened(ctx, bare.h, bare.s); err == nil || !strings.Contains(err.Error(), "no audience") {
		t.Fatalf("a connection service with no audience: %v", err)
	}
	broken := newCredentialFixture(t, helper(t), creds, nil, func(o *CellaOptions) { o.ConnectionsURL, o.ConnectionsAudience = "https://", "connections" })
	if err := opened(ctx, broken.h, broken.s); err == nil || !strings.Contains(err.Error(), "TOPOS_CONNECTIONS_URL") {
		t.Fatalf("a connection service with no host: %v", err)
	}
}

// TestHTTPConnectionsFollowTheList: a sandbox takes the session's http
// connections when it opens: git rewrites each git URL to its
// connection's git address and sends the connection service's
// placeholder there, and each command carries each connection's
// forwarding address under its variable. A later list replaces it whole,
// the same list runs nothing in the sandbox, and a sandbox that mounts no
// Secret of the connection service takes none (spec 069).
func TestHTTPConnectionsFollowTheList(t *testing.T) {
	creds := &issued{life: 15 * time.Minute}
	f := newCredentialFixture(t, helper(t), creds, nil, withConnectionService)
	setConnections(t, f.st, f.s.ID, codeHostConnection("https://connections.example"), calendarConnection)
	ctx, cancel := context.WithCancel(runner.WithTokens(t.Context(), runner.NewTokenSource(creds, nil, nil)))
	defer cancel()
	cfg, err := open(ctx, f.h, f.s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
			t.Error(err)
		}
	}()
	placeholder := "cella-placeholder-" + cella.SandboxName(f.s.ID) + SecretConnections
	const show = `git config --get-all url.https://connections.example/v1/connections/c_1/git/.insteadof; ` +
		`git config --get http.https://connections.example/v1/connections/c_1/git/.extraheader; ` +
		`echo "code=$CODE_HOST_API_URL calendar=$CALENDAR_API_URL"`
	run := func() string {
		t.Helper()
		res, err := cfg.Machine.Exec(ctx, machine.ExecRequest{Command: show, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return string(res.Output)
	}
	want := "https://git.example.com/\nAuthorization: Bearer " + placeholder + "\n" +
		"code=https://connections.example/v1/connections/c_1/http calendar=https://connections.example/v1/connections/c_9/http\n"
	if got := run(); got != want {
		t.Fatalf("the sandbox at its open:\n%s\nwant\n%s", got, want)
	}
	cm, ok := cfg.Machine.(machine.Connected)
	if !ok {
		t.Fatal("the session's machine is not connected")
	}
	// A request's own variable wins over a connection's.
	res, err := cfg.Machine.Exec(ctx, machine.ExecRequest{Command: `echo "$CALENDAR_API_URL"`, Env: map[string]string{"CALENDAR_API_URL": "mine"}, Timeout: time.Minute})
	if err != nil || string(res.Output) != "mine\n" {
		t.Fatalf("a request's own variable: %q %v", res.Output, err)
	}

	calendar := []machine.HTTPConnection{{Env: "CALENDAR_API_URL", Address: calendarConnection.Address}}
	if err := cm.ApplyConnections(ctx, calendar); err != nil {
		t.Fatal(err)
	}
	if got, want := run(), "code= calendar=https://connections.example/v1/connections/c_9/http\n"; got != want {
		t.Fatalf("the sandbox after the list changed:\n%q, want %q", got, want)
	}
	before := f.cella.Count(cellastub.OpExec)
	if err := cm.ApplyConnections(ctx, calendar); err != nil || f.cella.Count(cellastub.OpExec) != before {
		t.Fatalf("the same list ran %d commands, %v", f.cella.Count(cellastub.OpExec)-before, err)
	}

	plain := newCredentialFixture(t, helper(t), creds, nil)
	setConnections(t, plain.st, plain.s.ID, codeHostConnection("https://connections.example"))
	pcfg, err := open(ctx, plain.h, plain.s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pcfg.Machine.Release(context.WithoutCancel(ctx), true); err != nil {
			t.Error(err)
		}
	}()
	res, err = pcfg.Machine.Exec(ctx, machine.ExecRequest{Command: `git config --get-all include.path; echo "code=$CODE_HOST_API_URL"`, Timeout: time.Minute})
	if err != nil || string(res.Output) != "code=\n" {
		t.Fatalf("a sandbox with no connection service: %q %v", res.Output, err)
	}
}

// codeHostConnection is an http connection of the connection service at
// base, with a git address, and calendarConnection one without.
func codeHostConnection(base string) session.Connection {
	return session.Connection{
		Name: "code-host", Kind: session.ConnectionHTTP, Title: "Code host", Audience: "connections",
		Address: base + "/v1/connections/c_1/http", GitAddress: base + "/v1/connections/c_1/git", GitURL: "https://git.example.com",
	}
}

var calendarConnection = session.Connection{
	Name: "calendar", Kind: session.ConnectionHTTP, Title: "Calendar", Audience: "connections",
	Address: "https://connections.example/v1/connections/c_9/http",
}

// connectionService serves a stub connection service's addresses of one
// connection: git's smart HTTP under /v1/connections/c_1/git over the
// bare repositories under root, and /v1/connections/c_1/http/user, each
// to a request whose bearer is the placeholder want answers alone, and
// records every Authorization it saw.
type connectionService struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newConnectionService(t *testing.T, root string, want func() string) *connectionService {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed, and the stub git address is git's own http backend")
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	cs := &connectionService{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		cs.mu.Lock()
		cs.seen = append(cs.seen, auth)
		cs.mu.Unlock()
		if auth != "Bearer "+want() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_token","message":"Sign in again."}}`))
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/connections/c_1/git/"):
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v1/connections/c_1/git")
			backend.ServeHTTP(w, r)
		case r.URL.Path == "/v1/connections/c_1/http/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"login":"ada"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

// TestASandboxClonesThroughAConnection: a session whose allow named an
// http connection with a git address clones a repository by its usual
// URL from bash, and pushes its own branch, through a stub git address
// that admits only the placeholder of the sandbox's -connections Secret;
// it calls the connection's API at its variable with the same bearer;
// and no command, event or Secret's placeholder carries the token (spec
// 069).
func TestASandboxClonesThroughAConnection(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "acme", "app")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitIn(t, bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "--quiet", "-m", "Start")
	gitIn(t, seed, "push", "--quiet", "origin", "HEAD:main")

	var name string
	svc := newConnectionService(t, root, func() string { return "cella-placeholder-" + name + SecretConnections })
	c := newCloud(t, func(s *session.Session) { s.Connections = []session.Connection{codeHostConnection(svc.srv.URL)} })
	name = cella.SandboxName(c.s.ID)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o.Machines = Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir,
		ConnectionsURL: svc.srv.URL, ConnectionsAudience: "connections"})
	c.creds = &issued{life: 15 * time.Minute, err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}
	branch := session.Branch(c.s)
	command := `git clone -q https://git.example.com/acme/app && cd app && cat README.md && ` +
		`git -c user.name=Ada -c user.email=ada@example.com commit -q --allow-empty -m "Work" && git push -q origin HEAD:refs/heads/` + branch + ` && echo pushed`
	steps := []string{command}
	if _, err := exec.LookPath("curl"); err == nil {
		steps = append(steps, `curl -sf -H "Authorization: Bearer $CONNECTIONS_TOKEN" "$CODE_HOST_API_URL/user"`)
	}
	for i, step := range steps {
		c.drive("Clone the app and push a branch.", bash("toolu_"+string(rune('1'+i)), step), said("Done."))
	}
	got := c.results()
	if !strings.Contains(got[0], "app\n") || !strings.Contains(got[0], "pushed") {
		t.Fatalf("the clone answered %q", got[0])
	}
	if len(steps) == 2 && !strings.HasPrefix(got[1], `{"login":"ada"}`) {
		t.Fatalf("the API answered %q", got[1])
	}
	if head := gitIn(t, bare, "log", "-1", "--format=%s", "refs/heads/"+branch); head != "Work" {
		t.Fatalf("the pushed branch holds %q", head)
	}
	svc.mu.Lock()
	seen := slices.Clone(svc.seen)
	svc.mu.Unlock()
	placeholder := "Bearer cella-placeholder-" + name + SecretConnections
	if len(seen) == 0 || slices.ContainsFunc(seen, func(a string) bool { return a != placeholder }) {
		t.Fatalf("the connection service saw %q, want the placeholder alone", seen)
	}
	_, value, ok := c.cella.Secret(name + SecretConnections)
	if !ok || !strings.HasPrefix(value, "connections-sandbox-") {
		t.Fatalf("the Secret holds %q, %v", value, ok)
	}
	if strings.Contains(c.dump(), value) {
		t.Fatal("an event carries the connection service's token")
	}
}
