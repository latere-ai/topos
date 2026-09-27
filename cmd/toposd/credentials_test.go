// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/topos/authorizer"
	cellamachine "latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/idpstub"
	"latere.ai/x/topos/test/stubs/keystub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// credentialStubs are hostedStubs on an installation whose sessions act
// with their own credentials: a stub authorizer that allows everything,
// the stub identity provider, the stub session keys, and a git host, with
// neither installation credential.
func credentialStubs(t *testing.T) (map[string]string, stubsOf) {
	t.Helper()
	vars, lux, cella := hostedStubs(t)
	delete(vars, "TOPOS_MODELS_KEY")
	delete(vars, "TOPOS_CELLA_TOKEN_FILE")
	// Each call carries a token minted for it, which the checks below
	// read from the identity provider.
	cella.RequireToken("")
	az := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	idp := idpstub.New(t, "topos-host", "host-secret")
	keys := keystub.New(t, "keys-token")
	keys.AcceptAll()
	secret := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(secret, []byte("host-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	maps.Copy(vars, map[string]string{
		"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(),
		"TOPOS_AUTHORIZER_URL": az.URL(), "TOPOS_AUTHORIZER_TOKEN": az.Token(),
		"TOPOS_IDENTITY_URL": idp.URL(), "TOPOS_IDENTITY_CLIENT_ID": "topos-host", "TOPOS_IDENTITY_SECRET_FILE": secret,
		"TOPOS_SESSION_KEYS_URL": keys.URL(), "TOPOS_SESSION_KEYS_TOKEN": "keys-token",
		"TOPOS_ORIGO_URL": "https://origo.example",
	})
	return vars, stubsOf{az: az, idp: idp, keys: keys, lux: lux, cella: cella}
}

type stubsOf struct {
	az    *stub.Server
	idp   *idpstub.Server
	keys  *keystub.Server
	lux   *luxstub.Server
	cella *cellastub.Server
}

// waitAnswered waits until the session's turn answered.
func waitAnswered(t *testing.T, publicURL string, vars map[string]string, id string) {
	t.Helper()
	send := apiClient(t, publicURL, vars)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
		if strings.Contains(body, `"status":"idle"`) && strings.Contains(body, `"stop_reason":"end_turn"`) {
			return
		}
		if time.Now().After(deadline) {
			_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
			t.Fatalf("the session never answered: %s\nevents %s", body, events)
		}
	}
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// checkSessionCredentials holds a finished session id to spec 018's
// rows: the agent got its identity at the apply; the session's create
// named its id and that identity; every call the runner made to Cella
// carried the agent's token for the session and the workload session,
// none toposd's own; every model request carried the session's runner
// key, registered by hash; and the sandbox holds its own key and its git
// host's token as Secrets, each the session's credential for the
// workload sandbox.
func checkSessionCredentials(t *testing.T, s stubsOf, id string) {
	t.Helper()
	agents := s.idp.Agents()
	if len(agents) != 1 || agents[0].Owner != (idpstub.Owner{Type: "user", ID: "admin"}) {
		t.Fatalf("identities %+v", agents)
	}
	subject := agents[0].Subject
	created := false
	for _, r := range s.az.Requests() {
		if r.Action == authorizer.ActionSessionCreate {
			created = r.Resource.String("session_id") == id && r.Resource.String("agent_identity") == subject
		}
	}
	if !created {
		t.Error("session.create did not name the session's id and its agent's identity")
	}
	hostTokens := s.idp.HostTokens()
	reqs := s.cella.Requests()
	if len(reqs) == 0 {
		t.Fatal("no call reached Cella")
	}
	for _, r := range reqs {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		m, ok := s.idp.Token(tok)
		if slices.Contains(hostTokens, tok) || !ok || m.Subject != subject || m.Audience != "cella" || m.SessionID != id || m.Workload != "session" {
			t.Fatalf("%s %s carried %q: %+v", r.Method, r.Path, tok, m)
		}
	}
	runnerKey, ok := s.keys.Key(id, "session")
	if !ok {
		t.Fatal("the session has no runner key")
	}
	models := s.lux.Requests()
	if len(models) == 0 {
		t.Fatal("the model was not asked")
	}
	for _, r := range models {
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if hash(key) != runnerKey.Hash {
			t.Fatalf("a model request carried a key that is not the session's")
		}
	}
	name := cellamachine.SandboxName(id)
	_, luxValue, ok := s.cella.Secret(name + "-lux")
	sandboxKey, _ := s.keys.Key(id, "sandbox")
	if !ok || hash(luxValue) != sandboxKey.Hash || sandboxKey.Hash == runnerKey.Hash {
		t.Fatal("the sandbox's Secret does not hold the session's sandbox key")
	}
	_, origo, ok := s.cella.Secret(name + "-origo")
	if m, minted := s.idp.Token(origo); !ok || !minted || m.Audience != "origo" || m.Workload != "sandbox" || m.SessionID != id || m.Subject != subject {
		t.Fatalf("the sandbox's git host token %+v", m)
	}
}

// TestSessionCallsCarryTheAgentsSessionToken: on an installation with an
// identity provider and session keys, a hosted session created over the
// API runs with its own credentials, whether serve's own runner drives it
// or a runner role that reaches them over the token route; neither
// installation credential exists, and toposd's own token reaches no core.
func TestSessionCallsCarryTheAgentsSessionToken(t *testing.T) {
	t.Run("serve", func(t *testing.T) {
		vars, s := credentialStubs(t)
		vars["TOPOS_RUNNER_CAPACITY"] = "1"
		publicURL, _, stop := startServe(t, vars)
		_, id := createHostedSession(t, publicURL, vars, "cella")
		waitAnswered(t, publicURL, vars, id)
		checkSessionCredentials(t, s, id)
		if code := stop(); code != 0 {
			t.Fatalf("serve exited %d", code)
		}
	})
	t.Run("runner", func(t *testing.T) {
		vars, s := credentialStubs(t)
		maps.Copy(vars, map[string]string{"TOPOS_RUNNER_CAPACITY": "0", "TOPOS_RUNNER_TOKEN": "runner-token"})
		publicURL, internalURL, stop := startServe(t, vars)
		runnerVars := map[string]string{
			"TOPOS_INTERNAL_URL": internalURL, "TOPOS_INTERNAL_ADDR": "127.0.0.1:0", "TOPOS_RUNNER_CAPACITY": "1", "TOPOS_RUNNER_TOKEN": "runner-token",
			"TOPOS_MODELS_URL": vars["TOPOS_MODELS_URL"], "TOPOS_CELLA_URL": vars["TOPOS_CELLA_URL"], "TOPOS_ORIGO_URL": vars["TOPOS_ORIGO_URL"],
			"TOPOS_MACHINE_HELPERS": vars["TOPOS_MACHINE_HELPERS"], "TOPOS_MACHINE_DIR": vars["TOPOS_MACHINE_DIR"],
		}
		ctx, cancel := context.WithCancel(t.Context())
		var out, errOut syncBuffer
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"runner"}, env(runnerVars), &out, &errOut) }()
		_, id := createHostedSession(t, publicURL, vars, "cella")
		waitAnswered(t, publicURL, vars, id)
		checkSessionCredentials(t, s, id)
		cancel()
		if code := <-done; code != 0 {
			t.Fatalf("the runner role exited %d: %s", code, errOut.String())
		}
		if code := stop(); code != 0 {
			t.Fatalf("serve exited %d", code)
		}
	})
}
