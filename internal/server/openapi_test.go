// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
)

var update = flag.Bool("update", false, "rewrite api/openapi.yaml from the route table")

const committed = "../../api/openapi.yaml"

// TestOpenAPIIsGenerated holds api/openapi.yaml to the document the
// route and error tables render; go generate ./internal/server rewrites
// it.
func TestOpenAPIIsGenerated(t *testing.T) {
	want, err := OpenAPI(DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(committed, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("api/openapi.yaml differs from the route table; run go generate ./internal/server")
	}
}

// TestOpenAPIMatchesHandlers: every operation of the committed document
// is a route the server answers, with the actions its row asks, and
// every route is in the document; the served document names this
// installation's URL.
func TestOpenAPIMatchesHandlers(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string   `yaml:"operationId"`
			Actions     []string `yaml:"x-topos-actions"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	documented := map[string][]string{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			documented[strings.ToUpper(method)+" "+path] = op.Actions
		}
	}
	f := newFixture(t)
	for _, rt := range table() {
		key := rt.method + " " + rt.path
		actions, ok := documented[key]
		if !ok {
			t.Errorf("%s is served and not documented", key)
			continue
		}
		if !slices.Equal(actions, rt.actions) {
			t.Errorf("%s: the document names %v, the row %v", key, actions, rt.actions)
		}
		delete(documented, key)
		path := strings.NewReplacer("{name}", "x", "{ref}", "x", "{n}", "1", "{id}", "ses_x", "{digest}", "d", "{event_id}", "e").Replace(rt.path)
		if a := f.do(rt.method, "/v1"+path, "alice", ""); a.status == http.StatusMethodNotAllowed || strings.Contains(string(a.body), `"detail":"no route"`) {
			t.Errorf("%s is documented and not routed: %d %s", key, a.status, a.body)
		}
	}
	for key := range documented {
		t.Errorf("%s is documented and not served", key)
	}
	served := f.do(http.MethodGet, "/v1/openapi.yaml", "", "")
	if served.status != http.StatusOK || !strings.Contains(string(served.body), "url: https://topos.example/v1") || served.header.Get("Content-Type") != "application/yaml" {
		t.Fatalf("served: %d %s", served.status, served.header.Get("Content-Type"))
	}
}

// TestARouteAsksOnlyItsActions: a handler that asks a question its row
// does not name fails instead of asking it.
func TestARouteAsksOnlyItsActions(t *testing.T) {
	f := newFixture(t)
	c := &call{s: nil, r: httptest.NewRequest(http.MethodGet, "/", nil), rt: &route{method: http.MethodGet, path: "/x", actions: []string{authorizer.ActionAgentRead}}}
	if _, err := c.ask(t.Context(), authorizer.ActionAgentArchive, authz.Resource{}); err == nil {
		t.Fatal("asked an action the row does not name")
	}
	if _, err := c.askCreate(t.Context(), authorizer.ActionSessionCreate, authz.Resource{}); err == nil {
		t.Fatal("asked a create the row does not name")
	}
	if len(f.authz.take()) != 0 {
		t.Fatal("the authorizer was asked")
	}
}

// TestTheForkRouteStatesWhatItRestores: the fork route's description
// says that a hosted fork restores its files from the git host, the
// sentence a client reads before it promises them (spec 035).
func TestTheForkRouteStatesWhatItRestores(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Paths["/sessions/{id}/fork"]["post"].Description; !strings.Contains(got, ForkKeptFiles) {
		t.Fatalf("the fork route says %q", got)
	}
}
