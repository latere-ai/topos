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

// TestASummaryNamesItsAction: every operation of the committed document
// has a summary a reference can list it by, a few words that begin with
// a capital, end without a period and name no other operation, and a
// description that holds its sentences.
func TestASummaryNamesItsAction(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Summary     string `yaml:"summary"`
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	named := map[string]string{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			key := strings.ToUpper(method) + " " + path
			words := strings.Fields(op.Summary)
			if len(words) == 0 || len(words) > maxSummaryWords {
				t.Errorf("%s: the summary %q has %d words, want 1 to %d", key, op.Summary, len(words), maxSummaryWords)
				continue
			}
			if first := op.Summary[0]; first < 'A' || first > 'Z' || strings.HasSuffix(op.Summary, ".") {
				t.Errorf("%s: the summary %q is not an action's name", key, op.Summary)
			}
			if other, ok := named[op.Summary]; ok {
				t.Errorf("%s and %s share the summary %q", key, other, op.Summary)
			}
			named[op.Summary] = key
			if !strings.HasSuffix(op.Description, ".") {
				t.Errorf("%s: the description %q is not a sentence", key, op.Description)
			}
		}
	}
	if len(named) != len(table()) {
		t.Fatalf("%d operations named, the table has %d routes", len(named), len(table()))
	}
}

// TestEveryAnswerShowsAnExample: every operation of the committed
// document shows what its first success answer holds, an example of the
// body or, for bytes, a binary schema; an answer without a body shows
// no content.
func TestEveryAnswerShowsAnExample(t *testing.T) {
	ops := readDocument(t)
	if len(ops) != len(table()) {
		t.Fatalf("%d operations read, the table has %d routes", len(ops), len(table()))
	}
	for id, op := range ops {
		switch status, media, body := op.answers(t); {
		case status == http.StatusNoContent:
			if media != "" {
				t.Errorf("%s answers no content and shows %s", id, media)
			}
		case body.Example == nil && body.Schema.Format != "binary":
			t.Errorf("%s: its %d answer shows neither an example nor a binary schema", id, status)
		}
	}
}

// TestOpenAPIStatesTheOwner: the served document states the owner rule
// of spec 036 in the words a client reads it by, on the agent routes'
// name and on the PUT that creates an agent.
func TestOpenAPIStatesTheOwner(t *testing.T) {
	f := newFixture(t)
	served := f.do(http.MethodGet, "/v1/openapi.yaml", "", "")
	if served.status != http.StatusOK {
		t.Fatalf("the document: %d %s", served.status, served.body)
	}
	if !strings.Contains(string(served.body), OwnerRule) {
		t.Fatalf("the document does not state %q", OwnerRule)
	}
}
