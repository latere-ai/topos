// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/internal/config"
	cellamachine "latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	sessiondir "latere.ai/x/topos/session/dir"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
	"latere.ai/x/topos/test/stubs/srtstub"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// localKey is a P-256 key in the PEM PKCS#8 form of
// TOPOS_LOCAL_ISSUER_KEY.
func localKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// selfHosted is m over the identity of a self-hoster: the local issuer
// and the owner policy, with no identity provider, served under a base
// path.
func selfHosted(t *testing.T, m map[string]string) func(string) string {
	t.Helper()
	all := map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080/topos", "TOPOS_BASE_PATH": "/topos", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
	maps.Copy(all, m)
	return env(all)
}

func TestVersionFlagPrintsTheIdentityAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "toposd dev (") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"frobnicate"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), `unknown subcommand "frobnicate"`) {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRolesNotBuiltYetExitOneNamingTheirSpec(t *testing.T) {
	for role, spec := range pending {
		var errOut bytes.Buffer
		if code := run(t.Context(), []string{role}, env(nil), io.Discard, &errOut); code != 1 {
			t.Fatalf("%s: exit %d", role, code)
		}
		want := "toposd: " + role + " is not built yet; spec " + spec + " builds it\n"
		if errOut.String() != want {
			t.Fatalf("%s: stderr %q, want %q", role, errOut.String(), want)
		}
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"serve", "-no-such-flag"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestSubcommandSplitsAroundTheFirstBareWord(t *testing.T) {
	name, rest := subcommand([]string{"-a", "serve", "-b"})
	if name != "serve" || strings.Join(rest, " ") != "-a -b" {
		t.Fatalf("subcommand() = %q, %q", name, rest)
	}
	if name, rest := subcommand([]string{"-version"}); name != "" || len(rest) != 1 {
		t.Fatalf("subcommand() = %q, %q", name, rest)
	}
}

func TestBadConfigurationExitsOneWithOneLine(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"TOPOS_PUBLIC_ADDR": "nope"}), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "toposd: configuration: ") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q", got)
	}
}

func TestOccupiedAddressExitsOne(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	for _, tc := range []struct{ name, public, internal string }{
		{"public", ln.Addr().String(), "127.0.0.1:0"},
		{"internal", "127.0.0.1:0", ln.Addr().String()},
	} {
		var errOut bytes.Buffer
		code := run(t.Context(), nil, selfHosted(t, map[string]string{
			"TOPOS_PUBLIC_ADDR":   tc.public,
			"TOPOS_INTERNAL_ADDR": tc.internal,
		}), io.Discard, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "address already in use") {
			t.Fatalf("%s: exit %d, stderr %q", tc.name, code, errOut.String())
		}
	}
}

// syncBuffer lets the test read stdout while serve still writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// startServe runs serve on loopback ports with a short drain and returns
// the two base URLs and a stop function that cancels the context and
// returns the exit code.
func startServe(t *testing.T, vars map[string]string) (publicURL, internalURL string, stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	publicURL, internalURL, wait := serveOn(t, ctx, vars, 10*time.Millisecond)
	return publicURL, internalURL, func() int {
		cancel()
		return wait()
	}
}

