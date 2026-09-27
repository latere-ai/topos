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
	"io"
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
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	sessiondir "latere.ai/x/topos/session/dir"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
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
// and the owner policy, with no identity provider.
func selfHosted(t *testing.T, m map[string]string) func(string) string {
	t.Helper()
	all := map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080/topos", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_MODELS_URL": "https://lux.example/anthropic"}
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
// set is served under the path of TOPOS_PUBLIC_URL.
func TestServePublishesTheLocalKeySet(t *testing.T) {
	publicURL, _, stop := startServe(t, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080/topos", "TOPOS_LOCAL_ISSUER_KEY": localKey(t)})
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

func TestBasePath(t *testing.T) {
	for in, want := range map[string]string{"https://x.example": "", "https://x.example/": "", "https://x.example/topos/": "/topos", "%": ""} {
		if got := basePath(in); got != want {
			t.Errorf("basePath(%q) = %q, want %q", in, got, want)
		}
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

// hostedStubs are a stub Lux that answers with replies, or once with
// "Reviewed." when none is given, and a stub Cella that wants a bearer,
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
		replies = []luxstub.Reply{{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: "Reviewed."}}, StopReason: ir.StopEndTurn}}}
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
	send, id := createHostedSession(t, publicURL, vars)
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
	for _, want := range []string{`"type":"session.machine"`, `"kind":"cella"`, `"text":"Reviewed."`} {
		if !strings.Contains(events, want) {
			t.Errorf("the log lacks %s: %s", want, events)
		}
	}
	return events
}

// createHostedSession applies a Cella agent and creates a session of it
// over the API with the local issuer's token, and returns the function
// that sends as that token and the session's id.
func createHostedSession(t *testing.T, publicURL string, vars map[string]string) (func(method, path, body string) (int, string), string) {
	t.Helper()
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
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
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: cella}\n"
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
	if reqs := lux.Requests(); len(reqs) != 1 || reqs[0].Header.Get("X-Api-Key") != "model-key" && reqs[0].Header.Get("Authorization") != "Bearer model-key" {
		t.Errorf("the model was asked %d times, credential %v", len(reqs), reqs)
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
	send, id := createHostedSession(t, publicURL, vars)
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
