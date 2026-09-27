// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/server"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// oneCaller authenticates every request as the same person.
type oneCaller struct{}

func (oneCaller) Authenticate(*http.Request) (auth.Caller, error) {
	return auth.Caller{Subject: authz.Subject("https://login.example", "alice"), Issuer: "https://login.example", Sub: "alice", Claims: map[string]any{"sub": "alice"}}, nil
}

// separator splits a YAML file into its documents.
var separator = regexp.MustCompile(`(?m)^---[ \t]*\n`)

// instructionsFile matches a document that names its instructions file,
// which the resolved spec no longer carries.
var instructionsFile = regexp.MustCompile(`(?m)^\s+instructionsFile:`)

// agentID matches an agent id a resolver minted, in a pinned reference.
var agentID = regexp.MustCompile(`agent_[0-9A-HJKMNP-TV-Z]{26}`)

// withNames writes each agent id of spec as the name it was minted for,
// so two resolutions whose stores minted different ids compare equal
// when they pin the same agents at the same versions.
func withNames(spec []byte, names map[string]string) string {
	return agentID.ReplaceAllStringFunc(string(spec), func(id string) string {
		if n, ok := names[id]; ok {
			return "<" + n + ">"
		}
		return id
	})
}

// TestCLIAndServerResolveAgree resolves every agent of manifest/testdata
// through the topos command's resolver and through PUT /v1/agents/{name},
// and holds the two to one canonical spec: byte for byte, with the same
// digest, for an agent that pins no other; and byte for byte up to the
// ids of the agents it pins, which each store mints, for one that does.
// The API refuses instructionsFile, whose file only a client can read,
// and answers unknown_reference for a memory store or a connection, which
// it has no route to hold yet.
func TestCLIAndServerResolveAgree(t *testing.T) {
	api, err := server.New(server.Options{
		Sessions: session.NewMemoryStore(), Objects: store.NewMemory(nil), Verifier: oneCaller{},
		Guard: auth.Guard{Authorizer: &auth.OwnerPolicy{}}, PublicURL: "https://topos.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	put := func(name string, body []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/v1/agents/"+name, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}

	files, err := filepath.Glob(filepath.Join("..", "..", "manifest", "testdata", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("manifest/testdata: %v", err)
	}
	env := &cli{Getenv: func(string) string { return "" }, stdout: &console{w: io.Discard}, stderr: &console{w: io.Discard}}
	agreed := 0
	for _, file := range files {
		abs, err := filepath.Abs(file)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(abs)
		if err != nil {
			t.Fatal(err)
		}
		docs := separator.Split(string(body), -1)
		rs, err := resolveFile(t.Context(), env, "-f", abs)
		if err != nil && strings.Contains(err.Error(), manifest.CodeUnknownReference) && strings.Contains(err.Error(), "spec.credential") {
			// A credential is an object only a store holds, and the
			// topos command has no stored objects yet.
			continue
		}
		if err != nil {
			t.Fatalf("%s through the topos command: %v", file, err)
		}
		cliNames, serverNames := map[string]string{}, map[string]string{}
		for _, r := range rs {
			if r.Agent == nil {
				continue
			}
			cliNames[r.Agent.Status.ID] = r.Name
			doc := []byte(docs[r.Doc])
			code, answer := put(r.Name, doc)
			switch {
			case instructionsFile.Match(doc):
				if code != http.StatusBadRequest || !strings.Contains(string(answer), manifest.CodeInvalidManifest) || !strings.Contains(string(answer), "instructionsFile") {
					t.Errorf("%s: %s with instructionsFile: %d %s", file, r.Name, code, answer)
				}
				continue
			case len(r.Agent.Spec.MemoryStores) > 0 || len(r.Agent.Spec.Connections) > 0:
				if !strings.Contains(string(answer), manifest.CodeUnknownReference) {
					t.Errorf("%s: %s referencing a memory store or a connection: %d %s", file, r.Name, code, answer)
				}
				continue
			}
			if code != http.StatusCreated {
				t.Fatalf("%s: apply %s: %d %s", file, r.Name, code, answer)
			}
			var applied v1.Agent
			if err := json.Unmarshal(answer, &applied); err != nil {
				t.Fatal(err)
			}
			serverNames[applied.Status.ID] = r.Name
			spec, err := session.Marshal(applied.Spec)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Pinned) == 0 {
				if !bytes.Equal(spec, r.Spec) || applied.Status.Digest != r.Digest {
					t.Errorf("%s: %s resolves apart:\ntopos %s %s\nAPI   %s %s", file, r.Name, r.Digest, r.Spec, applied.Status.Digest, spec)
				}
			} else if withNames(spec, serverNames) != withNames(r.Spec, cliNames) {
				t.Errorf("%s: %s pins apart:\ntopos %s\nAPI   %s", file, r.Name, r.Spec, spec)
			}
			agreed++
		}
	}
	if agreed < 3 {
		t.Fatalf("only %d agents resolved through both; the fixtures lost the agents the two paths share", agreed)
	}
}