// serveOn runs serve on loopback ports until ctx ends, with drain as the
// drain delay, and returns the two base URLs and a function that waits
// for the exit code.
func serveOn(t *testing.T, ctx context.Context, vars map[string]string, drain time.Duration) (publicURL, internalURL string, wait func() int) {
	t.Helper()
	oldDrain := drainDelay
	drainDelay = drain
	t.Cleanup(func() { drainDelay = oldDrain })

	dataDir := t.TempDir()
	var out syncBuffer
	var errOut bytes.Buffer
	codec := make(chan int, 1)
	go func() {
		all := map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0", "TOPOS_DATA_DIR": dataDir, "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
		maps.Copy(all, vars)
		codec <- run(ctx, nil, env(all), &out, &errOut)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			return "http://" + m[1], "http://" + m[2], func() int {
				select {
				case code := <-codec:
					return code
				case <-time.After(gracePeriod + 10*time.Second):
					t.Fatal("serve did not stop")
					return -1
				}
			}
		}
		select {
		case code := <-codec:
			t.Fatalf("serve exited %d before listening; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never reported its listeners; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServeAnswersTheProbesOnBothListenersAndStopsCleanly(t *testing.T) {
	publicURL, internalURL, stop := startServe(t, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t)})

	for _, base := range []string{publicURL, internalURL} {
		for _, p := range []string{"/livez", "/readyz"} {
			if code, body := get(t, base+p); code != 200 || body != "ok\n" {
				t.Errorf("GET %s%s = %d %q", base, p, code, body)
			}
		}
		if code, body := get(t, base+"/version"); code != 200 || !strings.Contains(body, `"version":"dev"`) {
			t.Errorf("GET %s/version = %d %q", base, code, body)
		}
	}
	if code, body := get(t, publicURL+"/"); code != 200 || !strings.HasPrefix(body, "toposd dev (") {
		t.Errorf("GET / = %d %q", code, body)
	}

	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestReadinessFailsOnceDrainingBegins(t *testing.T) {
	draining := make(chan struct{})
	check := notDraining(draining)
	if err := check(t.Context()); err != nil {
		t.Fatalf("before draining: %v", err)
	}
	close(draining)
	if err := check(t.Context()); err == nil || err.Error() != "shutting down" {
		t.Fatalf("after draining: %v", err)
	}
}

func TestSleepCtxReturnsEarlyWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Minute)
	if time.Since(start) > time.Second {
		t.Fatal("sleepCtx waited for the timer despite a canceled context")
	}
}

// TestTokenRoleRoundTrip: toposd token prints a token the same toposd
// accepts, refuses a lifetime over a day, and opens no store: a database
// URL nothing could reach does not stop it.
func TestTokenRoleRoundTrip(t *testing.T) {
	vars := map[string]string{"TOPOS_PUBLIC_URL": "https://topos.example", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DB_URL": "postgres://nobody@192.0.2.1/none", "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"token", "--subject", "root", "--ttl", "2h"}, env(vars), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	cfg, err := config.Load(config.RoleServe, env(vars))
	if err != nil {
		t.Fatal(err)
	}
	id, err := newIdentity(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := id.verifier.Verify(strings.TrimSpace(out.String()))
	if err != nil || c.Subject != "https://topos.example|root" {
		t.Fatalf("the serving toposd read %+v, %v", c, err)
	}
	for _, args := range [][]string{{"token", "--ttl", "25h"}, {"token", "--subject", "a|b"}, {"token", "extra"}, {"token", "--no-such-flag"}} {
		errOut.Reset()
		if code := run(t.Context(), args, env(vars), io.Discard, &errOut); code != 2 {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut.String())
		}
	}
	errOut.Reset()
	if code := run(t.Context(), []string{"token"}, env(nil), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "TOPOS_LOCAL_ISSUER_KEY is required") {
		t.Fatalf("no key: stderr %q", errOut.String())
	}
}

// TestServePublishesTheLocalKeySet: with the local issuer on, the key
// set is served under the path of TOPOS_PUBLIC_URL, which is the base
// path.
func TestServePublishesTheLocalKeySet(t *testing.T) {
	publicURL, _, stop := startServe(t, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080/topos", "TOPOS_BASE_PATH": "/topos", "TOPOS_LOCAL_ISSUER_KEY": localKey(t)})
	code, body := get(t, publicURL+"/topos/.well-known/jwks.json")
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &set) != nil || len(set.Keys) != 1 || set.Keys[0].Alg != "ES256" {
		t.Fatalf("GET jwks.json = %d %q", code, body)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestBasePathReplacesTheRoot is spec 030's mount: with TOPOS_BASE_PATH
// set to a capability prefix, every route answers under it in the place
// of /v1, so the agent collection is /v1/agents/agents; the version
// segment is not repeated, a path outside the base is not_found, and the
// probes, the build identity and the key set stay where they were.
func TestBasePathReplacesTheRoot(t *testing.T) {
	vars := map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080/v1/agents", "TOPOS_BASE_PATH": "/v1/agents", "TOPOS_LOCAL_ISSUER_KEY": localKey(t)}
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	bearer := strings.TrimSpace(tok.String())
	publicURL, internalURL, stop := startServe(t, vars)
	send := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, publicURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	if code, body := send("/v1/agents/agents"); code != http.StatusOK || !strings.Contains(body, `"items":[]`) {
		t.Errorf("GET /v1/agents/agents = %d %s", code, body)
	}
	for _, p := range []string{"/v1/agents/v1/agents", "/v1/agents", "/v1/sessions", "/agents"} {
		if code, body := send(p); code != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
			t.Errorf("GET %s = %d %s, want not_found", p, code, body)
		}
	}
	if code, body := get(t, publicURL+"/v1/agents/openapi.yaml"); code != http.StatusOK || !strings.Contains(body, "url: http://127.0.0.1:8080/v1/agents\n") {
		t.Errorf("GET /v1/agents/openapi.yaml = %d, servers not the public URL", code)
	}
	if code, _ := get(t, publicURL+"/v1/agents/.well-known/jwks.json"); code != http.StatusOK {
		t.Errorf("GET the key set = %d", code)
	}
	for _, base := range []string{publicURL, internalURL} {
		if code, body := get(t, base+"/livez"); code != http.StatusOK || body != "ok\n" {
			t.Errorf("GET %s/livez = %d %q", base, code, body)
		}
	}
	if code, body := get(t, publicURL+"/"); code != http.StatusOK || !strings.HasPrefix(body, "toposd") {
		t.Errorf("GET / = %d %q", code, body)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestServeStopsOnAnIssuerThatDoesNotAnswer is the start-up rule: a
// listed issuer whose key set cannot be read is a deployment to fix.
func TestServeStopsOnAnIssuerThatDoesNotAnswer(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{
		"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0",
		"TOPOS_PUBLIC_URL": "https://topos.example", "TOPOS_OIDC_ISSUERS": gone.URL, "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_MODELS_URL": "https://lux.example/anthropic",
	}), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "TOPOS_OIDC_ISSUERS") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

// TestServeAnswersTheAPIAsASelfHoster: with the local issuer, the owner
// policy and a data directory, a token toposd token prints applies an
// agent and starts a session of it, and a second serve on the same data
// directory is refused.
func TestServeAnswersTheAPIAsASelfHoster(t *testing.T) {
	key, data := localKey(t), t.TempDir()
	vars := map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": key, "TOPOS_DATA_DIR": data, "TOPOS_ADMIN_SUBJECTS": "http://127.0.0.1:8080|admin", "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	publicURL, internalURL, stop := startServe(t, vars)
	bearer := strings.TrimSpace(tok.String())
	send := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, publicURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: claude-haiku-4-5}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/reviewer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"reviewer","message":"Review main.go."}`)
	if code != http.StatusCreated || !strings.Contains(body, `"initiator":{"subject":"http://127.0.0.1:8080|admin"`) {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := get(t, internalURL+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("readiness with the store: %d %q", code, body)
	}
	// The summary counts the one session, whatever its runner made of it,
	// from the data directory.
	code, body = send(http.MethodGet, "/v1/sessions/summary", "")
	var sum session.Summary
	if err := json.Unmarshal([]byte(body), &sum); code != http.StatusOK || err != nil ||
		sum.Sessions.Running+sum.Sessions.WaitingForApproval+sum.Sessions.Idle+sum.Sessions.Ended != 1 || sum.Agents != 1 {
		t.Fatalf("summary: %d %s (%v)", code, body, err)
	}
	if code, _ := get(t, publicURL+"/v1/openapi.yaml"); code != 200 {
		t.Fatalf("openapi: %d", code)
	}
	var errOut bytes.Buffer
	second := map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0"}
	maps.Copy(second, vars)
	if code := run(t.Context(), nil, env(second), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "data directory in use by pid") {
		t.Fatalf("a second serve: exit %d, stderr %q", code, errOut.String())
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestServeStopsOnAStoreItCannotOpen: a database that does not answer
// and a data directory that cannot be made each stop the start with the
// variable named.
func TestServeStopsOnAStoreItCannotOpen(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for want, extra := range map[string]map[string]string{
		"TOPOS_DB_URL":   {"TOPOS_DB_URL": "postgres://nobody@127.0.0.1:99999/none"},
		"TOPOS_DATA_DIR": {"TOPOS_DATA_DIR": filepath.Join(blocked, "data")},
	} {
		vars := map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0"}
		maps.Copy(vars, extra)
		var errOut bytes.Buffer
		if code := run(t.Context(), nil, selfHosted(t, vars), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), want) {
			t.Errorf("%s: exit %d, stderr %q", want, code, errOut.String())
		}
	}
}

// glob is a reply that calls glob, a tool that acts on the machine, so
// the session's machine is opened on demand.
var glob = luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_glob", Name: "glob", Args: json.RawMessage(`{"pattern":"*.go"}`)}}}, StopReason: ir.StopToolUse}}

// hostedStubs are a stub Lux that answers with replies, or when none is
// given calls glob, which opens the session's machine, and then answers
// "Reviewed.", and a stub Cella that wants a bearer,
// with the helper built for this machine, and the variables that point a
// role at them.
func hostedStubs(t *testing.T, replies ...luxstub.Reply) (map[string]string, *luxstub.Server, *cellastub.Server) {
	t.Helper()
	helpers := t.TempDir()
	out := filepath.Join(helpers, "topos-machine-"+runtime.GOOS+"-"+runtime.GOARCH)
	build := exec.CommandContext(t.Context(), "go", "build", "-o", out, "latere.ai/x/topos/cmd/topos-machine")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the helper: %v\n%s", err, b)
	}
	cella := cellastub.New(t)
	cella.RequireToken("cella-bearer")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("cella-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lux := luxstub.New(t)
	if len(replies) == 0 {
		replies = []luxstub.Reply{glob, {Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: "Reviewed."}}, StopReason: ir.StopEndTurn}}}
	}
	lux.Script("anthropic/claude-haiku-4.5", replies...)
	machineDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"TOPOS_MODELS_URL": lux.URL() + "/anthropic", "TOPOS_MODELS_KEY": "model-key",
		"TOPOS_CELLA_URL": cella.URL(), "TOPOS_CELLA_TOKEN_FILE": tokenFile,
		"TOPOS_MACHINE_HELPERS": helpers, "TOPOS_MACHINE_DIR": machineDir,
	}, lux, cella
}

// hostedSession applies a Cella agent and creates a session of it over
// the API with the local issuer's token, waits until its turn answered,
// and returns the session's events.
func hostedSession(t *testing.T, publicURL string, vars map[string]string) string {
	t.Helper()
	return runSession(t, publicURL, vars, "cella")
}

// runSession creates a session of an agent whose machine is of kind,
// waits until its turn answered, and returns the session's events.
func runSession(t *testing.T, publicURL string, vars map[string]string, kind string) string {
	t.Helper()
	send, id := createHostedSession(t, publicURL, vars, kind)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
		if strings.Contains(body, `"status":"idle"`) && strings.Contains(body, `"stop_reason":"end_turn"`) {
			break
		}
		if time.Now().After(deadline) {
			_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
			t.Fatalf("the session never answered: %s\nevents %s", body, events)
		}
	}
	_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
	for _, want := range []string{`"type":"session.machine"`, `"kind":"` + kind + `"`, `"text":"Reviewed."`} {
		if !strings.Contains(events, want) {
			t.Errorf("the log lacks %s: %s", want, events)
		}
	}
	return events
}

// apiClient sends requests to publicURL with a token the local issuer of
// vars signs.
func apiClient(t *testing.T, publicURL string, vars map[string]string) func(method, path, body string) (int, string) {
	t.Helper()
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	bearer := strings.TrimSpace(tok.String())
	return func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, publicURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
}

// createHostedSession applies an agent whose machine is of kind and
// creates a session of it over the API with the local issuer's token,
// and returns the function that sends as that token and the session's
// id.
func createHostedSession(t *testing.T, publicURL string, vars map[string]string, kind string) (func(method, path, body string) (int, string), string) {
	t.Helper()
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: " + kind + "}\n"
	if code, body := send(http.MethodPut, "/v1/agents/reviewer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"reviewer","message":"Review main.go."}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	return send, s.ID
}

// TestServeRunsAHostedSession: a session created over the API is claimed
// by serve's own runner, which opens its Cella sandbox, uploads the
// helper, asks the model with the installation's key and records the
// answer, with the Cella bearer read from its file.
func TestServeRunsAHostedSession(t *testing.T) {
	vars, lux, cella := hostedStubs(t)
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "2"})
	publicURL, _, stop := startServe(t, vars)
	events := hostedSession(t, publicURL, vars)
	if !strings.Contains(events, `"kind":"serve"`) {
		t.Errorf("the session ran on no serve runner: %s", events)
	}
	reqs := lux.Requests()
	if len(reqs) != 2 {
		t.Errorf("the model was asked %d times", len(reqs))
	}
	for _, r := range reqs {
		if r.Header.Get("X-Api-Key") != "model-key" && r.Header.Get("Authorization") != "Bearer model-key" {
			t.Errorf("the model was asked with %v", r.Header)
		}
	}
	if n := cella.Count(cellastub.OpCreate); n != 1 {
		t.Errorf("%d sandboxes created", n)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestTheRunnerRoleRunsAHostedSession: serve with no runner of its own
// and the runner routes mounted, and a runner role claiming from its
// internal listener, run a session created over the API; a runner with
// a token serve does not hold claims nothing.
func TestTheRunnerRoleRunsAHostedSession(t *testing.T) {
	vars, _, _ := hostedStubs(t)
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(),
		"TOPOS_RUNNER_CAPACITY": "0", "TOPOS_RUNNER_TOKEN": "runner-token"})
	publicURL, internalURL, stop := startServe(t, vars)
	runnerVars := maps.Clone(vars)
	maps.Copy(runnerVars, map[string]string{"TOPOS_INTERNAL_URL": internalURL, "TOPOS_INTERNAL_ADDR": "127.0.0.1:0", "TOPOS_RUNNER_CAPACITY": "1"})
	ctx, cancel := context.WithCancel(t.Context())
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"runner"}, env(runnerVars), &out, &errOut) }()
	events := hostedSession(t, publicURL, vars)
	if !strings.Contains(events, `"kind":"runner"`) {
		t.Errorf("the session ran on no runner role: %s", events)
	}
	cancel()
	if code := <-done; code != 0 || !strings.Contains(out.String(), "runner claiming from "+internalURL) {
		t.Fatalf("the runner role exited %d: %s %s", code, out.String(), errOut.String())
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
	var usage bytes.Buffer
	if code := run(t.Context(), []string{"runner"}, env(nil), io.Discard, &usage); code != 1 || !strings.Contains(usage.String(), "TOPOS_INTERNAL_URL") {
		t.Fatalf("a runner with no configuration: %d %q", code, usage.String())
	}
	if code := run(t.Context(), []string{"runner", "-no-such-flag"}, env(nil), io.Discard, io.Discard); code != 2 {
		t.Fatalf("a bad flag: %d", code)
	}
}

// TestServeStartsNoRunnerOrRefusesMissingHelpers: a capacity of zero
// serves without runners, and a Cella URL with no helper builds stops
// the start naming TOPOS_MACHINE_HELPERS.
func TestServeStartsNoRunnerOrRefusesMissingHelpers(t *testing.T) {
	_, _, stop := startServe(t, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_RUNNER_CAPACITY": "0"})
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	vars := map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0",
		"TOPOS_CELLA_URL": "http://127.0.0.1:1", "TOPOS_CELLA_TOKEN_FILE": tokenFile, "TOPOS_MACHINE_HELPERS": t.TempDir()}
	if code := run(t.Context(), nil, selfHosted(t, vars), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "TOPOS_MACHINE_HELPERS") {
		t.Fatalf("no helpers: exit %d, stderr %q", code, errOut.String())
	}
}

// TestTheRunnerRoleStopsOnWhatItCannotStart: an internal address in use
// and a Cella URL with no helper builds each stop the runner role.
func TestTheRunnerRoleStopsOnWhatItCannotStart(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	base := map[string]string{"TOPOS_INTERNAL_URL": "http://127.0.0.1:1", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example/anthropic", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0"}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	for want, extra := range map[string]map[string]string{
		"TOPOS_INTERNAL_ADDR":   {"TOPOS_INTERNAL_ADDR": ln.Addr().String()},
		"TOPOS_MACHINE_HELPERS": {"TOPOS_CELLA_URL": "http://127.0.0.1:1", "TOPOS_CELLA_TOKEN_FILE": tokenFile, "TOPOS_MACHINE_HELPERS": t.TempDir()},
	} {
		vars := maps.Clone(base)
		maps.Copy(vars, extra)
		var errOut bytes.Buffer
		if code := run(t.Context(), []string{"runner"}, env(vars), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), want) {
			t.Errorf("%s: exit %d, stderr %q", want, code, errOut.String())
		}
	}
}

// TestSigtermLeavesARunningSessionToTheNextClaim is spec 002's shutdown
// with spec 016's served session: on SIGTERM readiness answers 503 while
// the drain delay runs, the runner stops the turn it was in, the session
// stays running with nothing appended after the stop, its lease is
// released for the next runner's claim, and the process exits 0.
func TestSigtermLeavesARunningSessionToTheNextClaim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a process cannot send itself SIGTERM on Windows")
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	vars, _, _ := hostedStubs(t, luxstub.Reply{Respond: func(*ir.Request, *ir.Response) {
		once.Do(func() { close(started) })
		<-release
	}})
	t.Cleanup(func() { close(release) })
	data := t.TempDir()
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": data, "TOPOS_RUNNER_CAPACITY": "1"})

	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	publicURL, internalURL, wait := serveOn(t, ctx, vars, time.Second)
	send, id := createHostedSession(t, publicURL, vars, "cella")
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the runner never asked the model")
	}
	var before session.Session
	if code, body := send(http.MethodGet, "/v1/sessions/"+id, ""); code != http.StatusOK || json.Unmarshal([]byte(body), &before) != nil || before.Status != session.StatusRunning {
		t.Fatalf("the session mid-turn: %d %s", code, body)
	}

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		code, body := get(t, internalURL+"/readyz")
		if code == http.StatusServiceUnavailable {
			if body != "not ready: draining: shutting down\n" {
				t.Fatalf("readiness while draining: %q", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness still %d %q after SIGTERM", code, body)
		}
	}
	if code := wait(); code != 0 {
		t.Fatalf("exit %d", code)
	}

	st, err := sessiondir.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	after, err := st.Get(t.Context(), id)
	if err != nil || after.Status != session.StatusRunning || after.LastSeq != before.LastSeq {
		t.Fatalf("after the stop %+v (last %d before), %v", after, before.LastSeq, err)
	}
	claims, err := runner.NewQueue(st, time.Hour).Claim(t.Context(), session.Holder{Runner: "run_next"}, 1, 0)
	if err != nil || len(claims) != 1 || claims[0].ID != id {
		t.Fatalf("the next runner's claim: %+v, %v", claims, err)
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestServeRefusesAHostSessionWithoutTheSwitch: without
// TOPOS_HOST_SESSIONS=on a session of an agent whose machine is the host
// is refused machine_unavailable, and nothing runs on the server's host.
func TestServeRefusesAHostSessionWithoutTheSwitch(t *testing.T) {
	vars := map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_RUNNER_CAPACITY": "0"}
	publicURL, _, stop := startServe(t, vars)
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	send := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, publicURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok.String()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: local\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: host}\n"
	if code, body := send(http.MethodPut, "/v1/agents/local", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	if code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"local","message":"Hi."}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"machine_unavailable"`) {
		t.Fatalf("a host session: %d %s", code, body)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestHostSessionsOnAServer: with TOPOS_HOST_SESSIONS=on, serve and the
// runner role refuse to start when the host sandbox's preflight fails,
// when the sandbox cannot run a command, and when it lets a command read
// the data directory; with srt, serve runs a host session in a directory
// of its own inside the sandbox and records the sandbox driver.
func TestHostSessionsOnAServer(t *testing.T) {
	t.Run("refuses a sandbox that does not hold", testHostSessionsRefused)
	t.Run("runs a session inside srt", testAHostSessionRuns)
}

func testHostSessionsRefused(t *testing.T) {
	system := "/usr/bin:/bin"
	for name, c := range map[string]struct{ path, want string }{
		"no srt":         {t.TempDir(), "srt: missing"},
		"a broken srt":   {srtstub.Bin(t, srtstub.Broken) + ":" + system, "could not run a probe command"},
		"no confinement": {srtstub.Bin(t, srtstub.Unconfined) + ":" + system, "does not confine commands on this host"},
	} {
		host := map[string]string{"TOPOS_HOST_SESSIONS": "on", "PATH": c.path, "HOME": t.TempDir()}
		var errOut bytes.Buffer
		serveVars := maps.Clone(host)
		maps.Copy(serveVars, map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0"})
		if code := run(t.Context(), nil, selfHosted(t, serveVars), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "TOPOS_HOST_SESSIONS") || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("serve, %s: exit %d, stderr %q", name, code, errOut.String())
		}
		runnerVars := maps.Clone(host)
		maps.Copy(runnerVars, map[string]string{"TOPOS_INTERNAL_URL": "http://127.0.0.1:1", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_MODELS_URL": "https://lux.example/anthropic", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0", "TOPOS_DATA_DIR": t.TempDir()})
		errOut.Reset()
		if code := run(t.Context(), []string{"runner"}, env(runnerVars), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("the runner role, %s: exit %d, stderr %q", name, code, errOut.String())
		}
	}
	var errOut bytes.Buffer
	if code := run(t.Context(), nil, selfHosted(t, map[string]string{"TOPOS_HOST_SESSIONS": "on", "HOME": "/", "TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0"}), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "HOME") {
		t.Errorf("a server whose home is the root: exit %d, stderr %q", code, errOut.String())
	}
}

func testAHostSessionRuns(t *testing.T) {
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt, the host sandbox's runtime, is not on PATH; running a host session inside it needs it")
	}
	vars, _, _ := hostedStubs(t)
	data := t.TempDir()
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": data, "TOPOS_RUNNER_CAPACITY": "1",
		"TOPOS_HOST_SESSIONS": "on", "PATH": filepath.Dir(srt) + ":" + os.Getenv("PATH"), "HOME": os.Getenv("HOME")})
	publicURL, _, stop := startServe(t, vars)
	events := runSession(t, publicURL, vars, "host")
	resolved, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events, `"sandbox":"host"`) || !strings.Contains(events, `"workdir":"`+filepath.Join(resolved, "host-sessions", "ses_")) {
		t.Errorf("the session ran outside a host session's sandbox: %s", events)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestServeRefusesScriptedModel is invariant 9 of spec 001 for the
// server roles: serve and runner stop with one configuration line and
// exit 1 when no model connection is named, and when the one named is
// the scripted model, which answers from a script and never from a
// model.
func TestServeRefusesScriptedModel(t *testing.T) {
	runnerEnv := map[string]string{"TOPOS_INTERNAL_URL": "http://127.0.0.1:1", "TOPOS_RUNNER_TOKEN": "t", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0"}
	for name, url := range map[string]string{"no connection": "", "the scripted model": "scripted:/tmp/script.yaml"} {
		for _, role := range []string{"serve", "runner"} {
			vars := map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0", "TOPOS_MODELS_URL": url}
			getenv := selfHosted(t, vars)
			if role == "runner" {
				maps.Copy(runnerEnv, map[string]string{"TOPOS_MODELS_URL": url})
				getenv = env(runnerEnv)
			}
			var errOut bytes.Buffer
			code := run(t.Context(), []string{role}, getenv, io.Discard, &errOut)
			if got := errOut.String(); code != 1 || !strings.HasPrefix(got, "toposd: configuration: ") || !strings.Contains(got, "TOPOS_MODELS_URL") || strings.Count(got, "\n") != 1 {
				t.Errorf("%s, %s: exit %d, stderr %q", role, name, code, got)
			}
		}
	}
}

// TestServeFindsTheFamilysDoorFromLuxsRoot: with TOPOS_MODELS_URL naming
// a Lux root, serve's runner sends a hosted session's request to the
// family's door the discovery document names; a model URL that does not
// answer stops the start naming the variable.
func TestServeFindsTheFamilysDoorFromLuxsRoot(t *testing.T) {
	vars, lux, _ := hostedStubs(t)
	maps.Copy(vars, map[string]string{"TOPOS_MODELS_URL": lux.URL(), "TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	hostedSession(t, publicURL, vars)
	if reqs := lux.Requests(); len(reqs) != 2 || reqs[0].Dialect != ir.DialectAnthropicMessages || reqs[1].Dialect != ir.DialectAnthropicMessages {
		t.Errorf("the model was asked %+v", reqs)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	var errOut bytes.Buffer
	if code := run(t.Context(), nil, selfHosted(t, map[string]string{"TOPOS_PUBLIC_ADDR": "127.0.0.1:0", "TOPOS_INTERNAL_ADDR": "127.0.0.1:0", "TOPOS_MODELS_URL": gone.URL}), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "TOPOS_MODELS_URL") {
		t.Fatalf("a model URL that does not answer: exit %d, stderr %q", code, errOut.String())
	}
}

// TestCellaMachineSurvivesRunnerRestart: a serve stopped mid-turn, after
// a tool opened the session's sandbox, leaves its session running; a
// serve started again on the same data directory claims it, and when the
// resumed turn calls a tool on the machine it finds the session's sandbox
// by name, starts it since Cella stopped it meanwhile, and finishes the
// turn without creating another.
func TestCellaMachineSurvivesRunnerRestart(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	vars, _, cella := hostedStubs(t, glob,
		luxstub.Reply{Respond: func(*ir.Request, *ir.Response) {
			once.Do(func() { close(started) })
			<-release
		}},
		luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_glob_again", Name: "glob", Args: json.RawMessage(`{"pattern":"*.go"}`)}}}, StopReason: ir.StopToolUse}},
		luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: "Reviewed."}}, StopReason: ir.StopEndTurn}},
	)
	t.Cleanup(func() { close(release) })
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	_, id := createHostedSession(t, publicURL, vars, "cella")
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first serve never asked the model")
	}
	if code := stop(); code != 0 {
		t.Fatalf("the first serve exited %d", code)
	}
	if !cella.Stop(cellamachine.SandboxName(id)) {
		t.Fatal("the session has no sandbox to stop")
	}
	publicURL, _, stop = startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
		if strings.Contains(body, `"status":"idle"`) && strings.Contains(body, `"stop_reason":"end_turn"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted serve never finished the turn: %s", body)
		}
	}
	if n := cella.Count(cellastub.OpCreate); n != 1 {
		t.Errorf("%d sandboxes created, want the first one found again", n)
	}
	if n := cella.Count(cellastub.OpStart); n < 1 {
		t.Error("the stopped sandbox was not started")
	}
	if code := stop(); code != 0 {
		t.Fatalf("the second serve exited %d", code)
	}
}

// TestServeKeepsBlobsWhereTheURLSays: with TOPOS_BLOB_URL naming a
// directory, a hosted session's raw response lands there, under the
// session, and not in the session's own directory.
func TestServeKeepsBlobsWhereTheURLSays(t *testing.T) {
	vars, _, _ := hostedStubs(t)
	outside, data := t.TempDir(), t.TempDir()
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": data, "TOPOS_RUNNER_CAPACITY": "1", "TOPOS_BLOB_URL": "file://" + outside})
	publicURL, _, stop := startServe(t, vars)
	events := hostedSession(t, publicURL, vars)
	var s struct {
		Items []struct {
			SessionID string `json:"session_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(events), &s); err != nil || len(s.Items) == 0 {
		t.Fatalf("the events %s: %v", events, err)
	}
	id := s.Items[0].SessionID
	bodies, err := os.ReadDir(filepath.Join(outside, id))
	if err != nil || len(bodies) == 0 {
		t.Fatalf("the session's bodies outside: %v, %v", bodies, err)
	}
	if kept, err := os.ReadDir(filepath.Join(data, "sessions", id, "blobs", "sha256")); err != nil || len(kept) != 0 {
		t.Fatalf("the session's own blob directory holds %v, %v", kept, err)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestEveryRunsUntilTheContextEnds: a pass of the reaper or of the
// minute loop runs on each tick, logs what it could not do under its
// name, and stops with its context.
func TestEveryRunsUntilTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var logged syncBuffer
	ran := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		every(ctx, time.Millisecond, "fire triggers", func(context.Context) error {
			ran <- struct{}{}
			return errors.New("the store is down")
		}, slog.New(slog.NewTextHandler(&logged, nil)))
	}()
	for range 2 {
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatal("the reaper never ran")
		}
	}
	cancel()
	<-done
	if !strings.Contains(logged.String(), "the store is down") || !strings.Contains(logged.String(), "fire triggers") {
		t.Fatalf("the log %q", logged.String())
	}
}
